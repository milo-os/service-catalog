// SPDX-License-Identifier: AGPL-3.0-only

package contactenrollment

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
)

var membershipGK = schema.GroupKind{Group: "notification.miloapis.com", Kind: "ContactGroupMembership"}

func newGroup(name string) *notificationv1alpha1.ContactGroup {
	return &notificationv1alpha1.ContactGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: notificationv1alpha1.ContactGroupSpec{
			DisplayName: "Compute Testers",
			Visibility:  notificationv1alpha1.ContactGroupVisibilityPublic,
		},
	}
}

func newRemoval(name, contactName, groupName string) *notificationv1alpha1.ContactGroupMembershipRemoval {
	return &notificationv1alpha1.ContactGroupMembershipRemoval{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: notificationv1alpha1.ContactGroupMembershipRemovalSpec{
			ContactRef:      notificationv1alpha1.ContactReference{Name: contactName, Namespace: testNamespace},
			ContactGroupRef: notificationv1alpha1.ContactGroupReference{Name: groupName, Namespace: testNamespace},
		},
	}
}

// invalidMembershipErr builds the same shape of error
// apierrors.NewInvalid(...) produces for a ContactGroupMembership rejection,
// with the given message substituted for the aggregated field errors — this
// is what a real StatusError.Error() renders as, so Classify sees exactly
// what it would against Milo's real webhook.
func invalidMembershipErr(name, message string) error {
	err := apierrors.NewInvalid(membershipGK, name, nil)
	err.ErrStatus.Message = fmt.Sprintf("%s %q is invalid: %s", membershipGK.String(), name, message)
	return err
}

func TestEnsureMembership_CreatesNewMembership(t *testing.T) {
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	group := newGroup("compute-testers")
	c := newFakeClient(t, contact, group)

	outcome, err := EnsureMembership(context.Background(), c, contact, group)
	if err != nil {
		t.Fatalf("EnsureMembership() error = %v", err)
	}
	if outcome != OutcomeEnrolled {
		t.Fatalf("EnsureMembership() outcome = %v, want %v", outcome, OutcomeEnrolled)
	}

	var memberships notificationv1alpha1.ContactGroupMembershipList
	if err := c.List(context.Background(), &memberships, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships.Items) != 1 {
		t.Fatalf("len(memberships) = %d, want 1", len(memberships.Items))
	}
	m := memberships.Items[0]
	if m.Name != MembershipName(contact, group) {
		t.Errorf("membership name = %q, want deterministic %q", m.Name, MembershipName(contact, group))
	}
	if m.Spec.ContactRef.Name != contact.Name || m.Spec.ContactGroupRef.Name != group.Name {
		t.Errorf("membership spec = %+v, want refs to %q/%q", m.Spec, contact.Name, group.Name)
	}
}

func TestEnsureMembership_AlreadyMember(t *testing.T) {
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	group := newGroup("compute-testers")
	existing := &notificationv1alpha1.ContactGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Name: MembershipName(contact, group), Namespace: testNamespace},
		Spec: notificationv1alpha1.ContactGroupMembershipSpec{
			ContactRef:      notificationv1alpha1.ContactReference{Name: contact.Name, Namespace: testNamespace},
			ContactGroupRef: notificationv1alpha1.ContactGroupReference{Name: group.Name, Namespace: testNamespace},
		},
	}
	c := newFakeClient(t, contact, group, existing)

	outcome, err := EnsureMembership(context.Background(), c, contact, group)
	if err != nil {
		t.Fatalf("EnsureMembership() error = %v", err)
	}
	if outcome != OutcomeEnrolled {
		t.Fatalf("EnsureMembership() outcome = %v, want %v", outcome, OutcomeEnrolled)
	}

	var memberships notificationv1alpha1.ContactGroupMembershipList
	if err := c.List(context.Background(), &memberships, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships.Items) != 1 {
		t.Fatalf("len(memberships) = %d, want exactly 1 (no duplicate create)", len(memberships.Items))
	}
}

func TestEnsureMembership_TwoEntitlementsSameContactAndGroupCollapseToOneMembership(t *testing.T) {
	// Simulates the same requester registering for the same service across
	// two consumer projects: both reconciles resolve to the same Contact and
	// the same ContactGroup, so both must derive the same membership name.
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	group := newGroup("compute-testers")
	c := newFakeClient(t, contact, group)

	first, err := EnsureMembership(context.Background(), c, contact, group)
	if err != nil {
		t.Fatalf("first EnsureMembership() error = %v", err)
	}
	second, err := EnsureMembership(context.Background(), c, contact, group)
	if err != nil {
		t.Fatalf("second EnsureMembership() error = %v", err)
	}
	if first != OutcomeEnrolled || second != OutcomeEnrolled {
		t.Fatalf("outcomes = %v, %v, want both %v", first, second, OutcomeEnrolled)
	}

	var memberships notificationv1alpha1.ContactGroupMembershipList
	if err := c.List(context.Background(), &memberships, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships.Items) != 1 {
		t.Fatalf("len(memberships) = %d, want exactly 1", len(memberships.Items))
	}
}

