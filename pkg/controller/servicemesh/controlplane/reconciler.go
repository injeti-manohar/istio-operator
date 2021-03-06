package controlplane

import (
	"context"
	"fmt"
	"io/ioutil"
	"path"
	"reflect"
	"strings"

	"github.com/ghodss/yaml"
	imagev1 "github.com/openshift/api/image/v1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/helm/pkg/manifest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/maistra/istio-operator/pkg/apis/maistra/status"
	v1 "github.com/maistra/istio-operator/pkg/apis/maistra/v1"
	v2 "github.com/maistra/istio-operator/pkg/apis/maistra/v2"
	"github.com/maistra/istio-operator/pkg/bootstrap"
	"github.com/maistra/istio-operator/pkg/controller/common"
	"github.com/maistra/istio-operator/pkg/controller/common/cni"
	"github.com/maistra/istio-operator/pkg/controller/common/helm"
	"github.com/maistra/istio-operator/pkg/controller/hacks"
	"github.com/maistra/istio-operator/pkg/controller/versions"
)

type controlPlaneInstanceReconciler struct {
	common.ControllerResources
	Instance          *v2.ServiceMeshControlPlane
	Status            *v2.ControlPlaneStatus
	ownerRefs         []metav1.OwnerReference
	meshGeneration    string
	renderings        map[string][]manifest.Manifest
	waitForComponents sets.String
	cniConfig         cni.Config
}

// ensure controlPlaneInstanceReconciler implements ControlPlaneInstanceReconciler
var _ ControlPlaneInstanceReconciler = &controlPlaneInstanceReconciler{}

// these components have to be installed in the specified order
var orderedCharts = [][]string{
	{"istio"}, // core istio resources
	{"istio/charts/security"},
	{"istio/charts/prometheus"},
	{"istio/charts/tracing"},
	{"istio/charts/galley"},
	{"istio/charts/mixer", "istio/charts/pilot", "istio/charts/gateways", "istio/charts/sidecarInjectorWebhook"},
	{"istio/charts/grafana"},
	{"istio/charts/kiali"},
}

const (
	// Event reasons
	eventReasonInstalling              = "Installing"
	eventReasonPausingInstall          = "PausingInstall"
	eventReasonPausingUpdate           = "PausingUpdate"
	eventReasonInstalled               = "Installed"
	eventReasonUpdating                = "Updating"
	eventReasonUpdated                 = "Updated"
	eventReasonDeleting                = "Deleting"
	eventReasonDeleted                 = "Deleted"
	eventReasonPruning                 = "Pruning"
	eventReasonFailedRemovingFinalizer = "FailedRemovingFinalizer"
	eventReasonFailedDeletingResources = "FailedDeletingResources"
	eventReasonNotReady                = "NotReady"
	eventReasonReady                   = "Ready"
)

func NewControlPlaneInstanceReconciler(controllerResources common.ControllerResources, newInstance *v2.ServiceMeshControlPlane, cniConfig cni.Config) ControlPlaneInstanceReconciler {
	return &controlPlaneInstanceReconciler{
		ControllerResources: controllerResources,
		Instance:            newInstance,
		Status:              newInstance.Status.DeepCopy(),
		cniConfig:           cniConfig,
	}
}

