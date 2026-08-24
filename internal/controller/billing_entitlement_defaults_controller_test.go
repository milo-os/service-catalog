// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func newBillingEntitlementDefaultsScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = billingv1alpha1.AddToScheme(s)
	return s
}

func TestBillingEntitlementDefaults_AppliesDefaultOffer(t *testing.T) {
	scheme := newBillingEntitlementDefaultsScheme()

	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: billingServiceCanonicalName,
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-config"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef:   servicesv1alpha1.ServiceReference{Name: billingSvc.Name},
			Phase:        servicesv1alpha1.PhasePublished,
			DefaultOffer: "default-pay-as-you-go-v1",
		},
	}
	ba := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acct-1",
			Namespace: "organization-acme",
			UID:       "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, sc, ba).Build()
	r := &BillingEntitlementDefaultsReconciler{Client: &ssaClient{Client: c}, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ba.Name, Namespace: ba.Namespace},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	wantName := "be-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-default"
	var be billingv1alpha1.BillingEntitlement
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ba.Namespace, Name: wantName}, &be); err != nil {
		t.Fatalf("get BillingEntitlement: %v", err)
	}
	if be.Spec.BillingAccountRef.Name != ba.Name {
		t.Errorf("billingAccountRef.name = %q, want %q", be.Spec.BillingAccountRef.Name, ba.Name)
	}
	if be.Spec.OfferRef.Name != "default-pay-as-you-go-v1" {
		t.Errorf("offerRef.name = %q, want default-pay-as-you-go-v1", be.Spec.OfferRef.Name)
	}
	if be.Labels[labelManagedBy] != labelManagedByValue {
		t.Errorf("managed-by = %q, want %q", be.Labels[labelManagedBy], labelManagedByValue)
	}
}

func TestBillingEntitlementDefaults_SkipsWhenDefaultOfferEmpty(t *testing.T) {
	scheme := newBillingEntitlementDefaultsScheme()

	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: billingServiceCanonicalName,
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-config"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: billingSvc.Name},
			Phase:      servicesv1alpha1.PhasePublished,
		},
	}
	ba := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acct-1",
			Namespace: "organization-acme",
			UID:       "11111111-2222-3333-4444-555555555555",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, sc, ba).Build()
	r := &BillingEntitlementDefaultsReconciler{Client: &ssaClient{Client: c}, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ba.Name, Namespace: ba.Namespace},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var list billingv1alpha1.BillingEntitlementList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("expected no BillingEntitlements, got %d", len(list.Items))
	}
}

func TestBillingEntitlementDefaults_DoesNotOverwriteExisting(t *testing.T) {
	scheme := newBillingEntitlementDefaultsScheme()

	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: billingServiceCanonicalName,
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-config"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef:   servicesv1alpha1.ServiceReference{Name: billingSvc.Name},
			Phase:        servicesv1alpha1.PhasePublished,
			DefaultOffer: "default-pay-as-you-go-v1",
		},
	}
	baUID := types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	ba := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acct-1",
			Namespace: "organization-acme",
			UID:       baUID,
		},
	}
	existing := &billingv1alpha1.BillingEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultBillingEntitlementName(baUID),
			Namespace: ba.Namespace,
		},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: ba.Name},
			OfferRef:          billingv1alpha1.OfferReference{Name: "staff-switched-offer"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, sc, ba, existing).Build()
	r := &BillingEntitlementDefaultsReconciler{Client: &ssaClient{Client: c}, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ba.Name, Namespace: ba.Namespace},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var be billingv1alpha1.BillingEntitlement
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ba.Namespace,
		Name:      defaultBillingEntitlementName(baUID),
	}, &be); err != nil {
		t.Fatalf("get: %v", err)
	}
	if be.Spec.OfferRef.Name != "staff-switched-offer" {
		t.Errorf("offerRef overwritten to %q; want staff-switched-offer preserved", be.Spec.OfferRef.Name)
	}
}

