// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	testOrgName              = "acme"
	testOrgServiceName       = "billing.miloapis.com"
	testOrgServiceConfigName = "billing-config"
	testOrgLimitName         = "billing-accounts-per-organization"
	testOrgMetricName        = "billing.miloapis.com/billingaccount/count"
)

func newOrganization(name string) *resourcemanagerv1alpha1.Organization {
	return &resourcemanagerv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func newOrgScopedLimit(name, metric string, amount int64) servicesv1alpha1.QuotaLimitSpec {
	return servicesv1alpha1.QuotaLimitSpec{
		Name:         name,
		Metric:       metric,
		DefaultLimit: amount,
		Unit:         "1/{organization}",
		ConsumerType: servicesv1alpha1.QuotaConsumerType{
			APIGroup: "resourcemanager.miloapis.com",
			Kind:     "Organization",
		},
	}
}

func newProjectScopedLimit(name, metric string, amount int64) servicesv1alpha1.QuotaLimitSpec {
	return servicesv1alpha1.QuotaLimitSpec{
		Name:         name,
		Metric:       metric,
		DefaultLimit: amount,
		Unit:         "1/{project}",
		ConsumerType: servicesv1alpha1.QuotaConsumerType{
			APIGroup: "resourcemanager.miloapis.com",
			Kind:     "Project",
		},
	}
}

func newReconciler(c client.Client) *OrganizationDefaultsReconciler {
	return &OrganizationDefaultsReconciler{
		Client: &ssaClient{Client: c},
		Scheme: testScheme(),
	}
}

func orgRequest(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

// TestOrganizationDefaults_CreatesGrantForOrgScopedLimit covers the
// primary path: a Published ServiceConfiguration with an
// Organization-scoped limit produces a ResourceGrant in the org's
// tenant namespace whose ConsumerRef points at the org and whose
// allowance amount matches defaultLimit.
func TestOrganizationDefaults_CreatesGrantForOrgScopedLimit(t *testing.T) {
	org := newOrganization(testOrgName)
	sc := newPublishedServiceConfiguration(
		testOrgServiceConfigName,
		testOrgServiceName,
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit(testOrgLimitName, testOrgMetricName, 1),
		},
	)
	c := newFakeClient(org, sc)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants,
		client.InNamespace(organizationNamespace(testOrgName)),
		client.MatchingLabels{labelOrgDefaults: "true"},
	); err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants.Items) != 1 {
		t.Fatalf("got %d grants, want 1", len(grants.Items))
	}

	g := &grants.Items[0]
	wantName := organizationDefaultGrantName(testOrgServiceName, testOrgName, testOrgLimitName)
	if g.Name != wantName {
		t.Errorf("grant name = %q, want %q", g.Name, wantName)
	}
	if g.Spec.ConsumerRef.Kind != orgConsumerKind {
		t.Errorf("consumerRef.kind = %q, want %q", g.Spec.ConsumerRef.Kind, orgConsumerKind)
	}
	if g.Spec.ConsumerRef.Name != testOrgName {
		t.Errorf("consumerRef.name = %q, want %q", g.Spec.ConsumerRef.Name, testOrgName)
	}
	if len(g.Spec.Allowances) != 1 ||
		g.Spec.Allowances[0].ResourceType != testOrgMetricName ||
		len(g.Spec.Allowances[0].Buckets) != 1 ||
		g.Spec.Allowances[0].Buckets[0].Amount != 1 {
		t.Errorf("allowance = %+v, want 1 of %q", g.Spec.Allowances, testOrgMetricName)
	}
	if g.Labels[labelManagedBy] != labelManagedByValue {
		t.Errorf("managed-by label = %q, want %q", g.Labels[labelManagedBy], labelManagedByValue)
	}
	if g.Labels[labelOwnerService] != testOrgServiceName {
		t.Errorf("owner-service label = %q, want %q", g.Labels[labelOwnerService], testOrgServiceName)
	}
}

