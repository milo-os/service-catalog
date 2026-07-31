// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ssaClient wraps a fake client and handles SSA Apply patches by converting
// them to Create-or-Update operations. The controller-runtime fake client does
// not support Apply patches; this shim bridges the gap for unit tests.
type ssaClient struct {
	client.Client
}

func (c *ssaClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if patch.Type() != types.ApplyPatchType {
		return c.Client.Patch(ctx, obj, patch, opts...)
	}
	// Decode the patch body into the object.
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, obj); err != nil {
		return err
	}
	// SSA is last-writer-wins and does not use optimistic concurrency. The
	// fake client's Update does, so copy the live object's resourceVersion onto
	// the decoded object before updating to avoid spurious conflicts when the
	// same object is re-applied across reconciles.
	existing := obj.DeepCopyObject().(client.Object)
	getErr := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if getErr != nil {
		if !apierrors.IsNotFound(getErr) {
			return getErr
		}
		return c.Create(ctx, obj)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

func newPublishedServiceConfiguration(scName, svcObjectName string, limits []servicesv1alpha1.QuotaLimitSpec) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: scName,
			UID:  types.UID("sc-uid-" + scName),
		},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: svcObjectName},
			Phase:      servicesv1alpha1.PhasePublished,
			Quota: &servicesv1alpha1.ServiceQuotaConfig{
				Limits: limits,
			},
		},
	}
}

// TestEnsureQuotaGrants_CreatesGrantsWhenActive verifies that reconciling an
// active entitlement with a Published ServiceConfiguration creates one
// ResourceGrant per quota limit in the consumer VCP.
func TestEnsureQuotaGrants_CreatesGrantsWhenActive(t *testing.T) {
	limits := []servicesv1alpha1.QuotaLimitSpec{
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
		{
			Name:         "cpu",
			Metric:       "compute.miloapis.com/cpu",
			DefaultLimit: 100,
			Unit:         "1/{project}",
			ConsumerType: servicesv1alpha1.QuotaConsumerType{
				APIGroup: "resourcemanager.miloapis.com",
				Kind:     "Project",
			},
		},
	}

	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	sc := newPublishedServiceConfiguration(testServiceSlug+"-config", testServiceSlug, limits)
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(svc, sc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	// Verify entitlement is Active.
	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("entitlement phase = %q, want Active", got.Status.Phase)
	}

	// Verify one ResourceGrant per limit was created in the consumer VCP.
	var grantList quotav1alpha1.ResourceGrantList
	if err := consumerClient.List(context.Background(), &grantList,
		client.InNamespace(quotaGrantNamespace),
		client.MatchingLabels{labelEntitlementName: testServiceSlug},
	); err != nil {
		t.Fatalf("list ResourceGrants: %v", err)
	}
	if len(grantList.Items) != len(limits) {
		t.Errorf("got %d ResourceGrants, want %d", len(grantList.Items), len(limits))
	}

	// Build a map of grants by name for easier lookup.
	grantByName := make(map[string]*quotav1alpha1.ResourceGrant, len(grantList.Items))
	for i := range grantList.Items {
		g := &grantList.Items[i]
		grantByName[g.Name] = g
	}

	for _, limit := range limits {
		expectedName := resourceGrantName(testServiceName, testConsumerProject, limit.Name)
		g, ok := grantByName[expectedName]
		if !ok {
			t.Errorf("ResourceGrant %q not found for limit %q", expectedName, limit.Name)
			continue
		}
		// Verify managed-by label.
		if g.Labels[labelManagedBy] != labelManagedByValue {
			t.Errorf("grant %q label %q = %q, want %q", expectedName, labelManagedBy, g.Labels[labelManagedBy], labelManagedByValue)
		}
		// Verify entitlement label.
		if g.Labels[labelEntitlementName] != testServiceSlug {
			t.Errorf("grant %q label %q = %q, want %q", expectedName, labelEntitlementName, g.Labels[labelEntitlementName], testServiceSlug)
		}
		// Verify consumerRef.
		if g.Spec.ConsumerRef.Name != testConsumerProject {
			t.Errorf("grant %q consumerRef.name = %q, want %q", expectedName, g.Spec.ConsumerRef.Name, testConsumerProject)
		}
		if g.Spec.ConsumerRef.Kind != limit.ConsumerType.Kind {
			t.Errorf("grant %q consumerRef.kind = %q, want %q", expectedName, g.Spec.ConsumerRef.Kind, limit.ConsumerType.Kind)
		}
		// Verify allowance.
		if len(g.Spec.Allowances) != 1 {
			t.Errorf("grant %q allowances count = %d, want 1", expectedName, len(g.Spec.Allowances))
			continue
		}
		if g.Spec.Allowances[0].ResourceType != limit.Metric {
			t.Errorf("grant %q allowance resourceType = %q, want %q", expectedName, g.Spec.Allowances[0].ResourceType, limit.Metric)
		}
		if len(g.Spec.Allowances[0].Buckets) != 1 || g.Spec.Allowances[0].Buckets[0].Amount != limit.DefaultLimit {
			t.Errorf("grant %q allowance bucket amount = %v, want %d", expectedName, g.Spec.Allowances[0].Buckets, limit.DefaultLimit)
		}
	}
}