func TestEnsureMembership_HonorsExistingOptOut(t *testing.T) {
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	group := newGroup("compute-testers")
	removal := newRemoval("removal-1", contact.Name, group.Name)
	c := newFakeClient(t, contact, group, removal)

	outcome, err := EnsureMembership(context.Background(), c, contact, group)
	if err != nil {
		t.Fatalf("EnsureMembership() error = %v", err)
	}
	if outcome != OutcomeOptedOut {
		t.Fatalf("EnsureMembership() outcome = %v, want %v", outcome, OutcomeOptedOut)
	}

	var memberships notificationv1alpha1.ContactGroupMembershipList
	if err := c.List(context.Background(), &memberships, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships.Items) != 0 {
		t.Fatalf("len(memberships) = %d, want 0 — opt-out must not be overridden", len(memberships.Items))
	}
}

func TestEnsureMembership_OptOutForDifferentGroupIsIgnored(t *testing.T) {
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	group := newGroup("compute-testers")
	otherGroupRemoval := newRemoval("removal-1", contact.Name, "some-other-group")
	c := newFakeClient(t, contact, group, otherGroupRemoval)

	outcome, err := EnsureMembership(context.Background(), c, contact, group)
	if err != nil {
		t.Fatalf("EnsureMembership() error = %v", err)
	}
	if outcome != OutcomeEnrolled {
		t.Fatalf("EnsureMembership() outcome = %v, want %v (opt-out is for a different group)", outcome, OutcomeEnrolled)
	}
}

func TestEnsureMembership_ClassifiesRejectionAsBackstop(t *testing.T) {
	// Simulates the race the pre-check can't close: Milo's webhook rejects
	// the create as a duplicate even though our own Get (against a stale
	// cache) reported nothing there yet.
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	group := newGroup("compute-testers")
	base := newFakeClient(t, contact, group)

	name := MembershipName(contact, group)
	rejection := invalidMembershipErr(name, fmt.Sprintf(
		`spec: Duplicate value: "membership already exists in ContactGroupMembership %s"`, name,
	))

	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*notificationv1alpha1.ContactGroupMembership); ok {
				return rejection
			}
			return cl.Create(ctx, obj, opts...)
		},
	})

	outcome, err := EnsureMembership(context.Background(), c, contact, group)
	if err != nil {
		t.Fatalf("EnsureMembership() error = %v", err)
	}
	if outcome != OutcomeEnrolled {
		t.Fatalf("EnsureMembership() outcome = %v, want %v", outcome, OutcomeEnrolled)
	}
}

func TestEnsureMembership_ClassifiesOptOutRejectionAsBackstop(t *testing.T) {
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	group := newGroup("compute-testers")
	base := newFakeClient(t, contact, group)

	name := MembershipName(contact, group)
	rejection := invalidMembershipErr(name,
		"spec: Invalid value: cannot create membership as a ContactGroupMembershipRemoval removal-1 already exists")

	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*notificationv1alpha1.ContactGroupMembership); ok {
				return rejection
			}
			return cl.Create(ctx, obj, opts...)
		},
	})

	outcome, err := EnsureMembership(context.Background(), c, contact, group)
	if err != nil {
		t.Fatalf("EnsureMembership() error = %v", err)
	}
	if outcome != OutcomeOptedOut {
		t.Fatalf("EnsureMembership() outcome = %v, want %v", outcome, OutcomeOptedOut)
	}
}

func TestEnsureMembership_UnrecognizedCreateErrorIsRetryable(t *testing.T) {
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	group := newGroup("compute-testers")
	base := newFakeClient(t, contact, group)

	boom := apierrors.NewServiceUnavailable("etcd unavailable")
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*notificationv1alpha1.ContactGroupMembership); ok {
				return boom
			}
			return cl.Create(ctx, obj, opts...)
		},
	})

	outcome, err := EnsureMembership(context.Background(), c, contact, group)
	if err == nil {
		t.Fatal("EnsureMembership() expected an error for an unrecognized create failure")
	}
	if outcome != OutcomeFailed {
		t.Fatalf("EnsureMembership() outcome = %v, want %v", outcome, OutcomeFailed)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Outcome
	}{
		{
			name: "already exists",
			err:  apierrors.NewAlreadyExists(schema.GroupResource{Group: "notification.miloapis.com", Resource: "contactgroupmemberships"}, "cgm-abc"),
			want: OutcomeEnrolled,
		},
		{
			name: "duplicate membership rejection",
			err: invalidMembershipErr("cgm-abc",
				`spec: Duplicate value: "membership already exists in ContactGroupMembership cgm-abc"`),
			want: OutcomeEnrolled,
		},
		{
			name: "opt-out already recorded rejection",
			err: invalidMembershipErr("cgm-abc",
				"spec: Invalid value: cannot create membership as a ContactGroupMembershipRemoval removal-1 already exists"),
			want: OutcomeOptedOut,
		},
		{
			name: "unrelated invalid error",
			err: invalidMembershipErr("cgm-abc",
				"spec.contactRef.name: Not found: \"missing-contact\""),
			want: OutcomeFailed,
		},
		{
			name: "generic error",
			err:  apierrors.NewServiceUnavailable("boom"),
			want: OutcomeFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify() = %v, want %v", got, tt.want)
			}
		})
	}
}