// TestOrganizationDefaults_SkipsProjectScopedLimits guards the
// boundary between this reconciler and the existing per-project
// ensureQuotaGrants path. Project-typed limits must not produce
// org-scoped grants — that would emit a ResourceGrant whose
// consumerRef.kind says Project but whose consumerRef.name is an
// Organization name.
func TestOrganizationDefaults_SkipsProjectScopedLimits(t *testing.T) {
	org := newOrganization(testOrgName)
	sc := newPublishedServiceConfiguration(
		testOrgServiceConfigName,
		testOrgServiceName,
		[]servicesv1alpha1.QuotaLimitSpec{
			newProjectScopedLimit("projects", "resourcemanager.miloapis.com/projects", 10),
		},
	)
	c := newFakeClient(org, sc)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants.Items) != 0 {
		t.Errorf("got %d grants, want 0 (project-scoped limit must be ignored)", len(grants.Items))
	}
}

// TestOrganizationDefaults_SkipsDraftConfigurations verifies that
// Draft configurations are ignored. Without this, an in-progress
// configuration would leak provisional defaults into every org.
func TestOrganizationDefaults_SkipsDraftConfigurations(t *testing.T) {
	org := newOrganization(testOrgName)
	sc := newPublishedServiceConfiguration(
		testOrgServiceConfigName,
		testOrgServiceName,
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit(testOrgLimitName, testOrgMetricName, 1),
		},
	)
	sc.Spec.Phase = servicesv1alpha1.PhaseDraft
	c := newFakeClient(org, sc)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants.Items) != 0 {
		t.Errorf("got %d grants, want 0 (Draft must not materialise grants)", len(grants.Items))
	}
}

// TestOrganizationDefaults_PrunesGrantWhenLimitRemoved verifies that a
// previously-applied grant is deleted on the next reconcile when its
// underlying limit disappears from the ServiceConfiguration. This is
// the path that handles "we removed an org-scoped limit" or "we moved
// a ServiceConfiguration to Draft".
func TestOrganizationDefaults_PrunesGrantWhenLimitRemoved(t *testing.T) {
	org := newOrganization(testOrgName)
	sc := newPublishedServiceConfiguration(
		testOrgServiceConfigName,
		testOrgServiceName,
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit(testOrgLimitName, testOrgMetricName, 1),
		},
	)
	c := newFakeClient(org, sc)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// Drop the org-scoped limit and re-reconcile.
	sc.Spec.Quota.Limits = nil
	if err := c.Update(context.Background(), sc); err != nil {
		t.Fatalf("update SC: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants.Items) != 0 {
		t.Errorf("got %d grants after limit removal, want 0", len(grants.Items))
	}
}

// TestOrganizationDefaults_IdempotentReReconcile guards against
// duplicate grant creation on repeated reconciles. The grant name is
// derived deterministically from (serviceName, orgName, limitName),
// so SSA Apply should re-target the same object.
func TestOrganizationDefaults_IdempotentReReconcile(t *testing.T) {
	org := newOrganization(testOrgName)
	sc := newPublishedServiceConfiguration(
		testOrgServiceConfigName,
		testOrgServiceName,
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit(testOrgLimitName, testOrgMetricName, 1),
		},
	)
	c := newFakeClient(org, sc)
	r := newReconciler(c)

	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
			t.Fatalf("Reconcile iteration %d: %v", i, err)
		}
	}

	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants.Items) != 1 {
		t.Errorf("got %d grants after 3 reconciles, want 1 (idempotent)", len(grants.Items))
	}
}

