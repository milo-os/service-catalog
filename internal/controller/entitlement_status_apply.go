// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ServiceEntitlement status is written by several controllers at once while a
// service is being set up. Each writer server-side applies only the fields it
// owns under its own field manager, so their writes merge on the API server
// instead of racing on resourceVersion: conditions merge per type and
// provisionedResources per name.
//
// An apply must always carry a manager's complete set. Anything the manager
// owned and leaves out is removed, which is how a retired condition goes away.

// entitlementStatusOwner is one field manager's slice of ServiceEntitlement
// status: top-level status fields by JSON name, and conditions by type.
type entitlementStatusOwner struct {
	manager    string
	fields     []string
	conditions []string
}

var (
	// entitlementReadyOwner is the entitlement's authorization outcome. The
	// entitlement reconciler writes it, and the ServiceConsumer reconciler
	// writes the same fields through applyEntitlementReadyStatus when a
	// provider decides a gated request, so the decision lands without waiting
	// for the entitlement's next pass.
	entitlementReadyOwner = entitlementStatusOwner{
		manager:    "services-entitlement-status",
		fields:     []string{"phase", "origin", "dependencyOf", "entitledAt", "serviceName", "observedGeneration"},
		conditions: []string{ConditionTypeReady},
	}
	// dependencyOriginOwner is the parent's stamp on a dependency entitlement
	// it created. It shares origin and dependencyOf with entitlementReadyOwner;
	// both derive them from the dependency-of label, so the values agree.
	dependencyOriginOwner = entitlementStatusOwner{
		manager: "services-entitlement-dependency-origin",
		fields:  []string{"origin", "dependencyOf"},
	}
	dependenciesConditionOwner = entitlementStatusOwner{
		manager:    "service-entitlement-dependencies",
		conditions: []string{servicesv1alpha1.ConditionTypeDependenciesSatisfied},
	}
	suspendedConditionOwner = entitlementStatusOwner{
		manager:    suspendedConditionFieldManager,
		conditions: []string{ConditionTypeSuspended},
	}
	provisioningStatusOwner = entitlementStatusOwner{
		manager:    provisioningFieldManager,
		fields:     []string{"provisionedResources", "lastProvisioningEvaluation"},
		conditions: []string{servicesv1alpha1.ConditionTypeProvisioned},
	}
	setupStatusOwner = entitlementStatusOwner{
		manager:    "services-entitlement-setup",
		fields:     []string{"setup"},
		conditions: []string{servicesv1alpha1.ConditionTypeSetupComplete},
	}

	// entitlementStatusOwners decides who inherits each field when ownership
	// is migrated off the Update managers that wrote status before. Earlier
	// entries win a field two owners share.
	entitlementStatusOwners = []entitlementStatusOwner{
		entitlementReadyOwner,
		dependencyOriginOwner,
		dependenciesConditionOwner,
		suspendedConditionOwner,
		provisioningStatusOwner,
		setupStatusOwner,
	}
)

// project returns the owned part of status in its JSON form.
func (o entitlementStatusOwner) project(status *servicesv1alpha1.ServiceEntitlementStatus) (map[string]any, error) {
	all, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
	if err != nil {
		return nil, fmt.Errorf("convert ServiceEntitlement status: %w", err)
	}
	owned := map[string]any{}
	for _, field := range o.fields {
		if v, ok := all[field]; ok {
			owned[field] = v
		}
	}
	var conditions []any
	for _, c := range status.Conditions {
		if !slices.Contains(o.conditions, c.Type) {
			continue
		}
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if err != nil {
			return nil, fmt.Errorf("convert %s condition: %w", c.Type, err)
		}
		conditions = append(conditions, u)
	}
	if len(conditions) > 0 {
		owned["conditions"] = conditions
	}
	return owned, nil
}

// applyEntitlementStatus writes owner's fields from entitlement's in-memory
// status, unless they already match before. Callers mutate entitlement from
// the cached copy first, so condition transition times only move when a
// condition actually changed.
func applyEntitlementStatus(
	ctx context.Context,
	c client.Client,
	owner entitlementStatusOwner,
	before, entitlement *servicesv1alpha1.ServiceEntitlement,
) error {
	want, err := owner.project(&entitlement.Status)
	if err != nil {
		return err
	}
	had, err := owner.project(&before.Status)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(want, had) {
		return nil
	}

	if err := migrateEntitlementStatusOwnership(ctx, c, entitlement); err != nil {
		return err
	}

	u := &unstructured.Unstructured{Object: map[string]any{"status": want}}
	u.SetGroupVersionKind(servicesv1alpha1.GroupVersion.WithKind("ServiceEntitlement"))
	u.SetName(entitlement.Name)
	if err := c.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(owner.manager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply ServiceEntitlement %q status as %s: %w", entitlement.Name, owner.manager, err)
	}
	// entitlement already holds what was applied. Carry the new
	// resourceVersion so a locked write later in the same pass doesn't fail
	// on the version this apply just replaced.
	if rv := u.GetResourceVersion(); rv != "" {
		entitlement.ResourceVersion = rv
	}
	return nil
}

