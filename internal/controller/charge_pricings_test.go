// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// TestPruneChargePricings: the pricings the retired charge fan-out made for
// this configuration are deleted; standalone pricings and another
// configuration's are kept.
func TestPruneChargePricings(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(scheme)
	_ = billingv1alpha1.AddToScheme(scheme)

	sc := &servicesv1alpha1.ServiceConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "compute", UID: "sc-compute"}}
	pricing := func(name string, owner types.UID, labels map[string]string) *billingv1alpha1.ServicePricing {
		sp := &billingv1alpha1.ServicePricing{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: billingv1alpha1.DefaultServicePricingNamespace,
			Labels:    labels,
		}}
		if owner != "" {
			sp.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: servicesv1alpha1.GroupVersion.String(),
				Kind:       "ServiceConfiguration",
				Name:       "owner",
				UID:        owner,
			}}
		}
		return sp
	}
	fanOut := map[string]string{labelManagedBy: labelManagedByValue}
	legacy := map[string]string{labelManagedBy: labelManagedByValueLegacy}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		pricing("compute-datumapis-com--instance-milli-cpu-seconds", sc.UID, fanOut),
		pricing("compute-datumapis-com--instance-memory-gib-seconds", sc.UID, legacy),
		pricing("networking-datumapis-com--gateway-requests", "sc-networking", fanOut),
		pricing("compute-vcpu-v1", "", nil),
	).Build()

	if err := pruneChargePricings(context.Background(), c, sc); err != nil {
		t.Fatalf("pruneChargePricings: %v", err)
	}

	for name, wantKept := range map[string]bool{
		"compute-datumapis-com--instance-milli-cpu-seconds":  false,
		"compute-datumapis-com--instance-memory-gib-seconds": false,
		"networking-datumapis-com--gateway-requests":         true,
		"compute-vcpu-v1": true,
	} {
		err := c.Get(context.Background(), client.ObjectKey{
			Namespace: billingv1alpha1.DefaultServicePricingNamespace, Name: name,
		}, &billingv1alpha1.ServicePricing{})
		switch {
		case wantKept && err != nil:
			t.Errorf("%s: want kept, got %v", name, err)
		case !wantKept && !apierrors.IsNotFound(err):
			t.Errorf("%s: want deleted, got err=%v", name, err)
		}
	}
}
