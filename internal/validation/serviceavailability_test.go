// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func serviceAvailability(name, serviceRef, locName, locNamespace string) *servicesv1alpha1.ServiceAvailability {
	return &servicesv1alpha1.ServiceAvailability{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceAvailabilitySpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: serviceRef},
			LocationRef: servicesv1alpha1.LocationRef{Name: locName, Namespace: locNamespace},
		},
	}
}

func TestValidateServiceAvailabilityCreate_RejectsMissingService(t *testing.T) {
	c := newFakeReader(t)
	errs := ValidateServiceAvailabilityCreate(context.Background(), c,
		serviceAvailability("a", "missing", "us-central1-a", "platform"))
	if len(errs) == 0 {
		t.Fatal("expected error for missing service, got none")
	}
}

func TestValidateServiceAvailabilityCreate_RejectsNonPublishedService(t *testing.T) {
	for _, phase := range []servicesv1alpha1.Phase{
		servicesv1alpha1.PhaseDraft,
		servicesv1alpha1.PhaseDeprecated,
		servicesv1alpha1.PhaseRetired,
	} {
		svc := publishedService("compute")
		svc.Spec.Phase = phase
		c := newFakeReader(t, svc)
		errs := ValidateServiceAvailabilityCreate(context.Background(), c,
			serviceAvailability("a", "compute", "us-central1-a", "platform"))
		if len(errs) == 0 {
			t.Fatalf("expected error for %q service, got none", phase)
		}
	}
}

func TestValidateServiceAvailabilityCreate_AcceptsPublishedService(t *testing.T) {
	c := newFakeReader(t, publishedService("compute"))
	errs := ValidateServiceAvailabilityCreate(context.Background(), c,
		serviceAvailability("a", "compute", "us-central1-a", "platform"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestValidateServiceAvailabilityCreate_RejectsEmptyLocationName(t *testing.T) {
	c := newFakeReader(t, publishedService("compute"))
	errs := ValidateServiceAvailabilityCreate(context.Background(), c,
		serviceAvailability("a", "compute", "", "platform"))
	if len(errs) == 0 {
		t.Fatal("expected error for empty locationRef.name, got none")
	}
}

func TestValidateServiceAvailabilityUpdate_ServiceRefImmutable(t *testing.T) {
	old := serviceAvailability("a", "compute", "us-central1-a", "platform")
	updated := serviceAvailability("a", "storage", "us-central1-a", "platform")
	errs := ValidateServiceAvailabilityUpdate(context.Background(), nil, old, updated)
	if len(errs) == 0 {
		t.Fatal("expected error when changing serviceRef")
	}
}

func TestValidateServiceAvailabilityUpdate_LocationRefImmutable(t *testing.T) {
	old := serviceAvailability("a", "compute", "us-central1-a", "platform")
	updated := serviceAvailability("a", "compute", "eu-west1-b", "platform")
	errs := ValidateServiceAvailabilityUpdate(context.Background(), nil, old, updated)
	if len(errs) == 0 {
		t.Fatal("expected error when changing locationRef")
	}
}

func TestValidateServiceAvailabilityUpdate_Unchanged(t *testing.T) {
	old := serviceAvailability("a", "compute", "us-central1-a", "platform")
	errs := ValidateServiceAvailabilityUpdate(context.Background(), nil, old, old.DeepCopy())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}
