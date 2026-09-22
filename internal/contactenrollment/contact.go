// SPDX-License-Identifier: AGPL-3.0-only

package contactenrollment

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// Field-index keys ResolveContact and hasOptedOut list against. These name
// the same JSONPaths Milo's Contact and ContactGroupMembershipRemoval types
// declare as apiserver-side selectable fields, so a List with
// client.MatchingFields{key: value} is honored whether the reader talks to
// Milo's API server directly (the selectable-field marker handles it there)
// or reads from a manager cache with a matching local index registered —
// which the caller (ContactEnrollmentReconciler.SetupWithManager) must set
// up using these same key strings before any function in this package runs
// against a cache-backed client.
const (
	// ContactSubjectNameIndex looks up Contacts by spec.subject.name — the
	// primary lookup, keyed on the requester's User object name.
	ContactSubjectNameIndex = "spec.subject.name"

	// ContactEmailIndex looks up Contacts by spec.email — the fallback used
	// when a requester has no Contact matched by subject name.
	ContactEmailIndex = "spec.email"

	// MembershipRemovalContactRefNameIndex looks up
	// ContactGroupMembershipRemovals by spec.contactRef.name, to check for a
	// pre-existing opt-out before creating a membership.
	MembershipRemovalContactRefNameIndex = "spec.contactRef.name"
)

// ErrRequesterUnknown is returned by ResolveContact when the entitlement
// carries no requester (spec.requestedBy is nil) — an entitlement created
// before Phase 1 shipped, or by a caller the admission webhook didn't
// recognize as human. There is nothing to resolve a Contact from.
var ErrRequesterUnknown = errors.New("entitlement has no requestedBy; contact cannot be resolved")

// ErrContactNotFound is returned by ResolveContact when no Contact matches
// the requester, by subject name or, failing that, by email. It is not
// necessarily permanent: Milo's UserContactController may not have created
// the Contact yet, and a later reconcile (triggered by the Contact watch)
// resolves it once that happens.
var ErrContactNotFound = errors.New("no Contact found for requester")

// ResolveContact finds the notification.miloapis.com Contact for an
// entitlement's requester within namespace ns.
//
// It matches primarily by spec.subject.{kind,name} — the durable join key
// Milo's own UserContactController stamps onto a Contact it creates for a
// User, and the same field Milo's own ContactGroupMembership ownership
// checks compare against — and falls back to spec.email only when no
// subject-matched Contact exists, since a Contact's email can change
// independently of the underlying User's identity.
func ResolveContact(ctx context.Context, c client.Reader, ns string, requester *servicesv1alpha1.RequesterRef) (*notificationv1alpha1.Contact, error) {
	if requester == nil {
		return nil, ErrRequesterUnknown
	}

	if requester.Name != "" {
		var bySubject notificationv1alpha1.ContactList
		if err := c.List(ctx, &bySubject,
			client.InNamespace(ns),
			client.MatchingFields{ContactSubjectNameIndex: requester.Name},
		); err != nil {
			return nil, fmt.Errorf("list contacts by subject name %q: %w", requester.Name, err)
		}
		for i := range bySubject.Items {
			contact := &bySubject.Items[i]
			if contact.Spec.SubjectRef != nil &&
				contact.Spec.SubjectRef.Kind == "User" &&
				contact.Spec.SubjectRef.Name == requester.Name {
				return contact, nil
			}
		}
	}

	if requester.Email != "" {
		var byEmail notificationv1alpha1.ContactList
		if err := c.List(ctx, &byEmail,
			client.InNamespace(ns),
			client.MatchingFields{ContactEmailIndex: requester.Email},
		); err != nil {
			return nil, fmt.Errorf("list contacts by email %q: %w", requester.Email, err)
		}
		if len(byEmail.Items) > 0 {
			return &byEmail.Items[0], nil
		}
	}

	return nil, ErrContactNotFound
}
