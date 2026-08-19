// SPDX-License-Identifier: AGPL-3.0-only

// Package provisioning holds the platform-owned decisions about what a service
// may install into a consumer project. It is deliberately a separate package
// from both the webhook validation and the controller: the same table has to be
// enforced in both places, and a single source avoids the two drifting.
package provisioning

import (
	"fmt"
	"strings"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// AllowedKind is one entry in the platform allowlist: a kind a service may ask
// the platform to install into a consumer project, and the complete description
// of what the platform will write.
//
// Everything about the produced object other than which source objects are
// selected is fixed here, by the platform. A provider supplies a source project
// and a selector; it does not supply a version, a name, a spec, or a field to
// copy. That is what keeps the mechanism bounded, and it has to be kept
// deliberately, because nothing outside this code keeps it — the operator
// writes into consumer control planes with a certificate carrying the
// system:masters organization, so no RBAC grant stands behind this table.
type AllowedKind struct {
	// Group and Kind identify the source objects selected in the provider's
	// project, and the consumer-facing objects installed to reference them.
	Group string
	Kind  string

	// Version is the served API version the controller reads and writes. The
	// platform pins it so a provider's declaration does not have to name one.
	Version string

	// ReferenceSpec builds the consumer-side object's spec from the source
	// project and source object name. It is a function rather than a field-path
	// mapping because the produced object must be a reference and nothing else:
	// a declarative "copy these paths" mapping is a read primitive over
	// provider-controlled objects with platform-controlled destinations, and is
	// not needed by anything currently allowlisted.
	ReferenceSpec func(sourceProject, sourceName string) map[string]any

	// TargetAPIAuthorizesSource records that the target API performs its own
	// permission check on the reference at creation time, evaluated against
	// whoever creates the object.
	//
	// When true, this projection does not satisfy that check honestly: the
	// operator's identity passes it trivially and the consumer project never
	// holds the permission itself. See AuthorizationCaveat.
	TargetAPIAuthorizesSource bool

	// AuthorizationCaveat is the consumer-facing explanation of the above,
	// surfaced on the entitlement ledger so the gap is visible in a running
	// system rather than only in this source file.
	AuthorizationCaveat string
}

// GroupKind renders the entry as "Kind.group", the form used in messages.
func (a AllowedKind) GroupKind() string {
	return a.Kind + "." + a.Group
}

// allowlist is the complete set of kinds a ServiceConfiguration may declare for
// provisioning.
//
// It is static configuration in the platform repository rather than a
// cluster-scoped API object. Which of the two it should be is an unresolved
// question in the enhancement; static was chosen for the first version because
// it is the more auditable of the two and because an API object that only a
// platform admin may create is not meaningfully harder to change than a
// deployment, while being harder to review. Moving it to an API object later
// does not change the two enforcement points.
var allowlist = []AllowedKind{
	{
		Group:   "networking.datumapis.com",
		Kind:    "IPClass",
		Version: "v1alpha",
		// An IPClass in a consumer project whose spec names a class in another
		// project. Every other field must be empty; the IPAM API rejects chains
		// of references and treats the reference as immutable, so the consumer
		// holds a pointer at platform truth and no addressing of its own.
		ReferenceSpec: func(sourceProject, sourceName string) map[string]any {
			return map[string]any{
				"source": map[string]any{
					"project": sourceProject,
					"name":    sourceName,
				},
			}
		},
		TargetAPIAuthorizesSource: true,
		AuthorizationCaveat: "This reference was installed by the platform, which is authorized to " +
			"use the source class. The project itself was not granted use of it, so the reference " +
			"works on the platform's authority rather than the project's.",
	},
}

// deniedGroupKinds are excluded categorically, not by omission from the
// allowlist. These are the kinds whose installation converts "a provider can
// create an object" into "a provider can act as someone else, indefinitely,
// after the entitlement is gone".
//
// A positive allowlist already excludes them. This table exists so that adding
// an entry to the allowlist cannot quietly re-admit one: Lookup consults it
// first, and a unit test asserts no allowlist entry is denied.
var deniedGroupKinds = []struct{ Group, Kind string }{
	{"rbac.authorization.k8s.io", "Role"},
	{"rbac.authorization.k8s.io", "RoleBinding"},
	{"rbac.authorization.k8s.io", "ClusterRole"},
	{"rbac.authorization.k8s.io", "ClusterRoleBinding"},
	{"iam.miloapis.com", "PolicyBinding"},
	{"iam.miloapis.com", "Role"},
	{"", "ServiceAccount"},
	{"", "Secret"},
	{"admissionregistration.k8s.io", "ValidatingWebhookConfiguration"},
	{"admissionregistration.k8s.io", "MutatingWebhookConfiguration"},
	{"apiextensions.k8s.io", "CustomResourceDefinition"},
}

// ErrKindDenied is returned for a kind on the categorical deny list, separately
// from a kind that is merely absent from the allowlist, because the two need
// different answers: one is a request that will never be granted, the other is
// a request for a platform decision that has not been made.
type ErrKindDenied struct {
	GroupKind string
}

func (e *ErrKindDenied) Error() string {
	return fmt.Sprintf("kind %q may never be provisioned: kinds that grant access or cause "+
		"execution are excluded categorically, because installing one converts the ability to "+
		"create an object into the ability to act as someone else after the entitlement is gone",
		e.GroupKind)
}

// ErrKindNotAllowed is returned for a kind absent from the allowlist.
type ErrKindNotAllowed struct {
	GroupKind string
	Allowed   []string
}

func (e *ErrKindNotAllowed) Error() string {
	return fmt.Sprintf("kind %q is not in the platform provisioning allowlist (allowed: %s); "+
		"adding a kind is a platform decision, not a provider one",
		e.GroupKind, strings.Join(e.Allowed, ", "))
}

// Lookup resolves a declared kind against the platform allowlist.
//
// This is the single enforcement primitive. It is called at admission on the
// ServiceConfiguration, so a provider gets a synchronous error, and again in
// the controller before any write, so a bypassed, disabled, or stale admission
// path cannot widen what actually gets installed. The controller call is not
// redundant with the webhook: the webhook can be removed from the cluster, and
// a ServiceConfiguration admitted under an older, wider allowlist stays in
// etcd.
func Lookup(kind servicesv1alpha1.GVKRef) (AllowedKind, error) {
	gk := kind.Kind + "." + kind.Group
	if kind.Group == "" {
		gk = kind.Kind
	}

	for _, d := range deniedGroupKinds {
		if d.Group == kind.Group && d.Kind == kind.Kind {
			return AllowedKind{}, &ErrKindDenied{GroupKind: gk}
		}
	}

	for _, a := range allowlist {
		if a.Group == kind.Group && a.Kind == kind.Kind {
			return a, nil
		}
	}

	return AllowedKind{}, &ErrKindNotAllowed{GroupKind: gk, Allowed: AllowedGroupKinds()}
}

// All returns every allowlisted kind. The controller sweeps it at teardown so
// that removing a declaration, or removing a kind from the allowlist, still
// tears down what that kind previously installed.
func All() []AllowedKind {
	return append([]AllowedKind(nil), allowlist...)
}

// AllowedGroupKinds lists the allowlisted kinds, for error messages and tests.
func AllowedGroupKinds() []string {
	out := make([]string, 0, len(allowlist))
	for _, a := range allowlist {
		out = append(out, a.GroupKind())
	}
	return out
}
