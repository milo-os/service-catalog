// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// Field indexes on root-cluster billing objects used by quota gating
	// lookups and watch fan-out.
	bindingProjectRefIndex        = ".spec.projectRef.name"
	bindingBillingAccountRefIndex = ".spec.billingAccountRef.name"
	billingEntitlementOfferIndex  = ".spec.offerRef.name"
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

	// Opt-in BillingEntitlement gating: only issue project grants when the
	// project's billing account is entitled to an Offer that includes this
	// service. OrganizationDefault (the default) keeps issuing grants as
	// before. OrganizationDefaultsReconciler is unaffected.
	if !usesOrganizationDefaultQuotaGating(sc) {
		entitled, err := r.serviceEntitledViaBilling(ctx, consumerProject, svc.Spec.ServiceName)
		if err != nil {
			return err
		}
		if !entitled {
			logger.Info("pruning quota grants; service not covered by BillingEntitlement Offer",
				"service", svc.Spec.ServiceName,
				"project", consumerProject,
				"entitlement", entitlement.Name)
			return r.pruneStaleQuotaGrants(ctx, consumerClient, entitlement, map[string]struct{}{})
		}
	}

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

		if err := consumerClient.Patch(ctx, grant, client.Apply, //nolint:staticcheck // SA1019: migrate to client.Apply() with ApplyConfiguration in a follow-up
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

// offerIncludesService reports whether any snapshotted ServicePricing on the
// Offer covers the given canonical service name (Service.spec.serviceName).
func offerIncludesService(offer *billingv1alpha1.Offer, serviceName string) bool {
	if offer == nil || serviceName == "" {
		return false
	}
	for i := range offer.Spec.ServicePricings {
		if offer.Spec.ServicePricings[i].Spec.ServiceRef == serviceName {
			return true
		}
	}
	return false
}

// serviceEntitledViaBilling reports whether the project's active billing
// account has a Ready BillingEntitlement whose Offer snapshot includes
// serviceName. Transient API errors are returned so callers requeue rather
// than prune grants on a flaky read. A definitive "not entitled" (no binding,
// no Ready BE, offer missing the service) returns (false, nil).
func (r *ServiceEntitlementReconciler) serviceEntitledViaBilling(
	ctx context.Context,
	consumerProject string,
	serviceName string,
) (bool, error) {
	binding, err := r.activeBillingAccountBinding(ctx, consumerProject)
	if err != nil {
		return false, err
	}
	if binding == nil {
		return false, nil
	}

	be, err := r.readyBillingEntitlement(ctx, binding.Namespace, binding.Spec.BillingAccountRef.Name)
	if err != nil {
		return false, err
	}
	if be == nil {
		return false, nil
	}

	var offer billingv1alpha1.Offer
	if err := r.rootClient.Get(ctx, types.NamespacedName{Name: be.Spec.OfferRef.Name}, &offer); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get Offer %q for BillingEntitlement %q: %w",
			be.Spec.OfferRef.Name, be.Name, err)
	}

	return offerIncludesService(&offer, serviceName), nil
}

// activeBillingAccountBinding returns the Active BillingAccountBinding for
// the project, or nil when none exists.
func (r *ServiceEntitlementReconciler) activeBillingAccountBinding(
	ctx context.Context,
	consumerProject string,
) (*billingv1alpha1.BillingAccountBinding, error) {
	var list billingv1alpha1.BillingAccountBindingList
	if err := r.rootClient.List(ctx, &list,
		client.MatchingFields{bindingProjectRefIndex: consumerProject},
	); err != nil {
		return nil, fmt.Errorf("list BillingAccountBindings for project %q: %w", consumerProject, err)
	}
	for i := range list.Items {
		b := &list.Items[i]
		if b.Status.Phase == billingv1alpha1.BillingAccountBindingPhaseActive &&
			b.DeletionTimestamp.IsZero() {
			return b, nil
		}
	}
	return nil, nil
}