// TestEnsureQuotaGrants_SkippedWhenNotActive verifies that no ResourceGrants
// are created when the entitlement is in PendingApproval state (gated service).
func TestEnsureQuotaGrants_SkippedWhenNotActive(t *testing.T) {
	limits := []servicesv1alpha1.QuotaLimitSpec{
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
	}

	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, servicesv1alpha1.EnablementModeGatedByProvider)
	sc := newPublishedServiceConfiguration(testServiceSlug+"-config", testServiceSlug, limits)
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(svc, sc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	// Entitlement should be PendingApproval — not Active.
	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.EntitlementPhasePendingApproval {
		t.Errorf("entitlement phase = %q, want PendingApproval", got.Status.Phase)
	}

	// No ResourceGrants should have been created.
	var grantList quotav1alpha1.ResourceGrantList
	if err := consumerClient.List(context.Background(), &grantList,
		client.InNamespace(quotaGrantNamespace),
		client.MatchingLabels{labelEntitlementName: testServiceSlug},
	); err != nil {
		t.Fatalf("list ResourceGrants: %v", err)
	}
	if len(grantList.Items) != 0 {
		t.Errorf("got %d ResourceGrants, want 0 (entitlement not yet Active)", len(grantList.Items))
	}
}

// TestPruneQuotaGrants_DeletesGrantsOnFinalization verifies that deleting an
// entitlement removes the ResourceGrants that were created for it.
func TestPruneQuotaGrants_DeletesGrantsOnFinalization(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	limits := []servicesv1alpha1.QuotaLimitSpec{
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
	}
	sc := newPublishedServiceConfiguration(testServiceSlug+"-config", testServiceSlug, limits)

	grantName := resourceGrantName(testServiceName, testConsumerProject, "instances")
	existingGrant := &quotav1alpha1.ResourceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      grantName,
			Namespace: quotaGrantNamespace,
			Labels: map[string]string{
				labelManagedBy:       labelManagedByValue,
				labelEntitlementName: testServiceSlug,
			},
		},
		Spec: quotav1alpha1.ResourceGrantSpec{
			ConsumerRef: quotav1alpha1.ConsumerRef{
				APIGroup: "resourcemanager.miloapis.com",
				Kind:     "Project",
				Name:     testConsumerProject,
			},
			Allowances: []quotav1alpha1.Allowance{
				{
					ResourceType: "compute.miloapis.com/instances",
					Buckets:      []quotav1alpha1.Bucket{{Amount: 10}},
				},
			},
		},
	}

	now := metav1.NewTime(time.Now())
	ent := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testServiceSlug,
			Finalizers:        []string{serviceEntitlementFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: testServiceSlug},
		},
	}

	rootClient := newFakeClient(svc, sc)
	consumerClient := newFakeClient(ent, existingGrant)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	req := mcreconcile.Request{}
	req.ClusterName = testConsumerProject
	req.Name = testServiceSlug
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	// ResourceGrant should be deleted.
	var got quotav1alpha1.ResourceGrant
	err := consumerClient.Get(context.Background(), types.NamespacedName{Name: grantName, Namespace: quotaGrantNamespace}, &got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected ResourceGrant %q to be deleted, got err=%v", grantName, err)
	}
}

