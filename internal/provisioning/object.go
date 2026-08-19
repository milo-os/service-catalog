// SPDX-License-Identifier: AGPL-3.0-only

// Package provisioning decodes the objects a service embeds in its
// ServiceConfiguration into the writes the platform makes. The webhook and the
// controller both decode through it, so the two cannot disagree about what a
// declaration means.
package provisioning

import (
	"encoding/json"
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// maxObjectBytes caps one embedded object. A declaration is copied into every
// entitled project, so the cost of a large object is paid once per consumer.
const maxObjectBytes = 8192

var (
	groupPattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)
	versionPattern = regexp.MustCompile(`^v[0-9]+((alpha|beta)[0-9]*)?$`)
	kindPattern    = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
)

// Object is a decoded embedded object: the identity the platform writes it
// under, and the content it writes.
type Object struct {
	GVK  schema.GroupVersionKind
	Name string

	content map[string]any
}

// ErrObjectInvalid reports an embedded object the platform will not write. It
// names the offending field so the message is actionable whether it surfaces at
// admission or on a consumer's entitlement.
type ErrObjectInvalid struct {
	Field  string
	Detail string
}

func (e *ErrObjectInvalid) Error() string {
	return fmt.Sprintf("%s is invalid: %s", e.Field, e.Detail)
}

func invalid(field, format string, args ...any) error {
	return &ErrObjectInvalid{Field: field, Detail: fmt.Sprintf(format, args...)}
}

// Decode turns one embedded object into the write it stands for, or explains
// why it is not one.
//
// It repeats what the ServiceConfiguration schema already enforces, and adds
// what the schema cannot say. That is not redundant: the schema bounds what can
// be admitted today, and this bounds what gets written — including for a
// document admitted under an earlier schema and still sitting in etcd. RBAC
// bounds neither, because the operator reaches consumer control planes as an
// identity in system:masters.
func Decode(raw runtime.RawExtension) (Object, error) {
	if len(raw.Raw) == 0 {
		return Object{}, invalid("object", "must be a Kubernetes object")
	}
	if len(raw.Raw) > maxObjectBytes {
		return Object{}, invalid("object", "is %d bytes, above the limit of %d", len(raw.Raw), maxObjectBytes)
	}

	var content map[string]any
	if err := json.Unmarshal(raw.Raw, &content); err != nil {
		return Object{}, invalid("object", "is not a Kubernetes object: %v", err)
	}

	gvk, err := decodeGVK(content)
	if err != nil {
		return Object{}, err
	}

	name, meta, err := decodeMeta(content)
	if err != nil {
		return Object{}, err
	}

	if _, ok := content["status"]; ok {
		return Object{}, invalid("status", "is set by the API that owns the object, not by a declaration")
	}

	// Only the fields a declaration is entitled to state survive. Everything
	// the platform owns — the owner reference, the provisioning labels, the
	// object's place in a consumer plane — is applied at the write.
	installed := map[string]any{
		"apiVersion": gvk.GroupVersion().String(),
		"kind":       gvk.Kind,
		"metadata":   meta,
	}
	for k, v := range content {
		switch k {
		case "apiVersion", "kind", "metadata", "status":
		default:
			installed[k] = v
		}
	}

	return Object{GVK: gvk, Name: name, content: installed}, nil
}

// decodeGVK reads the object's identity.
//
// The group must be a dotted domain, so the core group cannot be named.
// Secrets, service accounts, config maps, and every other core kind are
// unreachable through a declaration as a matter of shape rather than of policy.
func decodeGVK(content map[string]any) (schema.GroupVersionKind, error) {
	apiVersion, _ := content["apiVersion"].(string)
	if apiVersion == "" {
		return schema.GroupVersionKind{}, invalid("apiVersion", "must name the group and version to write at")
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionKind{}, invalid("apiVersion", "%q is not a group and version", apiVersion)
	}
	if !groupPattern.MatchString(gv.Group) {
		return schema.GroupVersionKind{}, invalid("apiVersion", "%q is not a dotted API group", gv.Group)
	}
	if !versionPattern.MatchString(gv.Version) {
		return schema.GroupVersionKind{}, invalid("apiVersion", "%q is not a served API version", gv.Version)
	}

	kind, _ := content["kind"].(string)
	if !kindPattern.MatchString(kind) {
		return schema.GroupVersionKind{}, invalid("kind", "%q is not a Kubernetes kind", kind)
	}

	return gv.WithKind(kind), nil
}

// decodeMeta reads the metadata a declaration may state, and refuses the rest.
//
// A name is required because the platform no longer derives one. A namespace is
// refused because provisioning installs into a project's control plane and does
// not create namespaces there. Owner references and finalizers are refused
// because both defeat teardown: the platform sets the owner reference that
// makes deleting the entitlement reclaim the object.
func decodeMeta(content map[string]any) (string, map[string]any, error) {
	raw, ok := content["metadata"]
	if !ok {
		return "", nil, invalid("metadata.name", "must name the object to install")
	}
	meta, ok := raw.(map[string]any)
	if !ok {
		return "", nil, invalid("metadata", "must be an object")
	}

	for _, field := range []string{"namespace", "generateName", "ownerReferences", "finalizers"} {
		if _, set := meta[field]; set {
			return "", nil, invalid("metadata."+field, "can't be stated by a declaration")
		}
	}

	name, _ := meta["name"].(string)
	if name == "" {
		return "", nil, invalid("metadata.name", "must name the object to install")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", nil, invalid("metadata.name", "%q is not an object name: %s", name, errs[0])
	}

	installed := map[string]any{"name": name}
	for _, field := range []string{"labels", "annotations"} {
		if v, set := meta[field]; set {
			installed[field] = v
		}
	}
	return name, installed, nil
}

// Unstructured is the object to write, ready for the platform to add its labels
// and owner reference to.
func (o Object) Unstructured() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: runtime.DeepCopyJSON(o.content)}
}

// KindRef renders the object's kind for the entitlement ledger, so teardown can
// find what a withdrawn declaration installed.
func (o Object) KindRef() servicesv1alpha1.ProvisionedKindRef {
	return servicesv1alpha1.ProvisionedKindRef{
		Group:   o.GVK.Group,
		Version: o.GVK.Version,
		Kind:    o.GVK.Kind,
	}
}

// ListGVK is the list kind for an object kind, used to sweep installed objects.
func ListGVK(gvk schema.GroupVersionKind) schema.GroupVersionKind {
	return gvk.GroupVersion().WithKind(gvk.Kind + "List")
}