func TestBillingEntitlementDefaults_SkipsWhenStaffBEExistsUnderOtherName(t *testing.T) {
	scheme := newBillingEntitlementDefaultsScheme()

	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: billingServiceCanonicalName,
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-config"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef:   servicesv1alpha1.ServiceReference{Name: billingSvc.Name},
			Phase:        servicesv1alpha1.PhasePublished,
			DefaultOffer: "default-pay-as-you-go-v1",
		},
	}
	baUID := types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	ba := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acct-1",
			Namespace: "organization-acme",
			UID:       baUID,
		},
	}
	staffBE := &billingv1alpha1.BillingEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "staff-authored-entitlement",
			Namespace: ba.Namespace,
		},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: ba.Name},
			OfferRef:          billingv1alpha1.OfferReference{Name: "enterprise-v1"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, sc, ba, staffBE).Build()
	r := &BillingEntitlementDefaultsReconciler{Client: &ssaClient{Client: c}, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ba.Name, Namespace: ba.Namespace},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var list billingv1alpha1.BillingEntitlementList
	if err := c.List(context.Background(), &list, client.InNamespace(ba.Namespace)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected only staff BE, got %d", len(list.Items))
	}
	if list.Items[0].Name != "staff-authored-entitlement" {
		t.Errorf("unexpected BE %q", list.Items[0].Name)
	}
}

func TestBillingEntitlementDefaults_MigratesMatchingOfferAndClearsOneShot(t *testing.T) {
	scheme := newBillingEntitlementDefaultsScheme()

	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: billingServiceCanonicalName,
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-config"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef:       servicesv1alpha1.ServiceReference{Name: billingSvc.Name},
			Phase:            servicesv1alpha1.PhasePublished,
			DefaultOffer:     "payg-v2",
			MigrateFromOffer: "payg-v1",
		},
	}
	baUID := types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	ba := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acct-1",
			Namespace: "organization-acme",
			UID:       baUID,
		},
	}
	existing := &billingv1alpha1.BillingEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultBillingEntitlementName(baUID),
			Namespace: ba.Namespace,
		},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: ba.Name},
			OfferRef:          billingv1alpha1.OfferReference{Name: "payg-v1"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, sc, ba, existing).Build()
	r := &BillingEntitlementDefaultsReconciler{Client: &ssaClient{Client: c}, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ba.Name, Namespace: ba.Namespace},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var be billingv1alpha1.BillingEntitlement
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ba.Namespace,
		Name:      defaultBillingEntitlementName(baUID),
	}, &be); err != nil {
		t.Fatalf("get: %v", err)
	}
	if be.Spec.OfferRef.Name != "payg-v2" {
		t.Errorf("offerRef = %q, want payg-v2", be.Spec.OfferRef.Name)
	}

	var gotSC servicesv1alpha1.ServiceConfiguration
	if err := c.Get(context.Background(), client.ObjectKey{Name: sc.Name}, &gotSC); err != nil {
		t.Fatalf("get ServiceConfiguration: %v", err)
	}
	if gotSC.Spec.MigrateFromOffer != "" {
		t.Errorf("migrateFromOffer = %q, want cleared", gotSC.Spec.MigrateFromOffer)
	}
}

func TestBillingEntitlementDefaults_SkipsStaffSwitchedOfferAndClearsOneShot(t *testing.T) {
	scheme := newBillingEntitlementDefaultsScheme()

	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: billingServiceCanonicalName,
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-config"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef:       servicesv1alpha1.ServiceReference{Name: billingSvc.Name},
			Phase:            servicesv1alpha1.PhasePublished,
			DefaultOffer:     "payg-v2",
			MigrateFromOffer: "payg-v1",
		},
	}
	ba := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acct-1",
			Namespace: "organization-acme",
			UID:       "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		},
	}
	staffBE := &billingv1alpha1.BillingEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "staff-authored-entitlement",
			Namespace: ba.Namespace,
		},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: ba.Name},
			OfferRef:          billingv1alpha1.OfferReference{Name: "enterprise-v1"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, sc, ba, staffBE).Build()
	r := &BillingEntitlementDefaultsReconciler{Client: &ssaClient{Client: c}, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ba.Name, Namespace: ba.Namespace},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var be billingv1alpha1.BillingEntitlement
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ba.Namespace,
		Name:      staffBE.Name,
	}, &be); err != nil {
		t.Fatalf("get: %v", err)
	}
	if be.Spec.OfferRef.Name != "enterprise-v1" {
		t.Errorf("custom offerRef overwritten to %q", be.Spec.OfferRef.Name)
	}

	var gotSC servicesv1alpha1.ServiceConfiguration
	if err := c.Get(context.Background(), client.ObjectKey{Name: sc.Name}, &gotSC); err != nil {
		t.Fatalf("get ServiceConfiguration: %v", err)
	}
	if gotSC.Spec.MigrateFromOffer != "" {
		t.Errorf("migrateFromOffer = %q, want cleared when no accounts remain on previous default", gotSC.Spec.MigrateFromOffer)
	}
}