func (r *controlPlaneInstanceReconciler) Reconcile(ctx context.Context) (result reconcile.Result, err error) {
	log := common.LogFromContext(ctx)
	log.Info("Reconciling ServiceMeshControlPlane", "Status", r.Instance.Status.StatusType)
	if r.Status.GetCondition(status.ConditionTypeReconciled).Status != status.ConditionStatusFalse {
		r.initializeReconcileStatus()
		err := r.PostStatus(ctx)
		return reconcile.Result{}, err // ensure that the new reconcile status is posted immediately. Reconciliation will resume when the status update comes back into the operator
	}

	// make sure status gets updated on exit
	reconciledCondition := r.Status.GetCondition(status.ConditionTypeReconciled)
	reconciliationMessage := reconciledCondition.Message
	reconciliationReason := reconciledCondition.Reason
	reconciliationComplete := false
	defer func() {
		// this ensures we're updating status (if necessary) and recording events on exit
		if statusErr := r.postReconciliationStatus(ctx, reconciliationReason, reconciliationMessage, err); statusErr != nil {
			if err == nil {
				err = statusErr
			} else {
				log.Error(statusErr, "Error posting reconciliation status")
			}
		}
		if reconciliationComplete {
			hacks.ReduceLikelihoodOfRepeatedReconciliation(ctx)
		}
	}()

	if r.renderings == nil {
		// error handling
		defer func() {
			if err != nil {
				r.waitForComponents = sets.NewString()
				updateControlPlaneConditions(r.Status, err)
			}
		}()

		var version versions.Version
		version, err = versions.ParseVersion(r.Instance.Spec.Version)
		if err != nil {
			log.Error(err, "invalid version specified")
			return
		}

		// Render the templates
		err = r.renderCharts(ctx, version)
		if err != nil {
			// we can't progress here
			reconciliationReason = status.ConditionReasonReconcileError
			reconciliationMessage = "Error rendering helm charts"
			err = errors.Wrap(err, reconciliationMessage)
			return
		}

		// install istio

		// set the auto-injection flag
		// update injection label on namespace
		// XXX: this should probably only be done when installing a control plane
		// e.g. spec.pilot.enabled || spec.mixer.enabled || spec.galley.enabled || spec.sidecarInjectorWebhook.enabled || ....
		// which is all we're supporting atm.  if the scope expands to allow
		// installing custom gateways, etc., we should revisit this.
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.Instance.Namespace}}
		err = r.Client.Get(ctx, client.ObjectKey{Name: r.Instance.Namespace}, namespace)
		if err == nil {
			updateLabels := false
			if namespace.Labels == nil {
				namespace.Labels = map[string]string{}
			}
			// make sure injection is disabled for the control plane
			if label, ok := namespace.Labels["maistra.io/ignore-namespace"]; !ok || label != "ignore" {
				log.Info("Adding maistra.io/ignore-namespace=ignore label to Request.Namespace")
				namespace.Labels["maistra.io/ignore-namespace"] = "ignore"
				updateLabels = true
			}
			// make sure the member-of label is specified, so networking works correctly
			if label, ok := namespace.Labels[common.MemberOfKey]; !ok || label != namespace.GetName() {
				log.Info(fmt.Sprintf("Adding %s label to Request.Namespace", common.MemberOfKey))
				namespace.Labels[common.MemberOfKey] = namespace.GetName()
				updateLabels = true
			}
			if updateLabels {
				err = r.Client.Update(ctx, namespace)
			}
		}
		if err != nil {
			// bail if there was an error updating the namespace
			reconciliationReason = status.ConditionReasonReconcileError
			reconciliationMessage = "Error updating labels on mesh namespace"
			err = errors.Wrap(err, reconciliationMessage)
			return
		}

		// initialize new Status
		componentStatuses := make([]status.ComponentStatus, 0, len(r.Status.ComponentStatus))
		for _, charts := range r.getChartsInInstallationOrder() {
			for _, chartName := range charts {
				componentName := componentFromChartName(chartName)
				componentStatus := r.Status.FindComponentByName(componentName)
				if componentStatus == nil {
					componentStatus = status.NewComponentStatus()
					componentStatus.Resource = componentName
				}
				componentStatus.SetCondition(status.Condition{
					Type:   status.ConditionTypeReconciled,
					Status: status.ConditionStatusFalse,
				})
				componentStatuses = append(componentStatuses, *componentStatus)
			}
		}
		r.Status.ComponentStatus = componentStatuses

		// initialize common data
		owner := metav1.NewControllerRef(r.Instance, v2.SchemeGroupVersion.WithKind("ServiceMeshControlPlane"))
		r.ownerRefs = []metav1.OwnerReference{*owner}
		r.meshGeneration = status.CurrentReconciledVersion(r.Instance.GetGeneration())

		// Ensure CRDs are installed
		chartsDir := helm.GetChartsDir(version)
		if err = bootstrap.InstallCRDs(common.NewContextWithLog(ctx, log.WithValues("version", r.Instance.Spec.Version)), r.Client, chartsDir); err != nil {
			reconciliationReason = status.ConditionReasonReconcileError
			reconciliationMessage = "Failed to install/update Istio CRDs"
			log.Error(err, reconciliationMessage)
			return
		}

		// Ensure Istio CNI is installed
		if r.cniConfig.Enabled {
			r.waitForComponents = sets.NewString("cni")
			if err = bootstrap.InstallCNI(ctx, r.Client, r.cniConfig); err != nil {
				reconciliationReason = status.ConditionReasonReconcileError
				reconciliationMessage = "Failed to install/update Istio CNI"
				log.Error(err, reconciliationMessage)
				return
			} else if ready, _ := r.isCNIReady(ctx); !ready {
				reconciliationReason = status.ConditionReasonPausingInstall
				reconciliationMessage = fmt.Sprintf("Paused until %s becomes ready", "cni")
				return
			}
		}

		if err = r.reconcileRBAC(ctx); err != nil {
			reconciliationReason = status.ConditionReasonReconcileError
			reconciliationMessage = "Failed to install/update Maistra RBAC resources"
			log.Error(err, reconciliationMessage)
			return
		}

	} else if r.waitForComponents.Len() > 0 {
		// if we've already begun reconciling, make sure we weren't waiting for
		// the last component to become ready
		readyComponents, _, readinessErr := r.calculateComponentReadiness(ctx)
		if readinessErr != nil {
			// error calculating readiness
			reconciliationReason = status.ConditionReasonProbeError
			reconciliationMessage = "Error checking component readiness"
			err = errors.Wrap(readinessErr, reconciliationMessage)
			log.Error(err, reconciliationMessage)
			return
		}

		r.waitForComponents.Delete(readyComponents.List()...)
		if r.waitForComponents.Len() > 0 {
			reconciliationReason, reconciliationMessage, err = r.pauseReconciliation(ctx)
			return
		}
	}

	// create components
	for _, charts := range r.getChartsInInstallationOrder() {
		r.waitForComponents = sets.NewString()
		for _, chart := range charts {
			component := componentFromChartName(chart)
			var hasReadiness bool
			hasReadiness, err = r.processComponentManifests(ctx, chart)
			if err != nil {
				reconciliationReason = status.ConditionReasonReconcileError
				reconciliationMessage = fmt.Sprintf("Error processing component %s: %v", component, err)
				return
			}
			if hasReadiness {
				r.waitForComponents.Insert(component)
			}
		}

		if r.waitForComponents.Len() > 0 {
			readyComponents, _, readyErr := r.calculateComponentReadiness(ctx)
			if readyErr != nil {
				reconciliationReason, reconciliationMessage, err = r.pauseReconciliation(ctx)
				return
			}

			r.waitForComponents.Delete(readyComponents.List()...)
			if r.waitForComponents.Len() > 0 {
				reconciliationReason, reconciliationMessage, err = r.pauseReconciliation(ctx)
				return
			}
		}
	}

	// we still need to prune if this is the first generation, e.g. if the operator was updated during the install,
	// it's possible that some resources in the original version may not be present in the new version.
	// delete unseen components
	reconciliationMessage = "Pruning obsolete resources"
	r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReasonPruning, reconciliationMessage)
	log.Info(reconciliationMessage)
	err = r.prune(ctx, r.meshGeneration)
	if err != nil {
		reconciliationReason = status.ConditionReasonReconcileError
		reconciliationMessage = "Error pruning obsolete resources"
		err = errors.Wrap(err, reconciliationMessage)
		return
	}

	if r.isUpdating() {
		reconciliationReason = status.ConditionReasonUpdateSuccessful
		reconciliationMessage = fmt.Sprintf("Successfully updated from version %s to version %s", r.Status.GetReconciledVersion(), r.meshGeneration)
		r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReasonUpdated, reconciliationMessage)
	} else {
		reconciliationReason = status.ConditionReasonInstallSuccessful
		reconciliationMessage = fmt.Sprintf("Successfully installed version %s", r.meshGeneration)
		r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReasonInstalled, reconciliationMessage)
	}
	r.Status.ObservedGeneration = r.Instance.GetGeneration()
	r.Status.ReconciledVersion = r.meshGeneration
	updateControlPlaneConditions(r.Status, nil)

	reconciliationComplete = true
	log.Info("Completed ServiceMeshControlPlane reconcilation")
	return
}

