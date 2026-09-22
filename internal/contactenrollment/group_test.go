// SPDX-License-Identifier: AGPL-3.0-only

package contactenrollment

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func newServiceWithContactEnrollment(name, serviceName string, ce *servicesv1alpha1.ContactEnrollment) *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName:       serviceName,
			DisplayName:       "Compute",
			Phase:             servicesv1alpha1.PhasePublished,
			ContactEnrollment: ce,
			Owner: servicesv1alpha1.ServiceOwner{
				ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{Name: "compute-platform"},
			},
		},
	}
}

func TestEnsureGroup_CreatesWithDefaults(t *testing.T) {
	svc := newServiceWithContactEnrollment("compute", "compute.miloapis.com", &servicesv1alpha1.ContactEnrollment{
		ContactGroupRef: servicesv1alpha1.ContactGroupRef{Name: "compute-testers"},
	})
	c := newFakeClient(t)

	got, err := EnsureGroup(context.Background(), c, testNamespace, svc)
	if err != nil {
		t.Fatalf("EnsureGroup() error = %v", err)
	}
	if got.Name != "compute-testers" || got.Namespace != testNamespace {
		t.Fatalf("EnsureGroup() = %s/%s, want %s/compute-testers", got.Namespace, got.Name, testNamespace)
	}
	if got.Spec.DisplayName != "Compute" {
		t.Errorf("DisplayName = %q, want fallback to service displayName %q", got.Spec.DisplayName, "Compute")
	}
	if got.Spec.Description != "Consumers registered for compute.miloapis.com." {
		t.Errorf("Description = %q, want generated default", got.Spec.Description)
	}
	if got.Spec.Visibility != notificationv1alpha1.ContactGroupVisibilityPublic {
		t.Errorf("Visibility = %q, want public default", got.Spec.Visibility)
	}
	if len(got.Spec.Providers) != 0 {
		t.Errorf("Providers = %v, want empty so an operator attaches sync destinations afterward", got.Spec.Providers)
	}

	// Verify it was actually persisted, not just returned in memory.
	persisted := &notificationv1alpha1.ContactGroup{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "compute-testers"}, persisted); err != nil {
		t.Fatalf("get persisted ContactGroup: %v", err)
	}
}

func TestEnsureGroup_UsesExplicitOverrides(t *testing.T) {
	svc := newServiceWithContactEnrollment("compute", "compute.miloapis.com", &servicesv1alpha1.ContactEnrollment{
		ContactGroupRef: servicesv1alpha1.ContactGroupRef{Name: "compute-testers", Namespace: "custom-ns"},
		DisplayName:     "Compute Beta Testers",
		Description:     "Hand-picked beta testers.",
		Visibility:      servicesv1alpha1.ContactGroupVisibilityPrivate,
	})
	c := newFakeClient(t)

	got, err := EnsureGroup(context.Background(), c, testNamespace, svc)
	if err != nil {
		t.Fatalf("EnsureGroup() error = %v", err)
	}
	if got.Namespace != "custom-ns" {
		t.Errorf("Namespace = %q, want the explicit contactGroupRef.namespace %q", got.Namespace, "custom-ns")
	}
	if got.Spec.DisplayName != "Compute Beta Testers" {
		t.Errorf("DisplayName = %q, want explicit override", got.Spec.DisplayName)
	}
	if got.Spec.Description != "Hand-picked beta testers." {
		t.Errorf("Description = %q, want explicit override", got.Spec.Description)
	}
	if got.Spec.Visibility != notificationv1alpha1.ContactGroupVisibilityPrivate {
		t.Errorf("Visibility = %q, want explicit private override", got.Spec.Visibility)
	}
}

func TestEnsureGroup_ReturnsExistingGroupUnchanged(t *testing.T) {
	existing := &notificationv1alpha1.ContactGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-testers", Namespace: testNamespace},
		Spec: notificationv1alpha1.ContactGroupSpec{
			DisplayName: "Operator-Renamed Group",
			Visibility:  notificationv1alpha1.ContactGroupVisibilityPrivate,
			Description: "Operator's own description.",
		},
	}
	svc := newServiceWithContactEnrollment("compute", "compute.miloapis.com", &servicesv1alpha1.ContactEnrollment{
		ContactGroupRef: servicesv1alpha1.ContactGroupRef{Name: "compute-testers"},
		// These would produce different defaults, but must not overwrite an
		// existing group's settings.
		DisplayName: "Ignored",
		Visibility:  servicesv1alpha1.ContactGroupVisibilityPublic,
	})
	c := newFakeClient(t, existing)

	got, err := EnsureGroup(context.Background(), c, testNamespace, svc)
	if err != nil {
		t.Fatalf("EnsureGroup() error = %v", err)
	}
	if got.Spec.DisplayName != "Operator-Renamed Group" {
		t.Errorf("EnsureGroup() overwrote an existing group's DisplayName: got %q", got.Spec.DisplayName)
	}
	if got.Spec.Visibility != notificationv1alpha1.ContactGroupVisibilityPrivate {
		t.Errorf("EnsureGroup() overwrote an existing group's Visibility: got %q", got.Spec.Visibility)
	}
}

func TestEnsureGroup_NoContactEnrollment(t *testing.T) {
	svc := newServiceWithContactEnrollment("compute", "compute.miloapis.com", nil)
	c := newFakeClient(t)

	if _, err := EnsureGroup(context.Background(), c, testNamespace, svc); err == nil {
		t.Fatal("EnsureGroup() expected an error for a service with no contactEnrollment configured")
	}
}
