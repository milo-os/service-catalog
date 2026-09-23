// SPDX-License-Identifier: AGPL-3.0-only

// Package contactenrollment implements the client-facing logic behind
// entitlement contact-group enrollment: resolving a requester's CRM Contact,
// ensuring the target ContactGroup exists, and enrolling the Contact into it.
//
// Every function here takes a client.Client/client.Reader directly rather
// than a reconciler or manager, so the package is unit-testable against a
// fake client and has nothing controller-specific to wire up. The
// controller that drives this package (ContactEnrollmentReconciler) decides
// when to call these functions and how to report the result on status; this
// package only knows how to talk to Milo's notification.miloapis.com API.
//
// See docs/enhancements/entitlement-contact-enrollment-plan.md for the
// design this package implements (Phase 3).
package contactenrollment
