// SPDX-License-Identifier: AGPL-3.0-only

package provisioning

import (
	"errors"
	"testing"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestLookupAllowsListedKind(t *testing.T) {
	got, err := Lookup(servicesv1alpha1.GVKRef{Group: "networking.datumapis.com", Kind: "IPClass"})
	if err != nil {
		t.Fatalf("expected IPClass to be allowlisted, got %v", err)
	}
	if got.Version == "" || got.ReferenceSpec == nil {
		t.Fatalf("allowlist entry is incomplete: %+v", got)
	}
	spec := got.ReferenceSpec("platform-proj", "public-unicast")
	source, ok := spec["source"].(map[string]any)
	if !ok {
		t.Fatalf("expected a source reference, got %+v", spec)
	}
	if source["project"] != "platform-proj" || source["name"] != "public-unicast" {
		t.Errorf("reference does not name the source object: %+v", source)
	}
	// The projected object must carry a reference and nothing else; any extra
	// field would be data the consumer plane now holds a copy of.
	if len(spec) != 1 {
		t.Errorf("projected spec must contain only the source reference, got %+v", spec)
	}
}

func TestLookupRejectsUnlistedKind(t *testing.T) {
	_, err := Lookup(servicesv1alpha1.GVKRef{Group: "apps", Kind: "Deployment"})
	var notAllowed *ErrKindNotAllowed
	if !errors.As(err, &notAllowed) {
		t.Fatalf("expected ErrKindNotAllowed, got %v", err)
	}
}

// Kinds that grant access or cause execution must report as categorically
// denied, not merely absent, so the refusal cannot be read as a platform
// decision that has not been made yet.
func TestLookupDeniesPrivilegeGrantingKinds(t *testing.T) {
	for _, kind := range []servicesv1alpha1.GVKRef{
		{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"},
		{Group: "iam.miloapis.com", Kind: "PolicyBinding"},
		{Group: "", Kind: "Secret"},
		{Group: "", Kind: "ServiceAccount"},
		{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"},
		{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"},
	} {
		_, err := Lookup(kind)
		var denied *ErrKindDenied
		if !errors.As(err, &denied) {
			t.Errorf("kind %s.%s: expected ErrKindDenied, got %v", kind.Kind, kind.Group, err)
		}
	}
}

// The deny list is only meaningful if it is consulted before the allowlist.
// Adding an entry to the allowlist must not be able to re-admit a denied kind.
func TestAllowlistContainsNoDeniedKind(t *testing.T) {
	for _, a := range allowlist {
		for _, d := range deniedGroupKinds {
			if a.Group == d.Group && a.Kind == d.Kind {
				t.Fatalf("allowlist entry %s is on the categorical deny list", a.GroupKind())
			}
		}
	}
}

// Any allowlisted kind whose target API authorizes the reference itself must
// carry a consumer-facing explanation, because this version of provisioning
// does not establish that authorization and the gap has to be visible.
func TestAuthorizingKindsDeclareTheirCaveat(t *testing.T) {
	for _, a := range allowlist {
		if a.TargetAPIAuthorizesSource && a.AuthorizationCaveat == "" {
			t.Errorf("%s authorizes its source but declares no caveat", a.GroupKind())
		}
	}
}
