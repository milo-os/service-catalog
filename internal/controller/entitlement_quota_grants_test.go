// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

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
	// Try update first; if not found, create.
	if err := c.Client.Update(ctx, obj); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		return c.Client.Create(ctx, obj)
	}
	return nil
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
		if g.Labels[labelEntitlementManagedBy] != labelEntitlementManagedByValue {
			t.Errorf("grant %q label %q = %q, want %q", expectedName, labelEntitlementManagedBy, g.Labels[labelEntitlementManagedBy], labelEntitlementManagedByValue)
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
				labelEntitlementManagedBy: labelEntitlementManagedByValue,
				labelEntitlementName:      testServiceSlug,
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
