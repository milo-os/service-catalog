// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const provTestService = "networking-datumapis-com"

func provTestObject(body string) servicesv1alpha1.ProvisionedObject {
	return servicesv1alpha1.ProvisionedObject{RawExtension: runtime.RawExtension{Raw: []byte(body)}}
}

// The shape IPAM gives a cross-project class reference. The provider writes it;
// the platform holds no table saying what it should be.
func provTestIPClass() servicesv1alpha1.ProvisionedObject {
	return provTestObject(`{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind": "IPClass",
		"metadata": {"name": "tenant-endpoint-ipv6"},
		"spec": {"source": {"project": "platform-networking", "name": "tenant-endpoint-ipv6"}}
	}`)
}

func provTestConfig(objects ...servicesv1alpha1.ProvisionedObject) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: provTestService},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: provTestService},
			Phase:      servicesv1alpha1.PhasePublished,
			Provisioning: &servicesv1alpha1.ServiceProvisioningConfig{
				Resources: []servicesv1alpha1.ProvisionedResourceSpec{{
					Name:    "address-classes",
					Objects: objects,
				}},
			},
		},
	}
}

func provTestServiceObj() *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: provTestService},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: "networking.datumapis.com",
			Owner: servicesv1alpha1.ServiceOwner{
				ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{Name: "networking-platform"},
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

func TestProvisioningValidationAcceptsAnEmbeddedObject(t *testing.T) {
	if errs := validateProvisioning(provTestConfig(provTestIPClass())); len(errs) != 0 {
		t.Fatalf("expected a well-formed object to be accepted, got %v", errs)
	}
}

// Admission is the first of two enforcement points: a provider gets a
// synchronous error rather than a status field later. What it checks is that
// the platform can write the object at all — whether the object is acceptable
// is the owning API's decision, made when it accepts or refuses the write.
func TestProvisioningValidationRejectsObjectsThePlatformWillNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		obj   servicesv1alpha1.ProvisionedObject
		field string
	}{
		{
			name:  "core group",
			obj:   provTestObject(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`),
			field: "spec.provisioning.resources[0].objects[0].apiVersion",
		},
		{
			name:  "no version",
			obj:   provTestObject(`{"apiVersion":"ipam.miloapis.com","kind":"IPClass","metadata":{"name":"x"}}`),
			field: "spec.provisioning.resources[0].objects[0].apiVersion",
		},
		{
			name:  "no name",
			obj:   provTestObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{}}`),
			field: "spec.provisioning.resources[0].objects[0].metadata.name",
		},
		{
			name:  "namespaced",
			obj:   provTestObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","namespace":"kube-system"}}`),
			field: "spec.provisioning.resources[0].objects[0].metadata.namespace",
		},
		{
			name:  "owner reference the platform must set",
			obj:   provTestObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","ownerReferences":[]}}`),
			field: "spec.provisioning.resources[0].objects[0].metadata.ownerReferences",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateProvisioning(provTestConfig(tc.obj))
			if len(errs) != 1 {
				t.Fatalf("expected one rejection, got %v", errs)
			}
			if got := errs[0].Field; got != tc.field {
				t.Errorf("error points at %q, want %q", got, tc.field)
			}
		})
	}
}

// The provider now chooses the installed name, so one declaration can contend
// with itself. The second write would silently win.
func TestProvisioningValidationRejectsTwoObjectsUnderOneName(t *testing.T) {
	errs := validateProvisioning(provTestConfig(provTestIPClass(), provTestIPClass()))
	if len(errs) != 1 {
		t.Fatalf("expected the duplicate object to be rejected, got %v", errs)
	}
	if got := errs[0].Field; got != "spec.provisioning.resources[0].objects[1]" {
		t.Errorf("error points at %q, want the second object", got)
	}
}

// Two objects of different kinds may share a name; they are different objects.
func TestProvisioningValidationAllowsOneNameAcrossKinds(t *testing.T) {
	other := provTestObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPPool","metadata":{"name":"tenant-endpoint-ipv6"}}`)
	if errs := validateProvisioning(provTestConfig(provTestIPClass(), other)); len(errs) != 0 {
		t.Fatalf("expected one name across two kinds to be accepted, got %v", errs)
	}
}

func TestProvisioningValidationRejectsDuplicateResourceNames(t *testing.T) {
	sc := provTestConfig(provTestIPClass())
	sc.Spec.Provisioning.Resources = append(sc.Spec.Provisioning.Resources,
		sc.Spec.Provisioning.Resources[0])

	errs := validateProvisioning(sc)
	if len(errs) != 1 {
		t.Fatalf("expected a duplicate declaration name to be rejected, got %v", errs)
	}
}

// Editing the object is how a provider updates what a consumer holds, so
// nothing inside a declaration is frozen on a Published configuration.
func TestProvisioningPublishedAllowsEditingTheObject(t *testing.T) {
	oldSC := provTestConfig(provTestIPClass())
	newSC := provTestConfig(provTestObject(`{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind": "IPClass",
		"metadata": {"name": "tenant-endpoint-ipv4"},
		"spec": {"source": {"project": "platform-networking", "name": "tenant-endpoint-ipv4"}}
	}`))

	errs := ValidateServiceConfigurationUpdate(context.Background(),
		provTestReader(provTestServiceObj()).Build(), oldSC, newSC, false)
	for _, e := range errs {
		if strings.Contains(e.Field, "provisioning") {
			t.Errorf("editing a Published declaration was blocked: %v", e)
		}
	}
}

// A declaration can go invalid without being edited: the schema can be narrowed
// under it, and a document can be admitted while the webhook is absent.
// Removing the controller's finalizer is an update, so re-validating a
// terminating configuration would leave it undeletable.
func TestProvisioningValidationSkipsTerminatingConfiguration(t *testing.T) {
	oldSC := provTestConfig(provTestIPClass())
	newSC := provTestConfig(provTestObject(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`))
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
