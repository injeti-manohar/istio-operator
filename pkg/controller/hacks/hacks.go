package hacks

import (
	"context"
	"fmt"
	"strings"
	"time"

	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"

	"github.com/maistra/istio-operator/pkg/controller/common"
)

// ReduceLikelihoodOfRepeatedReconciliation simply performs a 2 second delay. Call this function after you post an
// update to a resource if you want to reduce the likelihood of the reconcile() function being called again before
// the update comes back into the operator (until it does, any invocation of reconcile() will perform reconciliation on
// a stale version of the resource). Calling this function prevents the next reconcile() from being invoked immediately,
// allowing the watch event more time to come back and update the cache.
//
// For the complete explanation, see https://issues.jboss.org/projects/MAISTRA/issues/MAISTRA-830
func ReduceLikelihoodOfRepeatedReconciliation(ctx context.Context) {
	log := common.LogFromContext(ctx)
	log.Info("Waiting 2 seconds to give the cache a chance to sync after updating resource")
	time.Sleep(2 * time.Second)
}

// RemoveTypeObjectFieldsFromCRDSchema works around the problem where OpenShift 3.11 doesn't like "type: object"
// in CRD OpenAPI schemas. This function removes all occurrences from the schema.
func RemoveTypeObjectFieldsFromCRDSchema(ctx context.Context, crd *apiextensionsv1beta1.CustomResourceDefinition) error {
	log := common.LogFromContext(ctx)
	log.Info("The API server rejected the CRD. Removing type:object fields from the CRD schema and trying again.")

	if crd.Spec.Validation == nil || crd.Spec.Validation.OpenAPIV3Schema == nil {
		return fmt.Errorf("Could not remove type:object fields from CRD schema as no spec.validation.openAPIV3Schema exists")
	}
	removeTypeObjectField(crd.Spec.Validation.OpenAPIV3Schema)
	return nil
}

// IsTypeObjectProblemInCRDSchemas returns true if the error provided is the error usually
// returned by the API server when it doesn't like "type:object" fields in the CRD's OpenAPI Schema.
func IsTypeObjectProblemInCRDSchemas(err error) bool {
	return err != nil && strings.Contains(err.Error(), "must only have \"properties\", \"required\" or \"description\" at the root if the status subresource is enabled")
}

func removeTypeObjectField(schema *apiextensionsv1beta1.JSONSchemaProps) {
	if schema == nil {
		return
	}

	if schema.Type == "object" {
		schema.Type = ""
	}

	removeTypeObjectFieldFromArray(schema.OneOf)
	removeTypeObjectFieldFromArray(schema.AnyOf)
	removeTypeObjectFieldFromArray(schema.AllOf)
	removeTypeObjectFieldFromMap(schema.Properties)
	removeTypeObjectFieldFromMap(schema.PatternProperties)
	removeTypeObjectFieldFromMap(schema.Definitions)
	removeTypeObjectField(schema.Not)

	if schema.Items != nil {
		removeTypeObjectField(schema.Items.Schema)
		removeTypeObjectFieldFromArray(schema.Items.JSONSchemas)
	}
	if schema.AdditionalProperties != nil {
		removeTypeObjectField(schema.AdditionalProperties.Schema)
	}
	if schema.AdditionalItems != nil {
		removeTypeObjectField(schema.AdditionalItems.Schema)
	}
	for k, v := range schema.Dependencies {
		removeTypeObjectField(v.Schema)
		schema.Dependencies[k] = v
	}
}

func removeTypeObjectFieldFromArray(array []apiextensionsv1beta1.JSONSchemaProps) {
	for i, child := range array {
		removeTypeObjectField(&child)
		array[i] = child
	}
}

func removeTypeObjectFieldFromMap(m map[string]apiextensionsv1beta1.JSONSchemaProps) {
	for k, v := range m {
		removeTypeObjectField(&v)
		m[k] = v
	}
}
