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
	return metav1.LabelSelector{MatchLabels: map[string]string{"networking.datumapis.com/shared": "true"}}
}

func provTestConfig(kind servicesv1alpha1.GVKRef, sourceProject string, selector metav1.LabelSelector) *servicesv1alpha1.ServiceConfiguration {
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
						Selector:      selector,
					},
				}},
			},
		},
	}
}

func provTestAllowedKind() servicesv1alpha1.GVKRef {
	return servicesv1alpha1.GVKRef{Group: "networking.datumapis.com", Kind: "IPClass"}
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

func TestProvisioningValidationAcceptsAllowlistedProjection(t *testing.T) {
	sc := provTestConfig(provTestAllowedKind(), provTestProducer, provTestSelector())
	if errs := validateProvisioning(sc); len(errs) != 0 {
		t.Fatalf("expected a valid projection to be accepted, got %v", errs)
	}
	errs := validateProvisioningSourceProjects(context.Background(),
		provTestReader(provTestServiceObj()).Build(), sc)
	if len(errs) != 0 {
		t.Fatalf("expected the producer project to be accepted as a source, got %v", errs)
	}
}

// Admission is the first of the allowlist's two enforcement points: a provider
// gets a synchronous error rather than a status field later.
func TestProvisioningValidationRejectsKindOutsideAllowlist(t *testing.T) {
	sc := provTestConfig(servicesv1alpha1.GVKRef{Group: "apps", Kind: "Deployment"},
		provTestProducer, provTestSelector())

	errs := validateProvisioning(sc)
	if len(errs) != 1 {
		t.Fatalf("expected the unlisted kind to be rejected, got %v", errs)
	}
	if got := errs[0].Field; got != "spec.provisioning.resources[0].projection.kind" {
		t.Errorf("error points at %q, want the kind field", got)
	}
	if !strings.Contains(errs[0].Detail, "allowlist") {
		t.Errorf("error does not explain the allowlist: %q", errs[0].Detail)
	}
}

func TestProvisioningValidationRejectsCategoricallyDeniedKind(t *testing.T) {
	for _, kind := range []servicesv1alpha1.GVKRef{
		{Group: "iam.miloapis.com", Kind: "PolicyBinding"},
		{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"},
		{Group: "", Kind: "Secret"},
	} {
		sc := provTestConfig(kind, provTestProducer, provTestSelector())
		errs := validateProvisioning(sc)
		if len(errs) != 1 {
			t.Fatalf("kind %s: expected rejection, got %v", kind.Kind, errs)
		}
		if !strings.Contains(errs[0].Detail, "never be provisioned") {
			t.Errorf("kind %s: refusal does not read as categorical: %q", kind.Kind, errs[0].Detail)
		}
	}
}

// An empty selector converts to "match everything". Projecting a whole source
// project by omission has to be refused rather than interpreted.
func TestProvisioningValidationRequiresNonEmptySelector(t *testing.T) {
	sc := provTestConfig(provTestAllowedKind(), provTestProducer, metav1.LabelSelector{})
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
	sc := provTestConfig(provTestAllowedKind(), "someone-elses-project", provTestSelector())
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
	sc := provTestConfig(provTestAllowedKind(), provTestProducer, provTestSelector())
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
	oldSC := provTestConfig(provTestAllowedKind(), provTestProducer, provTestSelector())
	newSC := provTestConfig(provTestAllowedKind(), provTestProducer, metav1.LabelSelector{
		MatchLabels: map[string]string{"networking.datumapis.com/tier": "premium"},
	})

	if errs := validateProvisioningPublishedImmutability(oldSC, newSC); len(errs) != 0 {
		t.Fatalf("expected a selector change on Published to be allowed, got %v", errs)
	}
}

// Changing the kind or source project under a retained name silently re-points
// everything already installed under it.
func TestProvisioningPublishedImmutabilityFreezesKindAndSource(t *testing.T) {
	oldSC := provTestConfig(provTestAllowedKind(), provTestProducer, provTestSelector())

	changedKind := provTestConfig(servicesv1alpha1.GVKRef{Group: "networking.datumapis.com", Kind: "Location"},
		provTestProducer, provTestSelector())
	if errs := validateProvisioningPublishedImmutability(oldSC, changedKind); len(errs) != 1 {
		t.Errorf("expected a kind change on Published to be forbidden, got %v", errs)
	}

	changedSource := provTestConfig(provTestAllowedKind(), "another-project", provTestSelector())
	if errs := validateProvisioningPublishedImmutability(oldSC, changedSource); len(errs) != 1 {
		t.Errorf("expected a source project change on Published to be forbidden, got %v", errs)
	}
}

// Withdrawing a declaration is the documented rollback path, so removal stays
// permitted even though other Published sections forbid it.
func TestProvisioningPublishedImmutabilityAllowsWithdrawal(t *testing.T) {
	oldSC := provTestConfig(provTestAllowedKind(), provTestProducer, provTestSelector())
	newSC := provTestConfig(provTestAllowedKind(), provTestProducer, provTestSelector())
	newSC.Spec.Provisioning.Resources = nil

	if errs := validateProvisioningPublishedImmutability(oldSC, newSC); len(errs) != 0 {
		t.Fatalf("expected withdrawing a declaration to be allowed, got %v", errs)
	}
}

// A declaration can go invalid without being edited: the platform can narrow
// the allowlist under it, and a document can be admitted while the webhook is
// absent. Removing the controller's finalizer is an update, so re-validating a
// terminating configuration would leave it undeletable.
func TestProvisioningValidationSkipsTerminatingConfiguration(t *testing.T) {
	oldSC := provTestConfig(provTestAllowedKind(), provTestProducer, provTestSelector())
	newSC := provTestConfig(servicesv1alpha1.GVKRef{Group: "iam.miloapis.com", Kind: "PolicyBinding"},
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
