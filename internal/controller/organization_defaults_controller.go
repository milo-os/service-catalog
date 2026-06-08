// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// orgConsumerKind is the only ConsumerType.Kind handled by this
	// reconciler. ServiceConfiguration limits with any other kind are
	// the responsibility of a different code path (per-project grants
	// live in ServiceEntitlementReconciler.ensureQuotaGrants).
	orgConsumerKind = "Organization"

	// orgDefaultsFieldManager is the SSA field manager used when
	// applying ResourceGrants from this controller. Kept distinct from
	// the per-project quotaGrantFieldManager so the two controllers
	// never fight over a shared grant.
	orgDefaultsFieldManager = "services-operator-org-defaults"

	// labelOrgDefaults marks ResourceGrants owned by this controller.
	// Combined with labelManagedBy it scopes the prune query so grants
	// authored by humans or other controllers are left alone.
	labelOrgDefaults = "services.miloapis.com/org-default"
)

// OrganizationDefaultsReconciler materializes the default org-scoped
// ResourceGrants declared by every Published ServiceConfiguration.
//
// For each Organization × org-scoped quota limit pairing, the
// reconciler ensures a ResourceGrant exists in the org's tenant
// namespace whose ConsumerRef points at the Organization and whose
// allowance amount equals the limit's defaultLimit. Removing a limit
// from a ServiceConfiguration (or moving the configuration to Draft)
// causes the corresponding grants to be pruned on the next reconcile
// of each affected Organization.
//
// This complements ServiceEntitlementReconciler.ensureQuotaGrants
// (which handles per-project limits triggered by ServiceEntitlement
// creation). Project- and org-scoped grants are emitted by different
// reconcilers and use different SSA field managers, so the two cannot
// stomp on each other.
type OrganizationDefaultsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=resourcemanager.miloapis.com,resources=organizations,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=quota.miloapis.com,resources=resourcegrants,verbs=get;list;create;update;patch;delete