// migrateEntitlementStatusOwnership hands status fields still owned by an
// Update manager to the apply manager that owns them now.
//
// Before server-side apply, status was written with Update and merge patches.
// Those managers keep owning whatever they last wrote, and an applier cannot
// remove a field someone else still owns, so a condition its new owner retires
// would linger. Each field moves to exactly one new owner; fields outside the
// ownership table stay with their Update manager. This runs at most once per
// object: afterwards no Update manager owns a mapped status field.
func migrateEntitlementStatusOwnership(ctx context.Context, c client.Client, entitlement *servicesv1alpha1.ServiceEntitlement) error {
	migrated, changed, err := migratedStatusManagedFields(entitlement.ManagedFields)
	if err != nil || !changed {
		return err
	}
	before := entitlement.DeepCopy()
	entitlement.ManagedFields = migrated
	// Locked so a concurrent rewrite isn't overwritten with a stale list. A
	// conflict here happens once per object and simply retries the pass.
	if err := c.Patch(ctx, entitlement, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("migrate ServiceEntitlement %q status ownership: %w", entitlement.Name, err)
	}
	return nil
}

// migratedStatusManagedFields returns managedFields with mapped status fields
// moved from Update entries to the owning Apply entries, and whether anything
// moved.
func migratedStatusManagedFields(entries []metav1.ManagedFieldsEntry) ([]metav1.ManagedFieldsEntry, bool, error) {
	moved := map[string]*fieldpath.Set{}
	var apiVersion string
	out := make([]metav1.ManagedFieldsEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Operation != metav1.ManagedFieldsOperationUpdate || entry.Subresource != "status" || entry.FieldsV1 == nil {
			out = append(out, entry)
			continue
		}
		set := &fieldpath.Set{}
		if err := set.FromJSON(bytes.NewReader(entry.FieldsV1.Raw)); err != nil {
			return nil, false, fmt.Errorf("decode managed fields of %q: %w", entry.Manager, err)
		}
		kept := &fieldpath.Set{}
		for path := range set.All() {
			manager := statusPathOwner(path)
			if manager == "" {
				kept.Insert(path)
				continue
			}
			if moved[manager] == nil {
				moved[manager] = &fieldpath.Set{}
			}
			moved[manager].Insert(path)
			apiVersion = entry.APIVersion
		}
		if kept.Equals(set) {
			out = append(out, entry)
			continue
		}
		if keptOnlyContainers(kept) {
			continue
		}
		raw, err := kept.ToJSON()
		if err != nil {
			return nil, false, fmt.Errorf("encode managed fields of %q: %w", entry.Manager, err)
		}
		entry.FieldsV1 = &metav1.FieldsV1{Raw: raw}
		out = append(out, entry)
	}
	if len(moved) == 0 {
		return entries, false, nil
	}

	now := metav1.Now()
	for _, owner := range entitlementStatusOwners {
		set := moved[owner.manager]
		if set == nil {
			continue
		}
		i := slices.IndexFunc(out, func(e metav1.ManagedFieldsEntry) bool {
			return e.Manager == owner.manager && e.Operation == metav1.ManagedFieldsOperationApply && e.Subresource == "status"
		})
		if i < 0 {
			out = append(out, metav1.ManagedFieldsEntry{
				Manager:     owner.manager,
				Operation:   metav1.ManagedFieldsOperationApply,
				Subresource: "status",
				APIVersion:  apiVersion,
				Time:        &now,
				FieldsType:  "FieldsV1",
			})
			i = len(out) - 1
		} else if out[i].FieldsV1 != nil {
			existing := &fieldpath.Set{}
			if err := existing.FromJSON(bytes.NewReader(out[i].FieldsV1.Raw)); err != nil {
				return nil, false, fmt.Errorf("decode managed fields of %q: %w", owner.manager, err)
			}
			set = set.Union(existing)
		}
		raw, err := set.ToJSON()
		if err != nil {
			return nil, false, fmt.Errorf("encode managed fields of %q: %w", owner.manager, err)
		}
		out[i].FieldsV1 = &metav1.FieldsV1{Raw: raw}
	}
	return out, true, nil
}

// statusPathOwner names the apply manager that owns path, or "" when the
// ownership table doesn't cover it.
func statusPathOwner(path fieldpath.Path) string {
	if len(path) < 2 || path[0].FieldName == nil || *path[0].FieldName != "status" || path[1].FieldName == nil {
		return ""
	}
	field := *path[1].FieldName
	if field == "conditions" {
		if len(path) < 3 || path[2].Key == nil {
			return ""
		}
		for _, key := range *path[2].Key {
			if key.Name != "type" || !key.Value.IsString() {
				continue
			}
			for _, owner := range entitlementStatusOwners {
				if slices.Contains(owner.conditions, key.Value.AsString()) {
					return owner.manager
				}
			}
		}
		return ""
	}
	for _, owner := range entitlementStatusOwners {
		if slices.Contains(owner.fields, field) {
			return owner.manager
		}
	}
	return ""
}

// keptOnlyContainers reports whether a legacy entry is left owning nothing but
// the status and conditions containers themselves. Such an entry is dropped:
// it would keep no field alive, only clutter managedFields.
func keptOnlyContainers(kept *fieldpath.Set) bool {
	// Set.All doesn't support stopping early, so this walks every path.
	only := true
	kept.Iterate(func(path fieldpath.Path) {
		switch {
		case len(path) > 2:
			only = false
		case len(path) == 2 && (path[1].FieldName == nil || *path[1].FieldName != "conditions"):
			only = false
		}
	})
	return only
}
