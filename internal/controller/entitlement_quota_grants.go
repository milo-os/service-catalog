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

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// labelEntitlementManagedBy is applied to ResourceGrants created by the
	// service-entitlement reconciler so they can be pruned on deletion.
	labelEntitlementManagedBy      = "services.miloapis.com/managed-by"
	labelEntitlementManagedByValue = "service-entitlement"

	// labelEntitlementName records which ServiceEntitlement owns the grant.
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
		// No published configuration with quota limits — nothing to create.
		return nil
	}

	for i := range sc.Spec.Quota.Limits {
		limit := &sc.Spec.Quota.Limits[i]
		grantName := resourceGrantName(svc.Spec.ServiceName, consumerProject, limit.Name)

		grant := &quotav1alpha1.ResourceGrant{
			TypeMeta: metav1.TypeMeta{
				APIVersion: quotav1alpha1.GroupVersion.String(),
				Kind:       "ResourceGrant",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: grantName,
				Labels: map[string]string{
					labelEntitlementManagedBy: labelEntitlementManagedByValue,
					labelEntitlementName:      entitlement.Name,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         servicesv1alpha1.GroupVersion.String(),
						Kind:               "ServiceEntitlement",
						Name:               entitlement.Name,
						UID:                entitlement.UID,
						Controller:         boolPtr(true),
						BlockOwnerDeletion: boolPtr(true),
					},
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
	}
	return nil
}

// pruneQuotaGrants deletes all ResourceGrants in the consumer VCP that are
// labeled with the given entitlement name. Called during finalization before
// the finalizer is removed.
func (r *ServiceEntitlementReconciler) pruneQuotaGrants(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
) error {
	var list quotav1alpha1.ResourceGrantList
	if err := consumerClient.List(ctx, &list,
		client.MatchingLabels{labelEntitlementName: entitlement.Name},
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

// boolPtr returns a pointer to the given bool value.
func boolPtr(b bool) *bool { return &b }