func (r *controlPlaneInstanceReconciler) pauseReconciliation(ctx context.Context) (status.ConditionReason, string, error) {
	log := common.LogFromContext(ctx)
	var eventReason string
	var conditionReason status.ConditionReason
	if r.isUpdating() {
		eventReason = eventReasonPausingUpdate
		conditionReason = status.ConditionReasonPausingUpdate
	} else {
		eventReason = eventReasonPausingInstall
		conditionReason = status.ConditionReasonPausingInstall
	}
	reconciliationMessage := fmt.Sprintf("Paused until the following components become ready: %v", r.waitForComponents.List())
	r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReason, reconciliationMessage)
	log.Info(reconciliationMessage)
	return conditionReason, reconciliationMessage, nil
}

func (r *controlPlaneInstanceReconciler) isUpdating() bool {
	return r.Instance.Status.ObservedGeneration != 0
}

// mergeValues merges a map containing input values on top of a map containing
// base values, giving preference to the base values for conflicts
func mergeValues(base map[string]interface{}, input map[string]interface{}) map[string]interface{} {
	if base == nil {
		base = make(map[string]interface{}, 1)
	}

	for key, value := range input {
		// if the key doesn't already exist, add it
		if _, exists := base[key]; !exists {
			base[key] = value
			continue
		}

		// at this point, key exists in both input and base.
		// If both are maps, recurse.
		// If only input is a map, ignore it. We don't want to overrwrite base.
		// If both are values, again, ignore it since we don't want to overrwrite base.
		if baseKeyAsMap, baseOK := base[key].(map[string]interface{}); baseOK {
			if inputAsMap, inputOK := value.(map[string]interface{}); inputOK {
				base[key] = mergeValues(baseKeyAsMap, inputAsMap)
			}
		}
	}
	return base
}

