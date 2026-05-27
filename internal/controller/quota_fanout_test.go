// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// namespaceScope implements meta.RESTScope for namespace-scoped resources.
type namespaceScope struct{}

func (namespaceScope) Name() meta.RESTScopeName { return meta.RESTScopeNameNamespace }

// stubRESTMapper returns a fixed mapping for any GroupKind lookup.
type stubRESTMapper struct {
	gvk schema.GroupVersionKind
}

func (m *stubRESTMapper) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return m.gvk, nil
}
func (m *stubRESTMapper) KindsFor(resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return []schema.GroupVersionKind{m.gvk}, nil
}
func (m *stubRESTMapper) ResourceFor(input schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return input, nil
}
func (m *stubRESTMapper) ResourcesFor(input schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return []schema.GroupVersionResource{input}, nil
}
func (m *stubRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return &meta.RESTMapping{
		Resource:         schema.GroupVersionResource{Group: gk.Group, Version: m.gvk.Version, Resource: "workloads"},
		GroupVersionKind: schema.GroupVersionKind{Group: gk.Group, Version: m.gvk.Version, Kind: gk.Kind},
		Scope:            namespaceScope{},
	}, nil
}
func (m *stubRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) ([]*meta.RESTMapping, error) {
	mapping, err := m.RESTMapping(gk, versions...)
	if err != nil {
		return nil, err
	}
	return []*meta.RESTMapping{mapping}, nil
}
func (m *stubRESTMapper) ResourceSingularizer(resource string) (string, error) { return resource, nil }

// patchCapturingClient wraps a client.Client and records every Patch call
// whose object is an *unstructured.Unstructured.
type patchCapturingClient struct {
	client.Client
	patches []*unstructured.Unstructured
}

func (c *patchCapturingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		c.patches = append(c.patches, u.DeepCopy())
	}
	return nil
}

func newQuotaFanOutScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = quotav1alpha1.AddToScheme(s)
	return s
}

// TestApplyClaimCreationPolicies_NoResourceRef verifies the serialization
// contract for the unstructured ClaimCreationPolicy SSA patch:
//   - spec.target.resourceClaimTemplate.spec.resourceRef must be absent
//     (the server's CEL rule rejects the field; typed marshaling always emits
//     it for a non-pointer struct even when zero-valued — this is the exact
//     bug the fix addresses)
//   - all required camelCase keys are present with non-empty values
func TestApplyClaimCreationPolicies_NoResourceRef(t *testing.T) {
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sc",
			UID:  "test-uid-1234",
		},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Quota: &servicesv1alpha1.ServiceQuotaConfig{
				MetricRules: []servicesv1alpha1.QuotaMetricRule{
					{
						Selector: servicesv1alpha1.QuotaMetricRuleSelector{
							APIGroup: "compute.datumapis.com",
							Kind:     "Workload",
						},
						MetricCosts: map[string]int64{
							"workloads": 1,
						},
					},
				},
			},
		},
	}

	scheme := newQuotaFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	capturing := &patchCapturingClient{Client: base}

	mapper := &stubRESTMapper{
		gvk: schema.GroupVersionKind{
			Group:   "compute.datumapis.com",
			Version: "v1alpha1",
			Kind:    "Workload",
		},
	}

	fanOut := &QuotaFanOut{
		Client:     capturing,
		Scheme:     scheme,
		RESTMapper: mapper,
	}

	if _, err := fanOut.applyClaimCreationPolicies(context.Background(), sc); err != nil {
		t.Fatalf("applyClaimCreationPolicies: %v", err)
	}

	if len(capturing.patches) == 0 {
		t.Fatal("expected at least one Patch call, got none")
	}

	obj := capturing.patches[0].Object

	spec := requireMap(t, obj, "spec")
	target := requireMap(t, spec, "target")
	rct := requireMap(t, target, "resourceClaimTemplate")
	rctSpec := requireMap(t, rct, "spec")

	if _, exists := rctSpec["resourceRef"]; exists {
		t.Error("spec.target.resourceClaimTemplate.spec.resourceRef is present; the server's CEL rule will reject this object")
	}

	trigger := requireMap(t, spec, "trigger")
	triggerResource := requireMap(t, trigger, "resource")
	requireNonEmptyString(t, triggerResource, "apiVersion", "spec.trigger.resource.apiVersion")
	requireNonEmptyString(t, triggerResource, "kind", "spec.trigger.resource.kind")

	rctMeta := requireMap(t, rct, "metadata")
	requireNonEmptyString(t, rctMeta, "generateName", "spec.target.resourceClaimTemplate.metadata.generateName")
	requireNonEmptyString(t, rctMeta, "namespace", "spec.target.resourceClaimTemplate.metadata.namespace")

	requests, ok := rctSpec["requests"]
	if !ok {
		t.Fatal("spec.target.resourceClaimTemplate.spec.requests is absent")
	}
	reqSlice, ok := requests.([]interface{})
	if !ok || len(reqSlice) == 0 {
		t.Errorf("spec.target.resourceClaimTemplate.spec.requests: want non-empty slice, got %T %v", requests, requests)
	}

	metadata := requireMap(t, obj, "metadata")
	ownerRefs, ok := metadata["ownerReferences"]
	if !ok {
		t.Fatal("metadata.ownerReferences is absent")
	}
	ownerSlice, ok := ownerRefs.([]interface{})
	if !ok || len(ownerSlice) == 0 {
		t.Errorf("metadata.ownerReferences: want non-empty slice, got %T %v", ownerRefs, ownerRefs)
	}
}

func requireMap(t *testing.T, m map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q not found", key)
	}
	result, ok := v.(map[string]interface{})
	if !ok {
		t.Fatalf("key %q: expected map[string]interface{}, got %T", key, v)
	}
	return result
}

func requireNonEmptyString(t *testing.T, m map[string]interface{}, key, path string) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("%s is absent", path)
		return
	}
	s, ok := v.(string)
	if !ok || s == "" {
		t.Errorf("%s = %v; want non-empty string", path, v)
	}
}
