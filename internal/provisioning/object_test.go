// SPDX-License-Identifier: AGPL-3.0-only

package provisioning

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func raw(body string) runtime.RawExtension {
	return runtime.RawExtension{Raw: []byte(body)}
}

func ipClass() runtime.RawExtension {
	return raw(`{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind": "IPClass",
		"metadata": {"name": "tenant-endpoint-ipv6", "labels": {"tier": "premium"}},
		"spec": {"source": {"project": "platform-networking", "name": "tenant-endpoint-ipv6"}}
	}`)
}

func TestDecodeKeepsTheObjectTheProviderWrote(t *testing.T) {
	obj, err := Decode(ipClass())
	if err != nil {
		t.Fatalf("expected the object to decode, got %v", err)
	}
	if obj.GVK.Group != "ipam.miloapis.com" || obj.GVK.Version != "v1alpha1" || obj.GVK.Kind != "IPClass" {
		t.Fatalf("decoded the wrong kind: %v", obj.GVK)
	}
	if obj.Name != "tenant-endpoint-ipv6" {
		t.Fatalf("decoded the wrong name: %q", obj.Name)
	}
	if got := ListGVK(obj.GVK).Kind; got != "IPClassList" {
		t.Errorf("list kind is %q", got)
	}

	u := obj.Unstructured()
	source, found, err := unstructuredMap(u.Object, "spec", "source")
	if err != nil || !found {
		t.Fatalf("spec.source did not survive decoding: %v", u.Object)
	}
	if source["project"] != "platform-networking" || source["name"] != "tenant-endpoint-ipv6" {
		t.Errorf("spec.source was rewritten: %+v", source)
	}
	if u.GetLabels()["tier"] != "premium" {
		t.Errorf("declared labels were dropped: %+v", u.GetLabels())
	}
}

// Unstructured hands out a copy each time. Two consumer projects receiving one
// declaration must not share the map the platform then labels.
func TestUnstructuredIsIndependentPerCall(t *testing.T) {
	obj, err := Decode(ipClass())
	if err != nil {
		t.Fatalf("expected the object to decode, got %v", err)
	}
	first := obj.Unstructured()
	first.SetLabels(map[string]string{"only": "here"})

	if _, set := obj.Unstructured().GetLabels()["only"]; set {
		t.Error("labelling one copy reached the next")
	}
}

// The core group is unreachable by shape, not by a table naming its kinds. No
// declaration can produce a Secret, a ConfigMap, or a ServiceAccount.
func TestDecodeRejectsTheCoreGroup(t *testing.T) {
	for _, body := range []string{
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`,
		`{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"x"}}`,
	} {
		var invalid *ErrObjectInvalid
		if _, err := Decode(raw(body)); !errors.As(err, &invalid) || invalid.Field != "apiVersion" {
			t.Errorf("%s: expected apiVersion to be rejected, got %v", body, err)
		}
	}
}

func TestDecodeRejectsObjectsThePlatformWillNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"no apiVersion", `{"kind":"IPClass","metadata":{"name":"x"}}`, "apiVersion"},
		{"unserved version", `{"apiVersion":"ipam.miloapis.com/latest","kind":"IPClass","metadata":{"name":"x"}}`, "apiVersion"},
		{"no kind", `{"apiVersion":"ipam.miloapis.com/v1alpha1","metadata":{"name":"x"}}`, "kind"},
		{"lowercase kind", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"ipClass","metadata":{"name":"x"}}`, "kind"},
		{"no metadata", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass"}`, "metadata.name"},
		{"no name", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"labels":{}}}`, "metadata.name"},
		{"name is not a name", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"Not A Name"}}`, "metadata.name"},
		{"generated name", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","generateName":"x-"}}`, "metadata.generateName"},
		{"namespaced", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","namespace":"kube-system"}}`, "metadata.namespace"},
		{"owner reference", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","ownerReferences":[]}}`, "metadata.ownerReferences"},
		{"finalizer", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","finalizers":["keep.me/forever"]}}`, "metadata.finalizers"},
		{"status", `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x"},"status":{}}`, "status"},
		{"not an object", `["not","an","object"]`, "object"},
		{"empty", ``, "object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var invalid *ErrObjectInvalid
			if _, err := Decode(raw(tc.body)); !errors.As(err, &invalid) || invalid.Field != tc.field {
				t.Fatalf("expected %s to be rejected, got %v", tc.field, err)
			}
		})
	}
}

// A declaration is copied into every entitled project, so its size is paid per
// consumer.
func TestDecodeRejectsAnOversizedObject(t *testing.T) {
	body := `{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x"},` +
		`"spec":{"blob":"` + strings.Repeat("a", maxObjectBytes) + `"}}`

	var invalid *ErrObjectInvalid
	if _, err := Decode(raw(body)); !errors.As(err, &invalid) || invalid.Field != "object" {
		t.Fatalf("expected an oversized object to be rejected, got %v", err)
	}
}

func unstructuredMap(obj map[string]any, path ...string) (map[string]any, bool, error) {
	cur := obj
	for _, key := range path {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur = next
	}
	return cur, true, nil
}