// TestEnsureQuotaGrants_PrunesStaleGrant verifies that when a limit is dropped
// from the ServiceConfiguration, the previously-applied ResourceGrant for that
// limit is deleted on the next reconcile while the remaining grant survives.
func TestEnsureQuotaGrants_PrunesStaleGrant(t *testing.T) {
	limitL1 := servicesv1alpha1.QuotaLimitSpec{
		Name:         "instances",
		Metric:       "compute.miloapis.com/instances",
		DefaultLimit: 10,
		Unit:         "1/{project}",
		ConsumerType: servicesv1alpha1.QuotaConsumerType{
			APIGroup: "resourcemanager.miloapis.com",
			Kind:     "Project",
		},
	}
	limitL2 := servicesv1alpha1.QuotaLimitSpec{
		Name:         "cpu",
		Metric:       "compute.miloapis.com/cpu",
		DefaultLimit: 100,
		Unit:         "1/{project}",
		ConsumerType: servicesv1alpha1.QuotaConsumerType{
			APIGroup: "resourcemanager.miloapis.com",
			Kind:     "Project",
		},
	}

	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	sc := newPublishedServiceConfiguration(testServiceSlug+"-config", testServiceSlug, []servicesv1alpha1.QuotaLimitSpec{limitL1, limitL2})
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	rootClient := newFakeClient(svc, sc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	// First reconcile: both limits -> 2 grants.
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	var grants quotav1alpha1.ResourceGrantList
	if err := consumerClient.List(context.Background(), &grants,
		client.InNamespace(quotaGrantNamespace),
		client.MatchingLabels{labelEntitlementName: testServiceSlug},
	); err != nil {
		t.Fatalf("list ResourceGrants: %v", err)
	}
	if len(grants.Items) != 2 {
		t.Fatalf("after first reconcile got %d grants, want 2", len(grants.Items))
	}

	l1Name := resourceGrantName(testServiceName, testConsumerProject, limitL1.Name)
	l2Name := resourceGrantName(testServiceName, testConsumerProject, limitL2.Name)

	// Drop L2 from the configuration in the root cluster.
	var liveSC servicesv1alpha1.ServiceConfiguration
	if err := rootClient.Get(context.Background(), types.NamespacedName{Name: sc.Name}, &liveSC); err != nil {
		t.Fatalf("get ServiceConfiguration: %v", err)
	}
	liveSC.Spec.Quota.Limits = []servicesv1alpha1.QuotaLimitSpec{limitL1}
	if err := rootClient.Update(context.Background(), &liveSC); err != nil {
		t.Fatalf("update ServiceConfiguration: %v", err)
	}

	// Second reconcile: only L1 desired -> L2 grant pruned.
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	var afterL1 quotav1alpha1.ResourceGrant
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: l1Name, Namespace: quotaGrantNamespace}, &afterL1); err != nil {
		t.Errorf("expected L1 grant %q to remain, got err=%v", l1Name, err)
	}

	var afterL2 quotav1alpha1.ResourceGrant
	err := consumerClient.Get(context.Background(), types.NamespacedName{Name: l2Name, Namespace: quotaGrantNamespace}, &afterL2)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected L2 grant %q to be pruned, got err=%v", l2Name, err)
	}
}

