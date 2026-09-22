// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/contactenrollment"
)

const (
	contactEnrollmentTestNamespace = "milo-system"
	testRequesterName              = "user-abc123"
	testRequesterEmail             = "alice@example.com"
	testGroupName                  = "compute-testers"
)

func newContactEnabledService(name, canonical, providerProject, groupName string) *servicesv1alpha1.Service {
	svc := newPublishedService(name, canonical, providerProject, "")
	svc.Spec.ContactEnrollment = &servicesv1alpha1.ContactEnrollment{
		ContactGroupRef: servicesv1alpha1.ContactGroupRef{Name: groupName},
	}
	return svc
}

func testRequester() *servicesv1alpha1.RequesterRef {
	return &servicesv1alpha1.RequesterRef{
		APIGroup: "iam.miloapis.com",
		Kind:     "User",
		Name:     testRequesterName,
		Email:    testRequesterEmail,
	}
}

func newActiveCEEntitlement(name, serviceRef, canonicalServiceName string, requestedBy *servicesv1alpha1.RequesterRef) *servicesv1alpha1.ServiceEntitlement {
	ent := newEntitlement(name, serviceRef)
	ent.Status.Phase = servicesv1alpha1.EntitlementPhaseActive
	ent.Status.ServiceName = canonicalServiceName
	ent.Spec.RequestedBy = requestedBy
	return ent
}

func newTestContact(name, subjectName, email string) *notificationv1alpha1.Contact {
	c := &notificationv1alpha1.Contact{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: contactEnrollmentTestNamespace},
		Spec:       notificationv1alpha1.ContactSpec{Email: email},
	}
	if subjectName != "" {
		c.Spec.SubjectRef = &notificationv1alpha1.SubjectReference{
			APIGroup: "iam.miloapis.com", Kind: "User", Name: subjectName,
		}
	}
	return c
}

func newTestRemoval(name, contactName, groupName string) *notificationv1alpha1.ContactGroupMembershipRemoval {
	return &notificationv1alpha1.ContactGroupMembershipRemoval{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: contactEnrollmentTestNamespace},
		Spec: notificationv1alpha1.ContactGroupMembershipRemovalSpec{
			ContactRef:      notificationv1alpha1.ContactReference{Name: contactName, Namespace: contactEnrollmentTestNamespace},
			ContactGroupRef: notificationv1alpha1.ContactGroupReference{Name: groupName, Namespace: contactEnrollmentTestNamespace},
		},
	}
}

func contactEnrolledCondition(ent *servicesv1alpha1.ServiceEntitlement) *metav1.Condition {
	return apimeta.FindStatusCondition(ent.Status.Conditions, servicesv1alpha1.ConditionTypeContactEnrolled)
}

func getCEEntitlement(t *testing.T, c client.Client, name string) *servicesv1alpha1.ServiceEntitlement {
	t.Helper()
	var got servicesv1alpha1.ServiceEntitlement
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("get ServiceEntitlement %q: %v", name, err)
	}
	return &got
}

func TestContactEnrollmentReconciler_SkipsNonActiveEntitlement(t *testing.T) {
	svc := newContactEnabledService(testServiceSlug, testServiceName, testProviderProject, testGroupName)
	ent := newEntitlement(testServiceSlug, testServiceSlug)
	ent.Status.Phase = servicesv1alpha1.EntitlementPhasePendingApproval
	ent.Status.ServiceName = testServiceName

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)

	r := &ContactEnrollmentReconciler{rootClient: rootClient, Manager: mgr, ContactNamespace: contactEnrollmentTestNamespace}
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getCEEntitlement(t, consumerClient, testServiceSlug)
	if cond := contactEnrolledCondition(got); cond != nil {
		t.Errorf("ContactEnrolled condition = %+v, want none for a non-Active entitlement", cond)
	}
}

func TestContactEnrollmentReconciler_SkipsDeletedEntitlement(t *testing.T) {
	svc := newContactEnabledService(testServiceSlug, testServiceName, testProviderProject, testGroupName)
	now := metav1.NewTime(time.Now())
	ent := newActiveCEEntitlement(testServiceSlug, testServiceSlug, testServiceName, testRequester())
	ent.Finalizers = []string{serviceEntitlementFinalizer}
	ent.DeletionTimestamp = &now

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)

	r := &ContactEnrollmentReconciler{rootClient: rootClient, Manager: mgr, ContactNamespace: contactEnrollmentTestNamespace}
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getCEEntitlement(t, consumerClient, testServiceSlug)
	if cond := contactEnrolledCondition(got); cond != nil {
		t.Errorf("ContactEnrolled condition = %+v, want none for a deleted entitlement", cond)
	}
}