// readyBillingEntitlement returns the Ready BillingEntitlement for the
// billing account in namespace, or nil when none is Ready.
func (r *ServiceEntitlementReconciler) readyBillingEntitlement(
	ctx context.Context,
	namespace, billingAccountName string,
) (*billingv1alpha1.BillingEntitlement, error) {
	var list billingv1alpha1.BillingEntitlementList
	if err := r.rootClient.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list BillingEntitlements in %q: %w", namespace, err)
	}
	for i := range list.Items {
		be := &list.Items[i]
		if be.DeletionTimestamp.IsZero() &&
			be.Spec.BillingAccountRef.Name == billingAccountName &&
			apimeta.IsStatusConditionTrue(be.Status.Conditions, ConditionTypeReady) {
			return be, nil
		}
	}
	return nil, nil
}

// mapBillingEntitlementToServiceEntitlements enqueues ServiceEntitlements in
// every project bound to the BillingEntitlement's billing account.
func (r *ServiceEntitlementReconciler) mapBillingEntitlementToServiceEntitlements(
	ctx context.Context,
	be *billingv1alpha1.BillingEntitlement,
) []mcreconcile.Request {
	return r.enqueueServiceEntitlementsForBillingAccount(ctx, be.Namespace, be.Spec.BillingAccountRef.Name)
}

// mapBillingAccountBindingToServiceEntitlements enqueues ServiceEntitlements
// in the binding's project when billing responsibility changes.
func (r *ServiceEntitlementReconciler) mapBillingAccountBindingToServiceEntitlements(
	ctx context.Context,
	binding *billingv1alpha1.BillingAccountBinding,
) []mcreconcile.Request {
	return r.enqueueServiceEntitlementsForProject(ctx, binding.Spec.ProjectRef.Name)
}

// mapOfferToServiceEntitlements enqueues ServiceEntitlements for every project
// whose BillingEntitlement references the Offer.
func (r *ServiceEntitlementReconciler) mapOfferToServiceEntitlements(
	ctx context.Context,
	offer *billingv1alpha1.Offer,
) []mcreconcile.Request {
	var beList billingv1alpha1.BillingEntitlementList
	if err := r.rootClient.List(ctx, &beList,
		client.MatchingFields{billingEntitlementOfferIndex: offer.Name},
	); err != nil {
		log.FromContext(ctx).Error(err, "list BillingEntitlements for Offer fan-out", "offer", offer.Name)
		return nil
	}

	seen := make(map[string]struct{})
	var out []mcreconcile.Request
	for i := range beList.Items {
		be := &beList.Items[i]
		reqs := r.enqueueServiceEntitlementsForBillingAccount(ctx, be.Namespace, be.Spec.BillingAccountRef.Name)
		for _, req := range reqs {
			key := string(req.ClusterName) + "/" + req.Namespace + "/" + req.Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, req)
		}
	}
	return out
}

// mapServiceConfigurationToServiceEntitlements enqueues ServiceEntitlements
// for the service named by the configuration so quotaGating / quota limit
// changes re-evaluate project grants.
func (r *ServiceEntitlementReconciler) mapServiceConfigurationToServiceEntitlements(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
) []mcreconcile.Request {
	return enqueueEntitlementsForConfiguration(ctx, r.rootClient, r.Manager, sc)
}

