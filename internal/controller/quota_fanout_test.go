// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// whose object is a *quotav1alpha1.ClaimCreationPolicy or
// *quotav1alpha1.ResourceRegistration.
type patchCapturingClient struct {
	client.Client
	policies      []*quotav1alpha1.ClaimCreationPolicy
	registrations []*quotav1alpha1.ResourceRegistration
}

func (c *patchCapturingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	switch o := obj.(type) {
	case *quotav1alpha1.ClaimCreationPolicy:
		c.policies = append(c.policies, o.DeepCopy())
	case *quotav1alpha1.ResourceRegistration:
		c.registrations = append(c.registrations, o.DeepCopy())
	}
	return nil
}

func newQuotaFanOutScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = quotav1alpha1.AddToScheme(s)
	return s
}

// TestApplyClaimCreationPolicies_NoResourceRef verifies that the
// ClaimCreationPolicy applied by the controller does not include ResourceRef
// in the template spec. The server's CEL rule rejects any template where
// spec.target.resourceClaimTemplate.spec.resourceRef is set, because the
// field is server-populated at admission time. The upstream fix
// (milo-os/milo#623) makes ResourceRef a pointer so omitempty works correctly;
// this test confirms the typed path produces a valid object.
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

	if _, err := fanOut.applyClaimCreationPolicies(context.Background(), sc, "test-service"); err != nil {
		t.Fatalf("applyClaimCreationPolicies: %v", err)
	}

	if len(capturing.policies) == 0 {
		t.Fatal("expected at least one Patch call, got none")
	}

	ccp := capturing.policies[0]
	spec := ccp.Spec

	// ResourceRef must be nil — the server rejects templates that include it.
	if spec.Target.ResourceClaimTemplate.Spec.ResourceRef != nil {
		t.Error("ResourceClaimTemplate.Spec.ResourceRef is non-nil; the server's CEL rule will reject this object")
	}

	// Trigger resource must be fully populated.
	if spec.Trigger.Resource.APIVersion == "" {
		t.Error("Trigger.Resource.APIVersion is empty")
	}
	if spec.Trigger.Resource.Kind == "" {
		t.Error("Trigger.Resource.Kind is empty")
	}

	// Template metadata must have dynamic name expressions.
	meta := spec.Target.ResourceClaimTemplate.Metadata
	if meta.GenerateName == "" {
		t.Error("ResourceClaimTemplate.Metadata.GenerateName is empty")
	}
	if meta.Namespace == "" {
		t.Error("ResourceClaimTemplate.Metadata.Namespace is empty")
	}

	// At least one resource request must be present.
	if len(spec.Target.ResourceClaimTemplate.Spec.Requests) == 0 {
		t.Error("ResourceClaimTemplate.Spec.Requests is empty")
	}

	// Owner reference must be set so the CCP is garbage-collected with the SC.
	if len(ccp.OwnerReferences) == 0 {
		t.Error("OwnerReferences is empty")
	}
}

