// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	provTestService  = "networking-datumapis-com"
	provTestProducer = "networking-platform"
)

func provTestSelector() metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{"ipam.miloapis.com/shared": "true"}}
}

func provTestConfig(kind servicesv1alpha1.ProjectedKindRef, sourceProject string, selector metav1.LabelSelector) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: provTestService},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: provTestService},
			Phase:      servicesv1alpha1.PhasePublished,
			Provisioning: &servicesv1alpha1.ServiceProvisioningConfig{
				Resources: []servicesv1alpha1.ProvisionedResourceSpec{{
					Name: "public-classes",
					Projection: servicesv1alpha1.ResourceProjectionSpec{
						SourceProject: sourceProject,
						Kind:          kind,
						Reference:     provTestReference(),
						Selector:      selector,
					},
				}},
			},
		},
	}
}

func provTestKind() servicesv1alpha1.ProjectedKindRef {
	return servicesv1alpha1.ProjectedKindRef{Group: "ipam.miloapis.com", Version: "v1alpha1", Kind: "IPClass"}
}

// The shape IPAM gives a cross-project class reference. The provider states it;
// the platform holds no table saying what it should be.
func provTestReference() servicesv1alpha1.ProjectedReferenceSpec {
	return servicesv1alpha1.ProjectedReferenceSpec{
		FieldPath:  "source",
		ProjectKey: "project",
		NameKey:    "name",
	}
}

func provTestServiceObj() *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: provTestService},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: "networking.datumapis.com",
			Owner: servicesv1alpha1.ServiceOwner{
				ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{Name: provTestProducer},
			},
		},
	}
}

func provTestReader(objs ...*servicesv1alpha1.Service) *fake.ClientBuilder {
	b := fake.NewClientBuilder().WithScheme(newServiceConfigurationValidationScheme())
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b
}

func TestProvisioningValidationAcceptsResolvableProjection(t *testing.T) {
	sc := provTestConfig(provTestKind(), provTestProducer, provTestSelector())
	if errs := validateProvisioning(sc); len(errs) != 0 {
		t.Fatalf("expected a valid projection to be accepted, got %v", errs)
	}
	errs := validateProvisioningSourceProjects(context.Background(),
		provTestReader(provTestServiceObj()).Build(), sc)
	if len(errs) != 0 {
		t.Fatalf("expected the producer project to be accepted as a source, got %v", errs)
	}
}

// Admission is the first of two enforcement points: a provider gets a
// synchronous error rather than a status field later. What it checks is that
// the declaration resolves into a write — which kinds are acceptable is the
// target API's decision, made when it accepts or refuses the write.
func TestProvisioningValidationRejectsUnresolvableProjection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  servicesv1alpha1.ProjectedKindRef
		ref   servicesv1alpha1.ProjectedReferenceSpec
		field string
	}{
		{
			name:  "core group",
			kind:  servicesv1alpha1.ProjectedKindRef{Group: "", Version: "v1", Kind: "Secret"},
			ref:   provTestReference(),
			field: "spec.provisioning.resources[0].projection.kind.group",
		},
		{
			name:  "no version",
			kind:  servicesv1alpha1.ProjectedKindRef{Group: "ipam.miloapis.com", Kind: "IPClass"},
			ref:   provTestReference(),
			field: "spec.provisioning.resources[0].projection.kind.version",
		},
		{
			name:  "reference path escapes spec",
			kind:  provTestKind(),
			ref:   servicesv1alpha1.ProjectedReferenceSpec{FieldPath: "metadata.ownerReferences[0]", ProjectKey: "project", NameKey: "name"},
			field: "spec.provisioning.resources[0].projection.reference.fieldPath",
		},
		{
			name:  "one key for both values",
			kind:  provTestKind(),
			ref:   servicesv1alpha1.ProjectedReferenceSpec{FieldPath: "source", ProjectKey: "name", NameKey: "name"},
			field: "spec.provisioning.resources[0].projection.reference.nameKey",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := provTestConfig(tc.kind, provTestProducer, provTestSelector())
			sc.Spec.Provisioning.Resources[0].Projection.Reference = tc.ref

			errs := validateProvisioning(sc)
			if len(errs) != 1 {
				t.Fatalf("expected one rejection, got %v", errs)
			}
			if got := errs[0].Field; got != tc.field {
				t.Errorf("error points at %q, want %q", got, tc.field)
			}
		})
	}
}

// An empty selector converts to "match everything". Projecting a whole source
// project by omission has to be refused rather than interpreted.
func TestProvisioningValidationRequiresNonEmptySelector(t *testing.T) {
	sc := provTestConfig(provTestKind(), provTestProducer, metav1.LabelSelector{})
	errs := validateProvisioning(sc)
	if len(errs) != 1 {
		t.Fatalf("expected the empty selector to be rejected, got %v", errs)
	}
	if got := errs[0].Field; got != "spec.provisioning.resources[0].projection.selector" {
		t.Errorf("error points at %q, want the selector field", got)
	}
}