func TestContactEnrollmentReconciler_NotConfigured(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "") // no contactEnrollment
	ent := newActiveCEEntitlement(testServiceSlug, testServiceSlug, testServiceName, testRequester())

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)

	r := &ContactEnrollmentReconciler{rootClient: rootClient, Manager: mgr, ContactNamespace: contactEnrollmentTestNamespace}
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getCEEntitlement(t, consumerClient, testServiceSlug)
	cond := contactEnrolledCondition(got)
	if cond == nil {
		t.Fatal("expected a ContactEnrolled condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != servicesv1alpha1.ReasonContactEnrollmentNotConfigured {
		t.Errorf("condition = %+v, want True/%s", cond, servicesv1alpha1.ReasonContactEnrollmentNotConfigured)
	}
}

func TestContactEnrollmentReconciler_RequesterUnknown(t *testing.T) {
	svc := newContactEnabledService(testServiceSlug, testServiceName, testProviderProject, testGroupName)
	ent := newActiveCEEntitlement(testServiceSlug, testServiceSlug, testServiceName, nil)

	rootClient := newFakeClient(svc)
	consumerClient := newFakeClient(ent)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)

	r := &ContactEnrollmentReconciler{rootClient: rootClient, Manager: mgr, ContactNamespace: contactEnrollmentTestNamespace}
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getCEEntitlement(t, consumerClient, testServiceSlug)
	cond := contactEnrolledCondition(got)
	if cond == nil {
		t.Fatal("expected a ContactEnrolled condition")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != servicesv1alpha1.ReasonContactRequesterUnknown {
		t.Errorf("condition = %+v, want False/%s", cond, servicesv1alpha1.ReasonContactRequesterUnknown)
	}
}

func TestContactEnrollmentReconciler_ContactNotFoundRetries(t *testing.T) {
	svc := newContactEnabledService(testServiceSlug, testServiceName, testProviderProject, testGroupName)
	ent := newActiveCEEntitlement(testServiceSlug, testServiceSlug, testServiceName, testRequester())

	rootClient := newFakeClient(svc) // no matching Contact
	consumerClient := newFakeClient(ent)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)

	r := &ContactEnrollmentReconciler{rootClient: rootClient, Manager: mgr, ContactNamespace: contactEnrollmentTestNamespace}
	res, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want > 0 so the pending Contact is retried", res.RequeueAfter)
	}

	got := getCEEntitlement(t, consumerClient, testServiceSlug)
	cond := contactEnrolledCondition(got)
	if cond == nil {
		t.Fatal("expected a ContactEnrolled condition")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != servicesv1alpha1.ReasonContactNotFound {
		t.Errorf("condition = %+v, want False/%s", cond, servicesv1alpha1.ReasonContactNotFound)
	}
}

func TestContactEnrollmentReconciler_EnrollsContact(t *testing.T) {
	svc := newContactEnabledService(testServiceSlug, testServiceName, testProviderProject, testGroupName)
	ent := newActiveCEEntitlement(testServiceSlug, testServiceSlug, testServiceName, testRequester())
	contact := newTestContact("user-abc123-contact", testRequesterName, testRequesterEmail)

	rootClient := newFakeClient(svc, contact)
	consumerClient := newFakeClient(ent)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)

	r := &ContactEnrollmentReconciler{rootClient: rootClient, Manager: mgr, ContactNamespace: contactEnrollmentTestNamespace}
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getCEEntitlement(t, consumerClient, testServiceSlug)
	cond := contactEnrolledCondition(got)
	if cond == nil {
		t.Fatal("expected a ContactEnrolled condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != servicesv1alpha1.ReasonContactEnrolled {
		t.Errorf("condition = %+v, want True/%s", cond, servicesv1alpha1.ReasonContactEnrolled)
	}

	var group notificationv1alpha1.ContactGroup
	if err := rootClient.Get(context.Background(), types.NamespacedName{Name: testGroupName, Namespace: contactEnrollmentTestNamespace}, &group); err != nil {
		t.Fatalf("expected ContactGroup to be auto-created: %v", err)
	}
	if group.Spec.DisplayName != svc.Spec.DisplayName {
		t.Errorf("group displayName = %q, want fallback to service displayName %q", group.Spec.DisplayName, svc.Spec.DisplayName)
	}

	var memberships notificationv1alpha1.ContactGroupMembershipList
	if err := rootClient.List(context.Background(), &memberships, client.InNamespace(contactEnrollmentTestNamespace)); err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships.Items) != 1 {
		t.Fatalf("len(memberships) = %d, want 1", len(memberships.Items))
	}
	if want := contactenrollment.MembershipName(contact, &group); memberships.Items[0].Name != want {
		t.Errorf("membership name = %q, want %q", memberships.Items[0].Name, want)
	}
}

