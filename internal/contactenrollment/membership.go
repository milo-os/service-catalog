// SPDX-License-Identifier: AGPL-3.0-only

package contactenrollment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
)

// Outcome classifies the result of EnsureMembership for status reporting.
type Outcome string

const (
	// OutcomeEnrolled means the Contact is (now, or already was) a member of
	// the group.
	OutcomeEnrolled Outcome = "Enrolled"

	// OutcomeOptedOut means a ContactGroupMembershipRemoval already exists
	// for this (contact, group) pair — the contact previously opted out of
	// this group, and that opt-out is honored rather than overridden.
	OutcomeOptedOut Outcome = "OptedOut"

	// OutcomeFailed means membership creation failed for a reason that may
	// be transient, or one this package doesn't otherwise recognize, and
	// should be retried rather than treated as a final state.
	OutcomeFailed Outcome = "Failed"
)

// MembershipName derives a deterministic, DNS-safe ContactGroupMembership
// name from the (contact, group) pair it represents — not from the
// ServiceEntitlement or consumer project driving the enrollment. Two
// entitlements for the same service, in two different consumer projects,
// resolve to the same requester Contact and the same target group, and
// therefore to the same membership name: the second EnsureMembership call
// finds the first one's membership already there (or, in a race, has its
// create rejected by Milo's own duplicate check — see Classify), which is
// what makes "registers twice, ends up in the group once" hold without a
// list-then-create race of our own.
func MembershipName(contact *notificationv1alpha1.Contact, group *notificationv1alpha1.ContactGroup) string {
	sum := sha256.Sum256([]byte(contact.Namespace + "/" + contact.Name + "|" + group.Namespace + "/" + group.Name))
	return "cgm-" + hex.EncodeToString(sum[:8])
}

// EnsureMembership makes contact a member of group, tolerating "already a
// member" and honoring a pre-existing opt-out rather than overriding it.
//
// It checks for an opt-out first with hasOptedOut: Milo's own
// ContactGroupMembership webhook would reject the create anyway once a
// ContactGroupMembershipRemoval exists, but checking beforehand avoids a
// create attempt whose only possible outcome is rejection, and produces a
// clean OutcomeOptedOut without leaning on Classify for the common case.
// Classify still handles that rejection as a backstop, in case the opt-out
// is created in the window between the pre-check and the create.
func EnsureMembership(ctx context.Context, c client.Client, contact *notificationv1alpha1.Contact, group *notificationv1alpha1.ContactGroup) (Outcome, error) {
	optedOut, err := hasOptedOut(ctx, c, contact, group)
	if err != nil {
		return OutcomeFailed, err
	}
	if optedOut {
		return OutcomeOptedOut, nil
	}

	name := MembershipName(contact, group)

	existing := &notificationv1alpha1.ContactGroupMembership{}
	err = c.Get(ctx, client.ObjectKey{Namespace: contact.Namespace, Name: name}, existing)
	if err == nil {
		return OutcomeEnrolled, nil
	}
	if !apierrors.IsNotFound(err) {
		return OutcomeFailed, fmt.Errorf("get ContactGroupMembership %s/%s: %w", contact.Namespace, name, err)
	}

	membership := &notificationv1alpha1.ContactGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: contact.Namespace},
		Spec: notificationv1alpha1.ContactGroupMembershipSpec{
			ContactRef:      notificationv1alpha1.ContactReference{Name: contact.Name, Namespace: contact.Namespace},
			ContactGroupRef: notificationv1alpha1.ContactGroupReference{Name: group.Name, Namespace: group.Namespace},
		},
	}
	if err := c.Create(ctx, membership); err != nil {
		outcome := Classify(err)
		if outcome == OutcomeFailed {
			return outcome, fmt.Errorf("create ContactGroupMembership %s/%s: %w", contact.Namespace, name, err)
		}
		return outcome, nil
	}
	return OutcomeEnrolled, nil
}

// hasOptedOut reports whether a ContactGroupMembershipRemoval already exists
// for the (contact, group) pair. Removals are only selectable server-side by
// spec.contactRef.name and status.username (see
// ContactGroupMembershipRemoval's selectable-field markers) — not by group —
// so this lists by contact and filters client-side for the matching group
// reference.
func hasOptedOut(ctx context.Context, c client.Reader, contact *notificationv1alpha1.Contact, group *notificationv1alpha1.ContactGroup) (bool, error) {
	var removals notificationv1alpha1.ContactGroupMembershipRemovalList
	if err := c.List(ctx, &removals,
		client.InNamespace(contact.Namespace),
		client.MatchingFields{MembershipRemovalContactRefNameIndex: contact.Name},
	); err != nil {
		return false, fmt.Errorf("list ContactGroupMembershipRemovals for contact %q: %w", contact.Name, err)
	}
	for i := range removals.Items {
		ref := removals.Items[i].Spec.ContactGroupRef
		if ref.Name == group.Name && ref.Namespace == group.Namespace {
			return true, nil
		}
	}
	return false, nil
}

// Classify maps a ContactGroupMembership create error onto an Outcome. It is
// only meaningful for a non-nil error from EnsureMembership's own Create
// call — callers don't invoke it otherwise. It recognizes the two rejection
// shapes Milo's ContactGroupMembershipValidator.ValidateCreate produces
// (duplicate membership, and opt-out already recorded) by matching the
// literal text those checks emit, and falls back to the retryable
// OutcomeFailed for anything else — including an AlreadyExists our own
// pre-check should have caught, and any error whose shape it doesn't
// recognize — rather than guessing OutcomeEnrolled and hiding a real
// problem.
func Classify(err error) Outcome {
	if apierrors.IsAlreadyExists(err) {
		return OutcomeEnrolled
	}
	if !apierrors.IsInvalid(err) {
		return OutcomeFailed
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "already exists in ContactGroupMembership"):
		return OutcomeEnrolled
	case strings.Contains(msg, "ContactGroupMembershipRemoval") && strings.Contains(msg, "already exists"):
		return OutcomeOptedOut
	default:
		return OutcomeFailed
	}
}
