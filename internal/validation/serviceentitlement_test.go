// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

func validationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := servicesv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func newFakeReader(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(validationScheme(t)).
		WithObjects(objs...).
		Build()
}

func publishedService(name string) *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: name + ".miloapis.com",
			DisplayName: name,
			Phase:       servicesv1alpha1.PhasePublished,
			Owner: servicesv1alpha1.ServiceOwner{
				ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{Name: "producer"},
			},
		},
	}
}

func draftService(name string) *servicesv1alpha1.Service {
	svc := publishedService(name)
	svc.Spec.Phase = servicesv1alpha1.PhaseDraft
	return svc
}

func entitlement(name, serviceRef string) *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: serviceRef},
		},
	}
}

func TestValidateServiceEntitlementCreate_RejectsMissingService(t *testing.T) {
	c := newFakeReader(t)
	errs := ValidateServiceEntitlementCreate(context.Background(), c, entitlement("a", "missing"))
	if len(errs) == 0 {
		t.Fatal("expected error for missing service, got none")
	}
}

func TestValidateServiceEntitlementCreate_RejectsDraftService(t *testing.T) {
	c := newFakeReader(t, draftService("compute"))
	errs := ValidateServiceEntitlementCreate(context.Background(), c, entitlement("a", "compute"))
	if len(errs) == 0 {
		t.Fatal("expected error for draft service, got none")
	}
}

func TestValidateServiceEntitlementCreate_AcceptsPublishedService(t *testing.T) {
	c := newFakeReader(t, publishedService("compute"))
	errs := ValidateServiceEntitlementCreate(context.Background(), c, entitlement("a", "compute"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestValidateServiceEntitlementUpdate_ServiceRefImmutable(t *testing.T) {
	old := entitlement("a", "compute")
	updated := entitlement("a", "storage")
	errs := ValidateServiceEntitlementUpdate(context.Background(), nil, old, updated)
	if len(errs) == 0 {
		t.Fatal("expected error when changing serviceRef.name")
	}
}

func TestValidateServiceEntitlementUpdate_UnchangedServiceRef(t *testing.T) {
	old := entitlement("a", "compute")
	errs := ValidateServiceEntitlementUpdate(context.Background(), nil, old, old.DeepCopy())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestValidateServiceEntitlementDelete_RejectsDependencyWithActiveParent(t *testing.T) {
	parent := entitlement("parent", "parent")
	parent.Status.Phase = servicesv1alpha1.EntitlementPhaseActive

	dep := entitlement("dep", "dep")
	dep.Status.Origin = servicesv1alpha1.EntitlementOriginDependency
	dep.Status.DependencyOf = "parent"

	c := newFakeReader(t, parent)
	errs := ValidateServiceEntitlementDelete(context.Background(), c, dep)
	if len(errs) == 0 {
		t.Fatal("expected error deleting dependency while parent active")
	}
}

func TestValidateServiceEntitlementDelete_AcceptsDirect(t *testing.T) {
	ent := entitlement("a", "compute")
	ent.Status.Origin = servicesv1alpha1.EntitlementOriginDirect
	errs := ValidateServiceEntitlementDelete(context.Background(), nil, ent)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors deleting direct entitlement: %v", errs)
	}
}