func (r *controlPlaneInstanceReconciler) getSMCPTemplate(name string, maistraVersion versions.Version) (v1.ControlPlaneSpec, error) {
	if strings.Contains(name, "/") {
		return v1.ControlPlaneSpec{}, fmt.Errorf("template name contains invalid character '/'")
	}

	templateContent, err := ioutil.ReadFile(path.Join(helm.GetUserTemplatesDir(), name))
	if err != nil {
		//if we can't read from the user template path, try from the default path
		//we use two paths because Kubernetes will not auto-update volume mounted
		//configmaps mounted in directories with pre-existing content
		defaultTemplateContent, defaultErr := ioutil.ReadFile(path.Join(helm.GetDefaultTemplatesDir(maistraVersion), name))
		if defaultErr != nil {
			return v1.ControlPlaneSpec{}, fmt.Errorf("template cannot be loaded from user or default directory. Error from user: %s. Error from default: %s", err, defaultErr)
		}
		templateContent = defaultTemplateContent
	}

	var template v1.ServiceMeshControlPlane
	if err = yaml.Unmarshal(templateContent, &template); err != nil {
		return v1.ControlPlaneSpec{}, fmt.Errorf("failed to parse template %s contents: %s", name, err)
	}
	return template.Spec, nil
}

//renderSMCPTemplates traverses and processes all of the references templates
func (r *controlPlaneInstanceReconciler) recursivelyApplyTemplates(ctx context.Context, smcp v1.ControlPlaneSpec, version versions.Version, visited sets.String) (v1.ControlPlaneSpec, error) {
	log := common.LogFromContext(ctx)
	if smcp.Template == "" {
		return smcp, nil
	}
	log.Info(fmt.Sprintf("processing smcp template %s", smcp.Template))

	if visited.Has(smcp.Template) {
		return smcp, fmt.Errorf("SMCP templates form cyclic dependency. Cannot proceed")
	}

	template, err := r.getSMCPTemplate(smcp.Template, version)
	if err != nil {
		return smcp, err
	}

	template, err = r.recursivelyApplyTemplates(ctx, template, version, visited)
	if err != nil {
		log.Info(fmt.Sprintf("error rendering SMCP templates: %s\n", err))
		return smcp, err
	}

	visited.Insert(smcp.Template)

	smcp.Istio = v1.NewHelmValues(mergeValues(smcp.Istio.GetContent(), template.Istio.GetContent()))
	smcp.ThreeScale = v1.NewHelmValues(mergeValues(smcp.ThreeScale.GetContent(), template.ThreeScale.GetContent()))
	return smcp, nil
}