func TestBillingEntitlementDefaults_LeavesOneShotUntilLastMatchingAccount(t *testing.T) {
	scheme := newBillingEntitlementDefaultsScheme()

	billingSvc := &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-miloapis-com"},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: billingServiceCanonicalName,
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Billing",
		},
	}
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-config"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef:       servicesv1alpha1.ServiceReference{Name: billingSvc.Name},
			Phase:            servicesv1alpha1.PhasePublished,
			DefaultOffer:     "payg-v2",
			MigrateFromOffer: "payg-v1",
		},
	}
	ba1UID := types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	ba2UID := types.UID("bbbbbbbb-cccc-dddd-eeee-ffffffffffff")
	ba1 := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acct-1",
			Namespace: "organization-acme",
			UID:       ba1UID,
		},
	}
	ba2 := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acct-2",
			Namespace: "organization-beta",
			UID:       ba2UID,
		},
	}
	be1 := &billingv1alpha1.BillingEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultBillingEntitlementName(ba1UID),
			Namespace: ba1.Namespace,
		},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: ba1.Name},
			OfferRef:          billingv1alpha1.OfferReference{Name: "payg-v1"},
		},
	}
	be2 := &billingv1alpha1.BillingEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultBillingEntitlementName(ba2UID),
			Namespace: ba2.Namespace,
		},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: ba2.Name},
			OfferRef:          billingv1alpha1.OfferReference{Name: "payg-v1"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(billingSvc, sc, ba1, ba2, be1, be2).Build()
	r := &BillingEntitlementDefaultsReconciler{Client: &ssaClient{Client: c}, Scheme: scheme}

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ba1.Name, Namespace: ba1.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile acct-1: %v", err)
	}
	if result.RequeueAfter != migrateFromOfferRequeue {
		t.Errorf("RequeueAfter = %v, want %v while other accounts remain on previous default", result.RequeueAfter, migrateFromOfferRequeue)
	}

	var gotBE1 billingv1alpha1.BillingEntitlement
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ba1.Namespace,
		Name:      be1.Name,
	}, &gotBE1); err != nil {
		t.Fatalf("get be1: %v", err)
	}
	if gotBE1.Spec.OfferRef.Name != "payg-v2" {
		t.Errorf("be1 offerRef = %q, want payg-v2", gotBE1.Spec.OfferRef.Name)
	}

	var gotBE2 billingv1alpha1.BillingEntitlement
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ba2.Namespace,
		Name:      be2.Name,
	}, &gotBE2); err != nil {
		t.Fatalf("get be2: %v", err)
	}
	if gotBE2.Spec.OfferRef.Name != "payg-v1" {
		t.Errorf("be2 offerRef = %q, want still payg-v1", gotBE2.Spec.OfferRef.Name)
	}

	var gotSC servicesv1alpha1.ServiceConfiguration
	if err := c.Get(context.Background(), client.ObjectKey{Name: sc.Name}, &gotSC); err != nil {
		t.Fatalf("get ServiceConfiguration: %v", err)
	}
	if gotSC.Spec.MigrateFromOffer != "payg-v1" {
		t.Errorf("migrateFromOffer = %q, want payg-v1 until last matching account is migrated", gotSC.Spec.MigrateFromOffer)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ba2.Name, Namespace: ba2.Namespace},
	}); err != nil {
		t.Fatalf("Reconcile acct-2: %v", err)
	}

	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ba2.Namespace,
		Name:      be2.Name,
	}, &gotBE2); err != nil {
		t.Fatalf("get be2 after second reconcile: %v", err)
	}
	if gotBE2.Spec.OfferRef.Name != "payg-v2" {
		t.Errorf("be2 offerRef = %q, want payg-v2", gotBE2.Spec.OfferRef.Name)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: sc.Name}, &gotSC); err != nil {
		t.Fatalf("get ServiceConfiguration after second reconcile: %v", err)
	}
	if gotSC.Spec.MigrateFromOffer != "" {
		t.Errorf("migrateFromOffer = %q, want cleared after last matching account", gotSC.Spec.MigrateFromOffer)
	}
}
