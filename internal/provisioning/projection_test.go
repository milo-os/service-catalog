// SPDX-License-Identifier: AGPL-3.0-only

package provisioning

import (
	"errors"
	"testing"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func ipClassProjection() servicesv1alpha1.ResourceProjectionSpec {
	return servicesv1alpha1.ResourceProjectionSpec{
		SourceProject: "platform-proj",
		Kind: servicesv1alpha1.ProjectedKindRef{
			Group:   "ipam.miloapis.com",
			Version: "v1alpha1",
			Kind:    "IPClass",
		},
		Reference: servicesv1alpha1.ProjectedReferenceSpec{
			FieldPath:  "source",
			ProjectKey: "project",
			NameKey:    "name",
		},
	}
}

func TestResolveBuildsTheReferenceTheProviderDeclared(t *testing.T) {
	p, err := Resolve(ipClassProjection())
	if err != nil {
		t.Fatalf("expected a resolvable projection, got %v", err)
	}
	if p.GVK.Group != "ipam.miloapis.com" || p.GVK.Version != "v1alpha1" || p.GVK.Kind != "IPClass" {
		t.Fatalf("resolved the wrong kind: %v", p.GVK)
	}
	if got := p.ListGVK().Kind; got != "IPClassList" {
		t.Errorf("list kind is %q", got)
	}

	spec := p.Spec("platform-proj", "public-unicast")
	source, ok := spec["source"].(map[string]any)
	if !ok {
		t.Fatalf("expected a source reference, got %+v", spec)
	}
	if source["project"] != "platform-proj" || source["name"] != "public-unicast" {
		t.Errorf("reference does not name the source object: %+v", source)
	}
	// The written spec is the reference and nothing else. Any extra field would
	// be data the consumer plane now holds a copy of.
	if len(spec) != 1 || len(source) != 2 {
		t.Errorf("projected spec carries more than the reference: %+v", spec)
	}
}

func TestResolveNestsTheReferenceAtTheDeclaredPath(t *testing.T) {
	decl := ipClassProjection()
	decl.Reference.FieldPath = "ref.upstream"

	p, err := Resolve(decl)
	if err != nil {
		t.Fatalf("expected a nested path to resolve, got %v", err)
	}
	ref, _ := p.Spec("a", "b")["ref"].(map[string]any)
	upstream, ok := ref["upstream"].(map[string]any)
	if !ok || upstream["project"] != "a" || upstream["name"] != "b" {
		t.Errorf("reference was not written at the declared path: %+v", ref)
	}
}

// The core group is unreachable by shape, not by a table naming its kinds. No
// projection can produce a Secret or a ServiceAccount.
func TestResolveRejectsTheCoreGroup(t *testing.T) {
	decl := ipClassProjection()
	decl.Kind = servicesv1alpha1.ProjectedKindRef{Version: "v1", Kind: "Secret"}

	var invalid *ErrProjectionInvalid
	if _, err := Resolve(decl); !errors.As(err, &invalid) || invalid.Field != "kind.group" {
		t.Fatalf("expected kind.group to be rejected, got %v", err)
	}
}

// A provider chooses where the two values go, never what they are and never how
// many there are. Everything here would widen that.
func TestResolveRejectsReferencesThatAreNotTwoKeysAtOnePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ref   servicesv1alpha1.ProjectedReferenceSpec
		field string
	}{
		{"no path", servicesv1alpha1.ProjectedReferenceSpec{ProjectKey: "project", NameKey: "name"}, "reference.fieldPath"},
		{"list index", servicesv1alpha1.ProjectedReferenceSpec{FieldPath: "sources[0]", ProjectKey: "project", NameKey: "name"}, "reference.fieldPath"},
		{"escapes spec", servicesv1alpha1.ProjectedReferenceSpec{FieldPath: "../metadata", ProjectKey: "project", NameKey: "name"}, "reference.fieldPath"},
		{"empty segment", servicesv1alpha1.ProjectedReferenceSpec{FieldPath: "source..project", ProjectKey: "project", NameKey: "name"}, "reference.fieldPath"},
		{"no project key", servicesv1alpha1.ProjectedReferenceSpec{FieldPath: "source", NameKey: "name"}, "reference.projectKey"},
		{"one key for both", servicesv1alpha1.ProjectedReferenceSpec{FieldPath: "source", ProjectKey: "name", NameKey: "name"}, "reference.nameKey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decl := ipClassProjection()
			decl.Reference = tc.ref

			var invalid *ErrProjectionInvalid
			if _, err := Resolve(decl); !errors.As(err, &invalid) || invalid.Field != tc.field {
				t.Fatalf("expected %s to be rejected, got %v", tc.field, err)
			}
		})
	}
}

func TestResolveRequiresAServedVersion(t *testing.T) {
	for _, version := range []string{"", "v1alpha1/../v1", "1alpha1", "latest"} {
		decl := ipClassProjection()
		decl.Kind.Version = version

		var invalid *ErrProjectionInvalid
		if _, err := Resolve(decl); !errors.As(err, &invalid) || invalid.Field != "kind.version" {
			t.Errorf("version %q: expected kind.version to be rejected, got %v", version, err)
		}
	}
}