func (r *controlPlaneInstanceReconciler) applyDisconnectedSettings(ctx context.Context, smcpSpec v1.ControlPlaneSpec, version versions.Version) (v1.ControlPlaneSpec, error) {
	log := common.LogFromContext(ctx)
	log.Info("updating image names for disconnected install")

	var err error
	if err = version.Strategy().SetImageValues(ctx, r.Client, &smcpSpec); err != nil {
		return smcpSpec, err
	}
	err = r.updateOauthProxyConfig(ctx, &smcpSpec)
	return smcpSpec, err
}

func (r *controlPlaneInstanceReconciler) updateOauthProxyConfig(ctx context.Context, smcpSpec *v1.ControlPlaneSpec) error {
	if !common.Config.OAuthProxy.Query || len(common.Config.OAuthProxy.Name) == 0 || len(common.Config.OAuthProxy.Namespace) == 0 {
		return nil
	}
	log := common.LogFromContext(ctx)
	is := &imagev1.ImageStream{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: common.Config.OAuthProxy.Namespace, Name: common.Config.OAuthProxy.Name}, is); err == nil {
		foundTag := false
		for _, tag := range is.Status.Tags {
			if tag.Tag == common.Config.OAuthProxy.Tag {
				foundTag = true
				if len(tag.Items) > 0 && len(tag.Items[0].DockerImageReference) > 0 {
					common.Config.OAuthProxy.Image = tag.Items[0].DockerImageReference
				} else {
					log.Info(fmt.Sprintf("warning: dockerImageReference not set for tag '%s' in ImageStream %s/%s", common.Config.OAuthProxy.Tag, common.Config.OAuthProxy.Namespace, common.Config.OAuthProxy.Name))
				}
				break
			}
		}
		if !foundTag {
			log.Info(fmt.Sprintf("warning: could not find tag '%s' in ImageStream %s/%s", common.Config.OAuthProxy.Tag, common.Config.OAuthProxy.Namespace, common.Config.OAuthProxy.Name))
		}
	} else if !(apierrors.IsNotFound(err) || apierrors.IsGone(err)) {
		log.Error(err, fmt.Sprintf("unexpected error retrieving ImageStream %s/%s", common.Config.OAuthProxy.Namespace, common.Config.OAuthProxy.Name))
	}
	if len(common.Config.OAuthProxy.Image) == 0 {
		log.Info("global.oauthproxy.image will not be overridden")
		return nil
	}
	log.Info(fmt.Sprintf("using '%s' for global.oauthproxy.image", common.Config.OAuthProxy.Image))
	r.updateImageField(smcpSpec.Istio, "global.oauthproxy.image", common.Config.OAuthProxy.Image)
	return nil
}