// enqueueEntitlementsForConfiguration turns a root-cluster ServiceConfiguration
// event into project-scoped requests for every ServiceEntitlement naming that
// service. The quota and provisioning fan-outs share it; both need a
// declaration change to reach entitled projects before the next resync.
func enqueueEntitlementsForConfiguration(
	ctx context.Context,
	rootClient client.Client,
	mgr mcmanager.Manager,
	sc *servicesv1alpha1.ServiceConfiguration,
) []mcreconcile.Request {
	if sc.Spec.ServiceRef.Name == "" {
		return nil
	}
	var svc servicesv1alpha1.Service
	if err := rootClient.Get(ctx, types.NamespacedName{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "get Service for ServiceConfiguration fan-out",
				"serviceConfiguration", sc.Name, "serviceRef", sc.Spec.ServiceRef.Name)
		}
		return nil
	}
	canonical := svc.Spec.ServiceName
	if canonical == "" {
		return nil
	}

	var projects resourcemanagerv1alpha1.ProjectList
	if err := rootClient.List(ctx, &projects); err != nil {
		log.FromContext(ctx).Error(err, "list Projects for ServiceConfiguration fan-out",
			"serviceConfiguration", sc.Name)
		return nil
	}

	seen := make(map[string]struct{})
	var out []mcreconcile.Request
	for i := range projects.Items {
		project := projects.Items[i].Name
		if project == "" {
			continue
		}
		cluster, err := mgr.GetCluster(ctx, multicluster.ClusterName(project))
		if err != nil {
			continue
		}
		var list servicesv1alpha1.ServiceEntitlementList
		if err := cluster.GetClient().List(ctx, &list,
			client.MatchingFields{entitlementServiceNameIndex: canonical},
		); err != nil {
			log.FromContext(ctx).Error(err, "list ServiceEntitlements for ServiceConfiguration fan-out",
				"project", project, "service", canonical)
			continue
		}
		for j := range list.Items {
			key := project + "/" + list.Items[j].Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, mcreconcile.Request{
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{Name: list.Items[j].Name},
				},
				ClusterName: multicluster.ClusterName(project),
			})
		}
	}
	return out
}

// enqueueServiceEntitlementsForBillingAccount finds Active bindings for the
// billing account and enqueues ServiceEntitlements in those projects.
func (r *ServiceEntitlementReconciler) enqueueServiceEntitlementsForBillingAccount(
	ctx context.Context,
	namespace, billingAccountName string,
) []mcreconcile.Request {
	var bindings billingv1alpha1.BillingAccountBindingList
	if err := r.rootClient.List(ctx, &bindings,
		client.InNamespace(namespace),
		client.MatchingFields{bindingBillingAccountRefIndex: billingAccountName},
	); err != nil {
		log.FromContext(ctx).Error(err, "list BillingAccountBindings for BE fan-out",
			"namespace", namespace, "billingAccount", billingAccountName)
		return nil
	}

	seen := make(map[string]struct{})
	var out []mcreconcile.Request
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if b.Status.Phase != billingv1alpha1.BillingAccountBindingPhaseActive {
			continue
		}
		project := b.Spec.ProjectRef.Name
		if project == "" {
			continue
		}
		if _, ok := seen[project]; ok {
			continue
		}
		seen[project] = struct{}{}
		out = append(out, r.enqueueServiceEntitlementsForProject(ctx, project)...)
	}
	return out
}

// enqueueServiceEntitlementsForProject lists ServiceEntitlements in the
// project's virtual control plane and returns reconcile requests. Projects
// that are not yet engaged are skipped.
func (r *ServiceEntitlementReconciler) enqueueServiceEntitlementsForProject(
	ctx context.Context,
	project string,
) []mcreconcile.Request {
	logger := log.FromContext(ctx)
	cluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(project))
	if err != nil {
		logger.V(1).Info("skipping ServiceEntitlement fan-out; project cluster not engaged",
			"project", project, "err", err)
		return nil
	}

	var list servicesv1alpha1.ServiceEntitlementList
	if err := cluster.GetClient().List(ctx, &list); err != nil {
		logger.Error(err, "list ServiceEntitlements for billing fan-out", "project", project)
		return nil
	}

	out := make([]mcreconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, mcreconcile.Request{
			Request: reconcile.Request{
				NamespacedName: types.NamespacedName{Name: list.Items[i].Name},
			},
			ClusterName: multicluster.ClusterName(project),
		})
	}
	return out
}
