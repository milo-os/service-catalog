// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// quotaGrantNamespace is the namespace where ResourceGrants live in project VCPs.
const quotaGrantNamespace = "milo-system"

// projectConsumerKind is the only ConsumerType.Kind that per-project quota
// grants support. Each grant's ConsumerRef.Name is the consumer project, so a
// limit whose ConsumerType.Kind is anything other than a Project would produce
// a ResourceGrant whose ref kind and name disagree. Such limits are skipped.
const projectConsumerKind = "Project"

const (
	// labelEntitlementName records which ServiceEntitlement owns the grant.
	// Combined with the shared managed-by label (app.kubernetes.io/managed-by =
	// services.miloapis.com) it scopes grants for pruning. ResourceGrant
	// provisioning is new, so there are no legacy-labeled grants to migrate.
	labelEntitlementName = "services.miloapis.com/entitlement"

	// quotaGrantFieldManager is the SSA field manager used when patching
	// ResourceGrant objects into consumer VCPs.
	quotaGrantFieldManager = "service-catalog"
)

// ensureQuotaGrants creates or updates one ResourceGrant per quota limit
// declared by the latest Published ServiceConfiguration for svc in the root
// cluster. Grants are written into the consumer project VCP via consumerClient.
func (r *ServiceEntitlementReconciler) ensureQuotaGrants(
	ctx context.Context,
	consumerClient client.Client,
	consumerProject string,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	svc *servicesv1alpha1.Service,
) error {
	// Find the Published ServiceConfiguration for this Service.
	var scList servicesv1alpha1.ServiceConfigurationList
	if err := r.rootClient.List(ctx, &scList,
		client.MatchingFields{"spec.serviceRef.name": svc.Name},
	); err != nil {
		return fmt.Errorf("list ServiceConfigurations for service %q: %w", svc.Name, err)
	}

	// Pick the first Published one (there is typically at most one).
	var sc *servicesv1alpha1.ServiceConfiguration
	for i := range scList.Items {
		if scList.Items[i].Spec.Phase == servicesv1alpha1.PhasePublished {
			sc = &scList.Items[i]
			break
		}
	}
	if sc == nil || sc.Spec.Quota == nil {
		// No published configuration with quota limits. This is a genuine
		// zero-limit configuration (the lookup succeeded), so it is safe to
		// prune any previously-applied grants for this entitlement.
		return r.pruneStaleQuotaGrants(ctx, consumerClient, entitlement, map[string]struct{}{})
	}

	logger := log.FromContext(ctx)
	desired := make(map[string]struct{}, len(sc.Spec.Quota.Limits))
	for i := range sc.Spec.Quota.Limits {
		limit := &sc.Spec.Quota.Limits[i]

		// These per-project grants set ConsumerRef.Name to the consumer
		// project, so they only make sense when ConsumerType.Kind is a Project.
		// A mismatched kind would emit a grant whose ref kind and name
		// disagree; skip rather than write a broken grant.
		if limit.ConsumerType.Kind != projectConsumerKind {
			logger.Info("skipping quota limit with unsupported consumer kind for per-project grant",
				"limit", limit.Name,
				"consumerKind", limit.ConsumerType.Kind,
				"supportedKind", projectConsumerKind)
			continue
		}

		grantName := resourceGrantName(svc.Spec.ServiceName, consumerProject, limit.Name)

		grant := &quotav1alpha1.ResourceGrant{
			TypeMeta: metav1.TypeMeta{
				APIVersion: quotav1alpha1.GroupVersion.String(),
				Kind:       "ResourceGrant",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      grantName,
				Namespace: quotaGrantNamespace,
				Labels: map[string]string{
					labelManagedBy:       labelManagedByValue,
					labelEntitlementName: entitlement.Name,
				},
			},
			Spec: quotav1alpha1.ResourceGrantSpec{
				ConsumerRef: quotav1alpha1.ConsumerRef{
					APIGroup: limit.ConsumerType.APIGroup,
					Kind:     limit.ConsumerType.Kind,
					Name:     consumerProject,
				},
				Allowances: []quotav1alpha1.Allowance{
					{
						ResourceType: limit.Metric,
						Buckets: []quotav1alpha1.Bucket{
							{Amount: limit.DefaultLimit},
						},
					},
				},
			},
		}

		if err := consumerClient.Patch(ctx, grant, client.Apply,
			client.FieldOwner(quotaGrantFieldManager),
			client.ForceOwnership,
		); err != nil {
			return fmt.Errorf("apply ResourceGrant %q for limit %q: %w", grantName, limit.Name, err)
		}
		desired[grantName] = struct{}{}
	}

	// Prune grants previously applied for this entitlement that are no longer
	// in the desired set (e.g. a limit was dropped from the configuration).
	// Reached only after a successful ServiceConfiguration lookup, so the
	// desired set reflects the genuine config rather than a transient error.
	return r.pruneStaleQuotaGrants(ctx, consumerClient, entitlement, desired)
}

// pruneStaleQuotaGrants deletes ResourceGrants managed by this reconciler for
// the given entitlement that are not in the desired set. It mirrors the
// desired-set/prune pattern used by QuotaFanOut. Grants are matched on BOTH
// the managed-by label AND the entitlement-name label so that objects a human
// or other controller happened to label with the entitlement name are left
// untouched. Callers MUST only invoke this after a successful configuration
// lookup; otherwise a transient error would collapse the desired set to empty
// and delete every grant.
func (r *ServiceEntitlementReconciler) pruneStaleQuotaGrants(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	desired map[string]struct{},
) error {
	var list quotav1alpha1.ResourceGrantList
	if err := consumerClient.List(ctx, &list,
		client.InNamespace(quotaGrantNamespace),
		client.MatchingLabels{
			labelManagedBy:       labelManagedByValue,
			labelEntitlementName: entitlement.Name,
		},
	); err != nil {
		return fmt.Errorf("list ResourceGrants for entitlement %q: %w", entitlement.Name, err)
	}
	for i := range list.Items {
		grant := &list.Items[i]
		if _, keep := desired[grant.Name]; keep {
			continue
		}
		if err := consumerClient.Delete(ctx, grant); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale ResourceGrant %q: %w", grant.Name, err)
		}
	}
	return nil
}

// pruneQuotaGrants deletes all ResourceGrants in the consumer VCP that this
// reconciler manages for the given entitlement. Called during finalization
// before the finalizer is removed. Matching requires BOTH the managed-by label
// AND the entitlement-name label so grants a human or other controller
// happened to label with the entitlement name are not deleted.
func (r *ServiceEntitlementReconciler) pruneQuotaGrants(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
) error {
	var list quotav1alpha1.ResourceGrantList
	if err := consumerClient.List(ctx, &list,
		client.InNamespace(quotaGrantNamespace),
		client.MatchingLabels{
			labelManagedBy:       labelManagedByValue,
			labelEntitlementName: entitlement.Name,
		},
	); err != nil {
		return fmt.Errorf("list ResourceGrants for entitlement %q: %w", entitlement.Name, err)
	}

	for i := range list.Items {
		grant := &list.Items[i]
		if err := consumerClient.Delete(ctx, grant); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ResourceGrant %q: %w", grant.Name, err)
		}
	}
	return nil
}

// resourceGrantName produces a deterministic, DNS-safe name for a ResourceGrant
// from the triple (serviceName, consumerProject, limitName).
func resourceGrantName(serviceName, consumerProject, limitName string) string {
	sum := sha256.Sum256([]byte(serviceName + "/" + consumerProject + "/" + limitName))
	return "rg-" + hex.EncodeToString(sum[:8])
}