func (r *OrganizationDefaultsReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var org resourcemanagerv1alpha1.Organization
	if err := r.Get(ctx, req.NamespacedName, &org); err != nil {
		if apierrors.IsNotFound(err) {
			// The org's tenant namespace is owned by milo's
			// resourcemanager controller; it will be deleted (along with
			// the ResourceGrants in it) when the org goes away. Nothing
			// for us to do here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetch Organization: %w", err)
	}

	if !org.DeletionTimestamp.IsZero() {
		// Same reasoning as the NotFound branch.
		return ctrl.Result{}, nil
	}

	orgNamespace := organizationNamespace(org.Name)

	// Gather every (serviceName, limit) pair where the limit's consumer
	// kind is Organization. Reads against the local cache, so the cost
	// scales with the number of Published ServiceConfigurations rather
	// than with the number of orgs.
	var scList servicesv1alpha1.ServiceConfigurationList
	if err := r.List(ctx, &scList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ServiceConfigurations: %w", err)
	}

	desired := make(map[string]struct{})
	for i := range scList.Items {
		sc := &scList.Items[i]
		if sc.Spec.Phase != servicesv1alpha1.PhasePublished || sc.Spec.Quota == nil {
			continue
		}

		for j := range sc.Spec.Quota.Limits {
			limit := &sc.Spec.Quota.Limits[j]
			if limit.ConsumerType.Kind != orgConsumerKind {
				continue
			}

			grantName := organizationDefaultGrantName(sc.Spec.ServiceRef.Name, org.Name, limit.Name)

			grant := &quotav1alpha1.ResourceGrant{
				TypeMeta: metav1.TypeMeta{
					APIVersion: quotav1alpha1.GroupVersion.String(),
					Kind:       "ResourceGrant",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      grantName,
					Namespace: orgNamespace,
					Labels: map[string]string{
						labelManagedBy:    labelManagedByValue,
						labelOrgDefaults:  "true",
						labelOwnerService: sc.Spec.ServiceRef.Name,
					},
				},
				Spec: quotav1alpha1.ResourceGrantSpec{
					ConsumerRef: quotav1alpha1.ConsumerRef{
						APIGroup: limit.ConsumerType.APIGroup,
						Kind:     limit.ConsumerType.Kind,
						Name:     org.Name,
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

			if err := r.Patch(ctx, grant, client.Apply,
				client.FieldOwner(orgDefaultsFieldManager),
				client.ForceOwnership,
			); err != nil {
				// A missing namespace means the resourcemanager controller
				// has not finished provisioning this org yet; controller-
				// runtime will retry. Surface the error so the retry shows
				// up in metrics rather than silently swallowing it.
				return ctrl.Result{}, fmt.Errorf("apply ResourceGrant %q for limit %q in org %q: %w",
					grantName, limit.Name, org.Name, err)
			}

			logger.V(1).Info("applied org default grant",
				"org", org.Name,
				"grant", grantName,
				"limit", limit.Name,
				"amount", limit.DefaultLimit,
			)
			desired[grantName] = struct{}{}
		}
	}

	if err := r.pruneStaleOrgDefaultGrants(ctx, orgNamespace, desired); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// pruneStaleOrgDefaultGrants deletes ResourceGrants in the org's
// namespace that this controller previously authored but which are no
// longer in the desired set (e.g. because the underlying limit was
// removed from a ServiceConfiguration). Matching requires BOTH the
// shared managed-by label and the org-defaults marker so grants from
// other controllers (or human operators) are left alone.
func (r *OrganizationDefaultsReconciler) pruneStaleOrgDefaultGrants(
	ctx context.Context,
	orgNamespace string,
	desired map[string]struct{},
) error {
	var list quotav1alpha1.ResourceGrantList
	if err := r.List(ctx, &list,
		client.InNamespace(orgNamespace),
		client.MatchingLabels{
			labelManagedBy:   labelManagedByValue,
			labelOrgDefaults: "true",
		},
	); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("list ResourceGrants in %q: %w", orgNamespace, err)
	}
	for i := range list.Items {
		g := &list.Items[i]
		if _, keep := desired[g.Name]; keep {
			continue
		}
		if err := r.Delete(ctx, g); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale ResourceGrant %q: %w", g.Name, err)
		}
	}
	return nil
}

// organizationDefaultGrantName produces a deterministic, DNS-safe
// name for an org-default ResourceGrant from the triple
// (serviceName, orgName, limitName). Idempotent SSA depends on this
// being stable across reconciles.
func organizationDefaultGrantName(serviceName, orgName, limitName string) string {
	sum := sha256.Sum256([]byte(serviceName + "/" + orgName + "/" + limitName))
	return "org-rg-" + hex.EncodeToString(sum[:8])
}

// organizationNamespace returns the tenant namespace for an
// Organization. The convention is fixed across the platform: every
// org-scoped namespaced resource lives in "organization-<name>". See
// milo's resourcemanager controller for the namespace provisioner.
func organizationNamespace(orgName string) string {
	return "organization-" + orgName
}

// SetupWithManager wires the reconciler into the manager. Client and
// Scheme are populated from the manager if not already set so tests
// can inject fakes without re-wiring.
//
// The Watches() on ServiceConfiguration re-enqueues every Organization
// when a ServiceConfiguration changes. Re-enqueueing the entire org
// set is reasonable because ServiceConfiguration changes are rare
// (publishing a new version, adding a quota limit) and reconciling an
// org is cheap.
func (r *OrganizationDefaultsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("organization-defaults").
		For(&resourcemanagerv1alpha1.Organization{}).
		Watches(
			&servicesv1alpha1.ServiceConfiguration{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueAllOrganizations),
			builder.WithPredicates(),
		).
		Complete(r)
}

// enqueueAllOrganizations returns a reconcile request for every
// Organization known to the cache. Used as the handler for
// ServiceConfiguration changes — when a new limit is published, every
// org needs a fresh reconcile so the new grant lands everywhere.
func (r *OrganizationDefaultsReconciler) enqueueAllOrganizations(ctx context.Context, _ client.Object) []reconcile.Request {
	var orgs resourcemanagerv1alpha1.OrganizationList
	if err := r.List(ctx, &orgs); err != nil {
		log.FromContext(ctx).Error(err, "list Organizations for fan-out re-enqueue")
		return nil
	}
	out := make([]reconcile.Request, 0, len(orgs.Items))
	for i := range orgs.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: orgs.Items[i].Name},
		})
	}
	return out
}