func TestContactEnrollmentReconciler_HonorsOptOut(t *testing.T) {
	svc := newContactEnabledService(testServiceSlug, testServiceName, testProviderProject, testGroupName)
	ent := newActiveCEEntitlement(testServiceSlug, testServiceSlug, testServiceName, testRequester())
	contact := newTestContact("user-abc123-contact", testRequesterName, testRequesterEmail)
	removal := newTestRemoval("removal-1", contact.Name, testGroupName)

	rootClient := newFakeClient(svc, contact, removal)
	consumerClient := newFakeClient(ent)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)

	r := &ContactEnrollmentReconciler{rootClient: rootClient, Manager: mgr, ContactNamespace: contactEnrollmentTestNamespace}
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getCEEntitlement(t, consumerClient, testServiceSlug)
	cond := contactEnrolledCondition(got)
	if cond == nil {
		t.Fatal("expected a ContactEnrolled condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != servicesv1alpha1.ReasonContactOptedOut {
		t.Errorf("condition = %+v, want True/%s", cond, servicesv1alpha1.ReasonContactOptedOut)
	}

	var memberships notificationv1alpha1.ContactGroupMembershipList
	if err := rootClient.List(context.Background(), &memberships, client.InNamespace(contactEnrollmentTestNamespace)); err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships.Items) != 0 {
		t.Fatalf("len(memberships) = %d, want 0 — opt-out must not be overridden", len(memberships.Items))
	}
}

// TestContactEnrollmentReconciler_TwoProjectsCollapseToOneMembership is the
// controller-level version of the same guarantee contactenrollment_test.go
// covers at the package level: the same requester registering for the same
// service in two different consumer projects ends up enrolled once, not
// twice.
func TestContactEnrollmentReconciler_TwoProjectsCollapseToOneMembership(t *testing.T) {
	const secondConsumerProject = "consumer-proj-2"

	svc := newContactEnabledService(testServiceSlug, testServiceName, testProviderProject, testGroupName)
	ent1 := newActiveCEEntitlement(testServiceSlug, testServiceSlug, testServiceName, testRequester())
	ent2 := newActiveCEEntitlement(testServiceSlug, testServiceSlug, testServiceName, testRequester())
	contact := newTestContact("user-abc123-contact", testRequesterName, testRequesterEmail)

	rootClient := newFakeClient(svc, contact)
	consumerClient1 := newFakeClient(ent1)
	consumerClient2 := newFakeClient(ent2)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient1)
	mgr.add(secondConsumerProject, consumerClient2)

	r := &ContactEnrollmentReconciler{rootClient: rootClient, Manager: mgr, ContactNamespace: contactEnrollmentTestNamespace}
	if _, err := r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("Reconcile() (project 1) error = %v", err)
	}
	if _, err := r.Reconcile(context.Background(), entitlementRequest(secondConsumerProject, testServiceSlug)); err != nil {
		t.Fatalf("Reconcile() (project 2) error = %v", err)
	}

	var memberships notificationv1alpha1.ContactGroupMembershipList
	if err := rootClient.List(context.Background(), &memberships, client.InNamespace(contactEnrollmentTestNamespace)); err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships.Items) != 1 {
		t.Fatalf("len(memberships) = %d, want exactly 1 across both projects", len(memberships.Items))
	}
}