// A provider must not project out of a project it does not own; the platform
// would read it with an identity nothing would stop.
func TestProvisioningValidationRejectsForeignSourceProject(t *testing.T) {
	sc := provTestConfig(provTestKind(), "someone-elses-project", provTestSelector())
	errs := validateProvisioningSourceProjects(context.Background(),
		provTestReader(provTestServiceObj()).Build(), sc)
	if len(errs) != 1 {
		t.Fatalf("expected a foreign source project to be rejected, got %v", errs)
	}
	if !strings.Contains(errs[0].Detail, provTestProducer) {
		t.Errorf("error does not name the producer project: %q", errs[0].Detail)
	}
}

func TestProvisioningValidationRejectsDuplicateResourceNames(t *testing.T) {
	sc := provTestConfig(provTestKind(), provTestProducer, provTestSelector())
	sc.Spec.Provisioning.Resources = append(sc.Spec.Provisioning.Resources,
		sc.Spec.Provisioning.Resources[0])

	errs := validateProvisioning(sc)
	if len(errs) != 1 {
		t.Fatalf("expected a duplicate declaration name to be rejected, got %v", errs)
	}
}

// The selector must stay editable on a Published configuration: adjusting which
// of a provider's own objects are offered is how new objects reach
// already-entitled projects without republishing.
func TestProvisioningPublishedImmutabilityAllowsSelectorChange(t *testing.T) {
	oldSC := provTestConfig(provTestKind(), provTestProducer, provTestSelector())
	newSC := provTestConfig(provTestKind(), provTestProducer, metav1.LabelSelector{
		MatchLabels: map[string]string{"ipam.miloapis.com/tier": "premium"},
	})

	if errs := validateProvisioningPublishedImmutability(oldSC, newSC); len(errs) != 0 {
		t.Fatalf("expected a selector change on Published to be allowed, got %v", errs)
	}
}

// Changing the kind, source project, or reference shape under a retained name
// silently re-points or rewrites everything already installed under it.
func TestProvisioningPublishedImmutabilityFreezesKindSourceAndReference(t *testing.T) {
	oldSC := provTestConfig(provTestKind(), provTestProducer, provTestSelector())

	changedKind := provTestConfig(servicesv1alpha1.ProjectedKindRef{Group: "ipam.miloapis.com", Version: "v1alpha1", Kind: "IPPool"},
		provTestProducer, provTestSelector())
	if errs := validateProvisioningPublishedImmutability(oldSC, changedKind); len(errs) != 1 {
		t.Errorf("expected a kind change on Published to be forbidden, got %v", errs)
	}

	changedSource := provTestConfig(provTestKind(), "another-project", provTestSelector())
	if errs := validateProvisioningPublishedImmutability(oldSC, changedSource); len(errs) != 1 {
		t.Errorf("expected a source project change on Published to be forbidden, got %v", errs)
	}

	// Moving the reference elsewhere in the spec rewrites every installed
	// object under a name a consumer already relies on.
	changedRef := provTestConfig(provTestKind(), provTestProducer, provTestSelector())
	changedRef.Spec.Provisioning.Resources[0].Projection.Reference.FieldPath = "classSource"
	if errs := validateProvisioningPublishedImmutability(oldSC, changedRef); len(errs) != 1 {
		t.Errorf("expected a reference change on Published to be forbidden, got %v", errs)
	}
}

// Withdrawing a declaration is the documented rollback path, so removal stays
// permitted even though other Published sections forbid it.
func TestProvisioningPublishedImmutabilityAllowsWithdrawal(t *testing.T) {
	oldSC := provTestConfig(provTestKind(), provTestProducer, provTestSelector())
	newSC := provTestConfig(provTestKind(), provTestProducer, provTestSelector())
	newSC.Spec.Provisioning.Resources = nil

	if errs := validateProvisioningPublishedImmutability(oldSC, newSC); len(errs) != 0 {
		t.Fatalf("expected withdrawing a declaration to be allowed, got %v", errs)
	}
}

// A declaration can go invalid without being edited: the schema can be narrowed
// under it, and a document can be admitted while the webhook is absent.
// Removing the controller's finalizer is an update, so re-validating a
// terminating configuration would leave it undeletable.
func TestProvisioningValidationSkipsTerminatingConfiguration(t *testing.T) {
	oldSC := provTestConfig(provTestKind(), provTestProducer, provTestSelector())
	newSC := provTestConfig(servicesv1alpha1.ProjectedKindRef{Group: "iam.miloapis.com", Kind: "PolicyBinding"},
		"someone-elses-project", metav1.LabelSelector{})
	now := metav1.Now()
	newSC.DeletionTimestamp = &now
	newSC.Finalizers = nil

	errs := ValidateServiceConfigurationUpdate(context.Background(),
		provTestReader(provTestServiceObj()).Build(), oldSC, newSC, false)
	for _, e := range errs {
		if strings.Contains(e.Field, "provisioning") {
			t.Errorf("terminating configuration was blocked by provisioning validation: %v", e)
		}
	}
}