// TestOrganizationDefaults_NoOpForDeletedOrg verifies that an
// Organization with a DeletionTimestamp set produces no grants. Once
// the org is going away, milo's resourcemanager controller will GC
// the tenant namespace; emitting fresh grants into a doomed namespace
// would race the cleanup.
func TestOrganizationDefaults_NoOpForDeletedOrg(t *testing.T) {
	now := metav1.Now()
	org := newOrganization(testOrgName)
	org.DeletionTimestamp = &now
	org.Finalizers = []string{"resourcemanager.miloapis.com/protection"}
	sc := newPublishedServiceConfiguration(
		testOrgServiceConfigName,
		testOrgServiceName,
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit(testOrgLimitName, testOrgMetricName, 1),
		},
	)
	c := newFakeClient(org, sc)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants.Items) != 0 {
		t.Errorf("got %d grants for deleting org, want 0", len(grants.Items))
	}
}

// TestOrganizationDefaults_MultipleConfigurations verifies that limits
// declared by separate ServiceConfigurations both produce grants
// against the same org without collision.
func TestOrganizationDefaults_MultipleConfigurations(t *testing.T) {
	org := newOrganization(testOrgName)
	billing := newPublishedServiceConfiguration(
		"billing-config",
		"billing.miloapis.com",
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit("billingaccounts", "billing.miloapis.com/billingaccount/count", 1),
		},
	)
	otherService := newPublishedServiceConfiguration(
		"other-config",
		"other.miloapis.com",
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit("widgets", "other.miloapis.com/widgets", 5),
		},
	)
	c := newFakeClient(org, billing, otherService)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants.Items) != 2 {
		t.Errorf("got %d grants, want 2 (one per Published org-scoped limit)", len(grants.Items))
	}
}

// TestOrganizationDefaults_DeprecatedPreservesExistingGrants verifies the
// Deprecated phase contract: "existing references continue to work." A
// SC that gains a grant on an org while Published, then moves to
// Deprecated, must keep its grant on the next reconcile — pruning would
// silently revoke quota from customers already on the service.
func TestOrganizationDefaults_DeprecatedPreservesExistingGrants(t *testing.T) {
	org := newOrganization(testOrgName)
	sc := newPublishedServiceConfiguration(
		testOrgServiceConfigName,
		testOrgServiceName,
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit(testOrgLimitName, testOrgMetricName, 1),
		},
	)
	c := newFakeClient(org, sc)
	r := newReconciler(c)

	// First reconcile while Published — grant lands.
	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants after publish: %v", err)
	}
	if len(grants.Items) != 1 {
		t.Fatalf("got %d grants after publish, want 1", len(grants.Items))
	}

	// Move SC to Deprecated and reconcile again — grant must survive.
	sc.Spec.Phase = servicesv1alpha1.PhaseDeprecated
	if err := c.Update(context.Background(), sc); err != nil {
		t.Fatalf("update SC to Deprecated: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("post-deprecation Reconcile: %v", err)
	}
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants after deprecation: %v", err)
	}
	if len(grants.Items) != 1 {
		t.Errorf("got %d grants after deprecation, want 1 (deprecated SC must preserve existing grants)", len(grants.Items))
	}
}

// TestOrganizationDefaults_DeprecatedDoesNotIssueNewGrants verifies that
// a SC that is Deprecated *before* any org has a grant for it does not
// produce new grants. Deprecated is "hidden from new onboarding" — orgs
// that did not already have the grant must not pick it up.
func TestOrganizationDefaults_DeprecatedDoesNotIssueNewGrants(t *testing.T) {
	org := newOrganization(testOrgName)
	sc := newPublishedServiceConfiguration(
		testOrgServiceConfigName,
		testOrgServiceName,
		[]servicesv1alpha1.QuotaLimitSpec{
			newOrgScopedLimit(testOrgLimitName, testOrgMetricName, 1),
		},
	)
	sc.Spec.Phase = servicesv1alpha1.PhaseDeprecated
	c := newFakeClient(org, sc)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), orgRequest(testOrgName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var grants quotav1alpha1.ResourceGrantList
	if err := c.List(context.Background(), &grants); err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants.Items) != 0 {
		t.Errorf("got %d grants for deprecated SC, want 0 (no new onboarding from deprecated)", len(grants.Items))
	}
}
