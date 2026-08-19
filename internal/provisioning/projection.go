// SPDX-License-Identifier: AGPL-3.0-only

// Package provisioning resolves a service's projection declaration into the
// object the platform will write. The webhook and the controller both resolve
// through it, so the two cannot disagree about what a declaration means.
package provisioning

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// Projection is a resolved declaration: the kind to read and write, and where
// in the written object's spec the reference to the source object goes.
type Projection struct {
	GVK        schema.GroupVersionKind
	path       []string
	projectKey string
	nameKey    string
}

var (
	groupPattern    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)
	versionPattern  = regexp.MustCompile(`^v[0-9]+((alpha|beta)[0-9]*)?$`)
	kindPattern     = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	fieldKeyPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
)

// ErrProjectionInvalid reports a declaration that cannot be resolved into a
// write. It names the offending field so the message is actionable whether it
// surfaces at admission or on a consumer's entitlement.
type ErrProjectionInvalid struct {
	Field  string
	Detail string
}

func (e *ErrProjectionInvalid) Error() string {
	return fmt.Sprintf("projection.%s is invalid: %s", e.Field, e.Detail)
}

// Resolve turns a declaration into a Projection, or explains why it is not one.
//
// It repeats what the ServiceConfiguration schema already enforces. That is not
// redundant: the schema bounds what can be admitted today, and this bounds what
// gets written — including for a document admitted under an earlier schema and
// still sitting in etcd. RBAC bounds neither, because the operator reaches
// consumer control planes as an identity in system:masters.
func Resolve(spec servicesv1alpha1.ResourceProjectionSpec) (Projection, error) {
	// A dotted group, so the core group cannot be named. Secrets, service
	// accounts, and every other core kind are unreachable through a projection
	// as a matter of shape rather than of policy.
	if !groupPattern.MatchString(spec.Kind.Group) {
		return Projection{}, &ErrProjectionInvalid{
			Field:  "kind.group",
			Detail: fmt.Sprintf("%q is not a dotted API group", spec.Kind.Group),
		}
	}
	if !versionPattern.MatchString(spec.Kind.Version) {
		return Projection{}, &ErrProjectionInvalid{
			Field:  "kind.version",
			Detail: fmt.Sprintf("%q is not a served API version", spec.Kind.Version),
		}
	}
	if !kindPattern.MatchString(spec.Kind.Kind) {
		return Projection{}, &ErrProjectionInvalid{
			Field:  "kind.kind",
			Detail: fmt.Sprintf("%q is not a Kubernetes kind", spec.Kind.Kind),
		}
	}

	ref := spec.Reference
	if ref.FieldPath == "" {
		return Projection{}, &ErrProjectionInvalid{
			Field:  "reference.fieldPath",
			Detail: "must name the field in spec that holds the reference",
		}
	}
	path := strings.Split(ref.FieldPath, ".")
	for _, segment := range path {
		if !fieldKeyPattern.MatchString(segment) {
			return Projection{}, &ErrProjectionInvalid{
				Field:  "reference.fieldPath",
				Detail: fmt.Sprintf("%q is not a field name", segment),
			}
		}
	}
	for _, k := range []struct{ field, key string }{
		{"reference.projectKey", ref.ProjectKey},
		{"reference.nameKey", ref.NameKey},
	} {
		if !fieldKeyPattern.MatchString(k.key) {
			return Projection{}, &ErrProjectionInvalid{
				Field:  k.field,
				Detail: fmt.Sprintf("%q is not a field name", k.key),
			}
		}
	}
	if ref.ProjectKey == ref.NameKey {
		return Projection{}, &ErrProjectionInvalid{
			Field:  "reference.nameKey",
			Detail: "projectKey and nameKey must differ",
		}
	}

	return Projection{
		GVK:        schema.GroupVersionKind{Group: spec.Kind.Group, Version: spec.Kind.Version, Kind: spec.Kind.Kind},
		path:       path,
		projectKey: ref.ProjectKey,
		nameKey:    ref.NameKey,
	}, nil
}

// ListGVK is the list kind used to read the source objects and to sweep the
// installed ones.
func (p Projection) ListGVK() schema.GroupVersionKind {
	return ListGVK(p.GVK)
}

// ListGVK is the list kind for an object kind.
func ListGVK(gvk schema.GroupVersionKind) schema.GroupVersionKind {
	return gvk.GroupVersion().WithKind(gvk.Kind + "List")
}

// Spec builds the installed object's spec: one field, holding the source
// project and the source object's name, and nothing else.
//
// Both values come from the platform. A provider chooses the two keys and where
// they sit; it cannot add a third key, a value of its own, or any type other
// than a string. That is what keeps a projected object a pointer rather than a
// payload.
func (p Projection) Spec(sourceProject, sourceName string) map[string]any {
	ref := map[string]any{
		p.projectKey: sourceProject,
		p.nameKey:    sourceName,
	}
	for i := len(p.path) - 1; i >= 0; i-- {
		ref = map[string]any{p.path[i]: ref}
	}
	return ref
}

// KindRef renders the resolved kind for the entitlement ledger, so teardown can
// find what a withdrawn declaration installed.
func (p Projection) KindRef() *servicesv1alpha1.ProjectedKindRef {
	return &servicesv1alpha1.ProjectedKindRef{
		Group:   p.GVK.Group,
		Version: p.GVK.Version,
		Kind:    p.GVK.Kind,
	}
}