// TestResourceGrantName verifies the naming scheme produces stable,
// prefix-correct names.
// TestOfferIncludesService covers the Offer snapshot membership check used
// by BillingEntitlement quota gating.
func TestOfferIncludesService(t *testing.T) {
	t.Parallel()

	offerWith := func(services ...string) *billingv1alpha1.Offer {
		snapshots := make([]billingv1alpha1.ServicePricingSnapshot, 0, len(services))
		for i, s := range services {
			snapshots = append(snapshots, billingv1alpha1.ServicePricingSnapshot{
				Name: fmt.Sprintf("pricing-%d", i),
				Spec: billingv1alpha1.ServicePricingSpec{
					ServiceRef: s,
					ChargeType: billingv1alpha1.ChargeTypeUsage,
					Currency:   "USD",
				},
			})
		}
		return &billingv1alpha1.Offer{
			Spec: billingv1alpha1.OfferSpec{ServicePricings: snapshots},
		}
	}

	tests := []struct {
		name        string
		offer       *billingv1alpha1.Offer
		serviceName string
		want        bool
	}{
		{
			name:        "nil offer",
			offer:       nil,
			serviceName: "compute.miloapis.com",
			want:        false,
		},
		{
			name:        "empty service name",
			offer:       offerWith("compute.miloapis.com"),
			serviceName: "",
			want:        false,
		},
		{
			name:        "empty snapshots",
			offer:       offerWith(),
			serviceName: "compute.miloapis.com",
			want:        false,
		},
		{
			name:        "service present",
			offer:       offerWith("compute.miloapis.com", "storage.miloapis.com"),
			serviceName: "compute.miloapis.com",
			want:        true,
		},
		{
			name:        "service absent",
			offer:       offerWith("storage.miloapis.com"),
			serviceName: "compute.miloapis.com",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := offerIncludesService(tt.offer, tt.serviceName); got != tt.want {
				t.Fatalf("offerIncludesService() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEnsureQuotaGrants_BillingEntitlementGating verifies that when
// ServiceConfiguration.spec.billing.quotaGating is BillingEntitlement,
// grants are issued only when the project's Offer snapshot covers the
// service, and are pruned otherwise.
func TestEnsureQuotaGrants_BillingEntitlementGating(t *testing.T) {
	limits := []servicesv1alpha1.QuotaLimitSpec{
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
	}

	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	sc := newPublishedServiceConfiguration(testServiceSlug+"-config", testServiceSlug, limits)
	sc.Spec.Billing = &servicesv1alpha1.ServiceBillingConfig{
		QuotaGating: servicesv1alpha1.QuotaGatingBillingEntitlement,
	}
	ent := newEntitlement(testServiceSlug, testServiceSlug)

	orgNS := "organization-acme"
	baName := "ba-acme"
	offerName := "offer-standard"

	binding := &billingv1alpha1.BillingAccountBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "binding-1", Namespace: orgNS},
		Spec: billingv1alpha1.BillingAccountBindingSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
			ProjectRef:        billingv1alpha1.ProjectRef{Name: testConsumerProject},
		},
		Status: billingv1alpha1.BillingAccountBindingStatus{
			Phase: billingv1alpha1.BillingAccountBindingPhaseActive,
		},
	}
	be := &billingv1alpha1.BillingEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: "be-default", Namespace: orgNS},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
			OfferRef:          billingv1alpha1.OfferReference{Name: offerName},
		},
		Status: billingv1alpha1.BillingEntitlementStatus{
			Conditions: []metav1.Condition{{
				Type:   ConditionTypeReady,
				Status: metav1.ConditionTrue,
				Reason: "BillingEntitlementReady",
			}},
		},
	}
	offerWithoutService := &billingv1alpha1.Offer{
		ObjectMeta: metav1.ObjectMeta{Name: offerName},
		Spec: billingv1alpha1.OfferSpec{
			LaunchStage: billingv1alpha1.OfferLaunchStageGA,
			ChargeTypes: []billingv1alpha1.ChargeType{billingv1alpha1.ChargeTypeUsage},
			ServicePricings: []billingv1alpha1.ServicePricingSnapshot{{
				Name: "other-pricing",
				Spec: billingv1alpha1.ServicePricingSpec{
					ServiceRef: "other.miloapis.com",
					ChargeType: billingv1alpha1.ChargeTypeUsage,
					Currency:   "USD",
				},
			}},
		},
	}

	rootClient := newFakeClient(svc, sc, binding, be, offerWithoutService)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	r := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}

	// Not entitled → no grants.
	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	var grantList quotav1alpha1.ResourceGrantList
	if err := consumerClient.List(context.Background(), &grantList,
		client.InNamespace(quotaGrantNamespace),
		client.MatchingLabels{labelEntitlementName: testServiceSlug},
	); err != nil {
		t.Fatalf("list ResourceGrants: %v", err)
	}
	if len(grantList.Items) != 0 {
		t.Fatalf("got %d ResourceGrants while service absent from Offer, want 0", len(grantList.Items))
	}

	// Add the service to the Offer snapshot → grants should appear.
	var liveOffer billingv1alpha1.Offer
	if err := rootClient.Get(context.Background(), types.NamespacedName{Name: offerName}, &liveOffer); err != nil {
		t.Fatalf("get Offer: %v", err)
	}
	liveOffer.Spec.ServicePricings = append(liveOffer.Spec.ServicePricings, billingv1alpha1.ServicePricingSnapshot{
		Name: "svc-pricing",
		Spec: billingv1alpha1.ServicePricingSpec{
			ServiceRef: testServiceName,
			ChargeType: billingv1alpha1.ChargeTypeUsage,
			Currency:   "USD",
		},
	})
	if err := rootClient.Update(context.Background(), &liveOffer); err != nil {
		t.Fatalf("update Offer: %v", err)
	}

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, testServiceSlug), 5)

	if err := consumerClient.List(context.Background(), &grantList,
		client.InNamespace(quotaGrantNamespace),
		client.MatchingLabels{labelEntitlementName: testServiceSlug},
	); err != nil {
		t.Fatalf("list ResourceGrants after entitle: %v", err)
	}
	if len(grantList.Items) != 1 {
		t.Fatalf("got %d ResourceGrants after service present on Offer, want 1", len(grantList.Items))
	}
}

// TestResourceGrantName verifies the naming scheme produces stable,
// prefix-correct names.
func TestResourceGrantName(t *testing.T) {
	n1 := resourceGrantName("compute.miloapis.com", "proj-a", "instances")
	n2 := resourceGrantName("compute.miloapis.com", "proj-a", "instances")
	if n1 != n2 {
		t.Errorf("resourceGrantName not stable: %q != %q", n1, n2)
	}
	if len(n1) < 3 || n1[:3] != "rg-" {
		t.Errorf("resourceGrantName %q does not start with 'rg-'", n1)
	}

	// Different inputs must produce different names.
	n3 := resourceGrantName("compute.miloapis.com", "proj-b", "instances")
	if n1 == n3 {
		t.Errorf("different consumerProject should produce different grant name")
	}
	n4 := resourceGrantName("compute.miloapis.com", "proj-a", "cpu")
	if n1 == n4 {
		t.Errorf("different limitName should produce different grant name")
	}
}