func (r *controlPlaneInstanceReconciler) updateImageField(helmValues *v1.HelmValues, path, value string) error {
	if len(value) == 0 {
		return nil
	}
	return helmValues.SetField(path, value)
}

func (r *controlPlaneInstanceReconciler) applyTemplates(ctx context.Context, smcpSpec v1.ControlPlaneSpec, version versions.Version) (v1.ControlPlaneSpec, error) {
	log := common.LogFromContext(ctx)
	log.Info("updating servicemeshcontrolplane with templates")
	if smcpSpec.Template == "" {
		smcpSpec.Template = v1.DefaultTemplate
		log.Info("No template provided. Using default")
	}

	applyDisconnectedSettings := true
	if tag, _, _ := smcpSpec.Istio.GetString("global.tag"); tag != "" {
		// don't update anything
		applyDisconnectedSettings = false
	} else if hub, _, _ := smcpSpec.Istio.GetString("global.hub"); hub != "" {
		// don't update anything
		applyDisconnectedSettings = false
	}

	spec, err := r.recursivelyApplyTemplates(ctx, smcpSpec, version, sets.NewString())

	if applyDisconnectedSettings {
		spec, err = r.applyDisconnectedSettings(ctx, spec, version)
		if err != nil {
			log.Error(err, "warning: failed to apply image names to support disconnected install")

			return spec, err
		}
	}

	log.Info("finished updating ServiceMeshControlPlane", "Spec", spec)

	return spec, err
}

func (r *controlPlaneInstanceReconciler) validateSMCPSpec(spec v1.ControlPlaneSpec, basePath string) error {
	if spec.Istio == nil {
		return fmt.Errorf("ServiceMeshControlPlane missing %s.istio section", basePath)
	}

	if _, ok, _ := spec.Istio.GetMap("global"); !ok {
		return fmt.Errorf("ServiceMeshControlPlane missing %s.istio.global section", basePath)
	}
	return nil
}