// TestQuotaFanOut_OwnerServiceLabel verifies that the canonical service name
// passed in lands on the labelOwnerService label of both the produced
// ResourceRegistration and ClaimCreationPolicy, alongside the managed-by label.
func TestQuotaFanOut_OwnerServiceLabel(t *testing.T) {
	const serviceName = "compute.miloapis.com"

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sc",
			UID:  "test-uid-owner-label",
		},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Quota: &servicesv1alpha1.ServiceQuotaConfig{
				Limits: []servicesv1alpha1.QuotaLimitSpec{
					{
						Name:         "instances",
						Metric:       "compute.miloapis.com/instances",
						DefaultLimit: 10,
						Unit:         "1/{project}",
						ConsumerType: servicesv1alpha1.QuotaConsumerType{
							APIGroup: "resourcemanager.miloapis.com",
							Kind:     "Project",
						},
					},
				},
				MetricRules: []servicesv1alpha1.QuotaMetricRule{
					{
						Selector: servicesv1alpha1.QuotaMetricRuleSelector{
							APIGroup: "compute.datumapis.com",
							Kind:     "Workload",
						},
						MetricCosts: map[string]int64{"compute.miloapis.com/instances": 1},
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

	if _, err := fanOut.applyResourceRegistrations(context.Background(), sc, serviceName); err != nil {
		t.Fatalf("applyResourceRegistrations: %v", err)
	}
	if _, err := fanOut.applyClaimCreationPolicies(context.Background(), sc, serviceName); err != nil {
		t.Fatalf("applyClaimCreationPolicies: %v", err)
	}

	if len(capturing.registrations) == 0 {
		t.Fatal("expected at least one ResourceRegistration Patch, got none")
	}
	rr := capturing.registrations[0]
	if rr.Labels[labelOwnerService] != serviceName {
		t.Errorf("ResourceRegistration label %q = %q, want %q", labelOwnerService, rr.Labels[labelOwnerService], serviceName)
	}
	if rr.Labels[labelManagedBy] != labelManagedByValue {
		t.Errorf("ResourceRegistration label %q = %q, want %q", labelManagedBy, rr.Labels[labelManagedBy], labelManagedByValue)
	}

	if len(capturing.policies) == 0 {
		t.Fatal("expected at least one ClaimCreationPolicy Patch, got none")
	}
	ccp := capturing.policies[0]
	if ccp.Labels[labelOwnerService] != serviceName {
		t.Errorf("ClaimCreationPolicy label %q = %q, want %q", labelOwnerService, ccp.Labels[labelOwnerService], serviceName)
	}
	if ccp.Labels[labelManagedBy] != labelManagedByValue {
		t.Errorf("ClaimCreationPolicy label %q = %q, want %q", labelManagedBy, ccp.Labels[labelManagedBy], labelManagedByValue)
	}
}

// TestQuotaFanOut_DisplayMetadataAnnotations verifies that a limit's
// DisplayName/Description are propagated to the generated ResourceRegistration
// as kubernetes.io display annotations, and that a limit without display
// metadata produces no annotations (rather than empty-string entries).
func TestQuotaFanOut_DisplayMetadataAnnotations(t *testing.T) {
	const serviceName = "compute.datumapis.com"

	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sc",
			UID:  "test-uid-display-annotations",
		},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			Phase: servicesv1alpha1.PhasePublished,
			Quota: &servicesv1alpha1.ServiceQuotaConfig{
				Limits: []servicesv1alpha1.QuotaLimitSpec{
					{
						Name:         "instances",
						DisplayName:  "Instances",
						Description:  "Maximum number of compute instances per project",
						Metric:       "compute.datumapis.com/instances",
						DefaultLimit: 10,
						Unit:         "1/{project}",
						ConsumerType: servicesv1alpha1.QuotaConsumerType{
							APIGroup: "resourcemanager.miloapis.com",
							Kind:     "Project",
						},
					},
					{
						Name:         "workloads",
						Metric:       "compute.datumapis.com/workloads",
						DefaultLimit: 1000,
						Unit:         "1/{project}",
						ConsumerType: servicesv1alpha1.QuotaConsumerType{
							APIGroup: "resourcemanager.miloapis.com",
							Kind:     "Project",
						},
					},
				},
			},
		},
	}

	scheme := newQuotaFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	capturing := &patchCapturingClient{Client: base}
	fanOut := &QuotaFanOut{
		Client:     capturing,
		Scheme:     scheme,
		RESTMapper: &stubRESTMapper{},
	}

	if _, err := fanOut.applyResourceRegistrations(context.Background(), sc, serviceName); err != nil {
		t.Fatalf("applyResourceRegistrations: %v", err)
	}

	byResourceType := make(map[string]map[string]string)
	for _, rr := range capturing.registrations {
		byResourceType[rr.Spec.ResourceType] = rr.Annotations
	}

	withMeta := byResourceType["compute.datumapis.com/instances"]
	if got := withMeta[annotationDisplayName]; got != "Instances" {
		t.Errorf("display-name annotation = %q, want %q", got, "Instances")
	}
	if got := withMeta[annotationDescription]; got != "Maximum number of compute instances per project" {
		t.Errorf("description annotation = %q, want the limit description", got)
	}

	if anns := byResourceType["compute.datumapis.com/workloads"]; len(anns) != 0 {
		t.Errorf("expected no annotations for a limit without display metadata, got %v", anns)
	}
}