func (r *controlPlaneInstanceReconciler) renderCharts(ctx context.Context, version versions.Version) error {
	log := common.LogFromContext(ctx)
	//Generate the spec
	v1spec := v1.ControlPlaneSpec{}
	if err := r.Scheme.Convert(&r.Instance.Spec, &v1spec, nil); err != nil {
		return err
	}
	r.Status.LastAppliedConfiguration = v1spec
	r.Status.LastAppliedConfiguration.Version = version.String()

	spec, err := r.applyTemplates(ctx, r.Status.LastAppliedConfiguration, version)
	if err != nil {
		log.Error(err, "warning: failed to apply ServiceMeshControlPlane templates")

		return err
	}

	r.Status.LastAppliedConfiguration = spec

	if err := r.validateSMCPSpec(r.Status.LastAppliedConfiguration, "status.lastAppliedConfiguration"); err != nil {
		return err
	}

	err = r.Status.LastAppliedConfiguration.Istio.SetField("global.operatorNamespace", r.OperatorNamespace)
	if err != nil {
		return err
	}

	err = r.Status.LastAppliedConfiguration.Istio.SetField("istio_cni.enabled", r.cniConfig.Enabled)
	if err != nil {
		return fmt.Errorf("Could not set field status.lastAppliedConfiguration.istio.istio_cni.enabled: %v", err)
	}

	cniNetworkName, ok := cni.GetNetworkName(version)
	if !ok {
		return fmt.Errorf("unknown maistra version: %s", version)
	}
	err = r.Status.LastAppliedConfiguration.Istio.SetField("istio_cni.istio_cni_network", cniNetworkName)
	if err != nil {
		return fmt.Errorf("Could not set field status.lastAppliedConfiguration.istio.istio_cni.istio_cni_network: %v", err)
	}

	//Render the charts
	allErrors := []error{}
	var threeScaleRenderings map[string][]manifest.Manifest
	log.Info("rendering helm charts")
	log.V(2).Info("rendering Istio charts")
	istioRenderings, _, err := helm.RenderChart(path.Join(helm.GetChartsDir(version), "istio"), r.Instance.GetNamespace(), r.Status.LastAppliedConfiguration.Istio.GetContent())
	if err != nil {
		allErrors = append(allErrors, err)
	}
	if isEnabled(r.Status.LastAppliedConfiguration.ThreeScale) {
		log.V(2).Info("rendering 3scale charts")
		threeScaleRenderings, _, err = helm.RenderChart(path.Join(helm.GetChartsDir(version), "maistra-threescale"), r.Instance.GetNamespace(), r.Status.LastAppliedConfiguration.ThreeScale.GetContent())
		if err != nil {
			allErrors = append(allErrors, err)
		}
	} else {
		threeScaleRenderings = map[string][]manifest.Manifest{}
	}

	if len(allErrors) > 0 {
		return utilerrors.NewAggregate(allErrors)
	}

	// merge the rendernings
	r.renderings = map[string][]manifest.Manifest{}
	for key, value := range istioRenderings {
		r.renderings[key] = value
	}
	for key, value := range threeScaleRenderings {
		r.renderings[key] = value
	}
	return nil
}

func (r *controlPlaneInstanceReconciler) PostStatus(ctx context.Context) error {
	// we should only post status if it has changed
	if reflect.DeepEqual(r.Status, &r.Instance.Status) {
		return nil
	}
	log := common.LogFromContext(ctx)
	instance := &v2.ServiceMeshControlPlane{}
	log.Info("Posting status update", "conditions", r.Status.Conditions)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: r.Instance.Name, Namespace: r.Instance.Namespace}, instance); err == nil {
		instance.Status = *r.Status.DeepCopy()
		if err = r.Client.Status().Patch(ctx, instance, common.NewStatusPatch(instance.Status)); err != nil && !(apierrors.IsGone(err) || apierrors.IsNotFound(err)) {
			return errors.Wrap(err, "error updating ServiceMeshControlPlane status")
		}
	} else if !(apierrors.IsGone(err) || apierrors.IsNotFound(err)) {
		return errors.Wrap(err, "error getting ServiceMeshControlPlane prior to updating status")
	}

	return nil
}

func (r *controlPlaneInstanceReconciler) postReconciliationStatus(ctx context.Context, reconciliationReason status.ConditionReason, reconciliationMessage string, processingErr error) error {
	_, err := r.updateReadinessStatus(ctx)
	if err != nil {
		return err
	}

	var reason string
	if r.isUpdating() {
		reason = eventReasonUpdating
	} else {
		reason = eventReasonInstalling
	}
	reconciledCondition := r.Status.GetCondition(status.ConditionTypeReconciled)
	reconciledCondition.Reason = reconciliationReason
	if processingErr == nil {
		reconciledCondition.Message = reconciliationMessage
	} else {
		// grab the cause, as it's likely the error includes the reconciliation message
		reconciledCondition.Message = fmt.Sprintf("%s: error: %s", reconciliationMessage, errors.Cause(processingErr))
		r.EventRecorder.Event(r.Instance, corev1.EventTypeWarning, reason, reconciledCondition.Message)
	}
	r.Status.SetCondition(reconciledCondition)

	return r.PostStatus(ctx)
}

func (r *controlPlaneInstanceReconciler) initializeReconcileStatus() {
	var readyMessage string
	var eventReason string
	var conditionReason status.ConditionReason
	if r.isUpdating() {
		if r.Status.ObservedGeneration == r.Instance.GetGeneration() {
			fromVersion := r.Status.GetReconciledVersion()
			toVersion := status.CurrentReconciledVersion(r.Instance.GetGeneration())
			readyMessage = fmt.Sprintf("Upgrading mesh from version %s to version %s", fromVersion[strings.LastIndex(fromVersion, "-")+1:], toVersion[strings.LastIndex(toVersion, "-")+1:])
		} else {
			readyMessage = fmt.Sprintf("Updating mesh from generation %d to generation %d", r.Status.ObservedGeneration, r.Instance.GetGeneration())
		}
		eventReason = eventReasonUpdating
		conditionReason = status.ConditionReasonSpecUpdated
	} else {
		readyMessage = fmt.Sprintf("Installing mesh generation %d", r.Instance.GetGeneration())
		eventReason = eventReasonInstalling
		conditionReason = status.ConditionReasonResourceCreated

		r.Status.SetCondition(status.Condition{
			Type:    status.ConditionTypeInstalled,
			Status:  status.ConditionStatusFalse,
			Reason:  conditionReason,
			Message: readyMessage,
		})
	}
	r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReason, readyMessage)
	r.Status.SetCondition(status.Condition{
		Type:    status.ConditionTypeReconciled,
		Status:  status.ConditionStatusFalse,
		Reason:  conditionReason,
		Message: readyMessage,
	})
	r.Status.SetCondition(status.Condition{
		Type:    status.ConditionTypeReady,
		Status:  status.ConditionStatusFalse,
		Reason:  conditionReason,
		Message: readyMessage,
	})
}

func (r *controlPlaneInstanceReconciler) SetInstance(newInstance *v2.ServiceMeshControlPlane) {
	if newInstance.GetGeneration() != r.Instance.GetGeneration() {
		// we need to regenerate the renderings
		r.renderings = nil
		r.waitForComponents = sets.NewString()
		// reset reconcile status
		r.Status.SetCondition(status.Condition{Type: status.ConditionTypeReconciled, Status: status.ConditionStatusUnknown})
	}
	r.Instance = newInstance
}

func (r *controlPlaneInstanceReconciler) IsFinished() bool {
	return r.Status.GetCondition(status.ConditionTypeReconciled).Status == status.ConditionStatusTrue
}

// returns the keys from r.renderings in the order they need to be installed in:
// - keys in orderedCharts
// - other istio components that have the "istio/" prefix
// - 3scale and other components
func (r *controlPlaneInstanceReconciler) getChartsInInstallationOrder() [][]string {
	charts := make([][]string, 0, len(r.renderings))
	seen := sets.NewString()

	// first install the charts listed in orderedCharts (but only if they appear in r.renderings)
	for _, chartSet := range orderedCharts {
		chartsToDeploy := make([]string, 0, len(chartSet))
		for _, chart := range chartSet {
			if _, found := r.renderings[chart]; found {
				chartsToDeploy = append(chartsToDeploy, chart)
				seen.Insert(chart)
			}
		}
		if len(chartsToDeploy) > 0 {
			charts = append(charts, chartsToDeploy)
		}
	}

	// install other istio components that aren't listed in orderedCharts
	for chart := range r.renderings {
		if strings.HasPrefix(chart, "istio/") && !seen.Has(chart) {
			charts = append(charts, []string{chart})
			seen.Insert(chart)
		}
	}

	// install 3scale and any other components
	for chart := range r.renderings {
		if !seen.Has(chart) {
			charts = append(charts, []string{chart})
		}
	}
	return charts
}

func componentFromChartName(chartName string) string {
	_, componentName := path.Split(chartName)
	return componentName
}

func isEnabled(spec *v1.HelmValues) bool {
	if enabled, found, _ := spec.GetBool("enabled"); found {
		return enabled
	}
	return false
}
