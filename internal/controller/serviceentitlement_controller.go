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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// serviceEntitlementFinalizer guards delete: the reconciler must remove the
// matching ServiceConsumer from the provider project and clean up dependency
// entitlements before allowing Kubernetes to garbage-collect the object.
const serviceEntitlementFinalizer = "services.miloapis.com/service-entitlement"

const (
	reasonEntitlementActive          = "EntitlementActive"
	reasonEntitlementPendingApproval = "EntitlementPendingApproval"
	reasonEntitlementRejected        = "EntitlementRejected"
	reasonServiceNotPublished        = "ServiceNotPublished"
)

// ServiceEntitlementReconciler runs in every engaged project cluster. Each
// reconcile call carries the consumer project name as req.ClusterName. The
// reconciler reads the referenced Service from the root cluster, resolves the
// provider project, and writes a ServiceConsumer into the provider project's
// virtual control plane.
type ServiceEntitlementReconciler struct {
	// rootClient reads cluster-scoped Service objects from the root key space.
	// Services live in the root etcd prefix, not in any project, so a normal
	// per-cluster client (which talks to a project's virtual control plane)
	// cannot see them.
	rootClient client.Client
	Manager    mcmanager.Manager
	Scheme     *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements/finalizers,verbs=update
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconsumers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconsumers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch

func (r *ServiceEntitlementReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	consumerProject := req.ClusterName
	if consumerProject == "" {
		return ctrl.Result{}, fmt.Errorf("ServiceEntitlement reconcile invoked without a cluster name")
	}

	consumerCluster, err := r.Manager.GetCluster(ctx, consumerProject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get consumer cluster %q: %w", consumerProject, err)
	}
	consumerClient := consumerCluster.GetClient()

	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(ctx, req.NamespacedName, &entitlement); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ServiceEntitlement: %w", err)
	}

	if !entitlement.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, consumerProject, consumerClient, &entitlement)
	}

	if !controllerutil.ContainsFinalizer(&entitlement, serviceEntitlementFinalizer) {
		controllerutil.AddFinalizer(&entitlement, serviceEntitlementFinalizer)
		if err := consumerClient.Update(ctx, &entitlement); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	var svc servicesv1alpha1.Service
	if err := r.rootClient.Get(ctx, types.NamespacedName{Name: entitlement.Spec.ServiceRef.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setRejectedStatus(ctx, consumerClient, &entitlement,
				reasonServiceNotPublished, "Referenced Service does not exist.")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Service %q: %w", entitlement.Spec.ServiceRef.Name, err)
	}

	if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
		return ctrl.Result{}, r.setRejectedStatus(ctx, consumerClient, &entitlement,
			reasonServiceNotPublished,
			fmt.Sprintf("Referenced Service %q is in phase %q; only Published services may be entitled.",
				svc.Spec.ServiceName, svc.Spec.Phase))
	}

	providerProject := svc.Spec.Owner.ProducerProjectRef.Name
	providerCluster, err := r.Manager.GetCluster(ctx, providerProject)
	if err != nil {
		// Provider project may not be engaged yet; requeue.
		logger.Info("provider cluster not yet available, requeuing", "providerProject", providerProject, "err", err)
		return ctrl.Result{Requeue: true}, nil
	}
	providerClient := providerCluster.GetClient()

	gated := svc.Spec.EnablementPolicy != nil && svc.Spec.EnablementPolicy.Mode == servicesv1alpha1.EnablementModeGatedByProvider

	consumerName := serviceConsumerName(svc.Spec.ServiceName, consumerProject)
	consumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: consumerName},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, providerClient, consumer, func() error {
		consumer.Spec.ServiceRef = servicesv1alpha1.ServiceRef{Name: svc.Name}
		consumer.Spec.ConsumerProjectRef = servicesv1alpha1.ConsumerProjectRef{Name: consumerProject}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to upsert ServiceConsumer %q in provider %q: %w", consumerName, providerProject, err)
	}
	logger.V(1).Info("upserted ServiceConsumer", "name", consumerName, "providerProject", providerProject, "op", op)

	if err := r.reconcileConsumerStatus(ctx, providerClient, consumer, gated); err != nil {
		return ctrl.Result{}, err
	}

	// Set entitlement status. If the consumer was already approved/denied by
	// the provider (re-reconcile triggered by the consumer controller), reflect
	// that decision back onto the entitlement.
	desiredPhase := servicesv1alpha1.EntitlementPhaseActive
	reason := reasonEntitlementActive
	message := "Service entitlement is active."
	switch {
	case gated && consumer.Spec.Approval == nil:
		desiredPhase = servicesv1alpha1.EntitlementPhasePendingApproval
		reason = reasonEntitlementPendingApproval
		message = "Awaiting provider approval."
	case gated && consumer.Spec.Approval != nil && consumer.Spec.Approval.Decision == servicesv1alpha1.ApprovalDecisionDenied:
		desiredPhase = servicesv1alpha1.EntitlementPhaseRejected
		reason = reasonEntitlementRejected
		message = "Provider denied the request."
	}

	if err := r.setEntitlementStatus(ctx, consumerClient, &entitlement, desiredPhase, reason, message); err != nil {
		return ctrl.Result{}, err
	}

	// Only enroll dependencies once the parent is Active. Dependency
	// entitlements created earlier (while gated) would race the parent
	// approval; defer creation until the parent is unblocked.
	if desiredPhase == servicesv1alpha1.EntitlementPhaseActive {
		if err := r.ensureDependencies(ctx, consumerClient, &svc, &entitlement); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *ServiceEntitlementReconciler) reconcileConsumerStatus(ctx context.Context, providerClient client.Client, consumer *servicesv1alpha1.ServiceConsumer, gated bool) error {
	original := consumer.Status.DeepCopy()
	consumer.Status.ObservedGeneration = consumer.Generation

	desired := servicesv1alpha1.ConsumerPhaseActive
	switch {
	case gated && consumer.Spec.Approval == nil:
		desired = servicesv1alpha1.ConsumerPhasePendingApproval
	case gated && consumer.Spec.Approval != nil && consumer.Spec.Approval.Decision == servicesv1alpha1.ApprovalDecisionDenied:
		desired = servicesv1alpha1.ConsumerPhaseDenied
	case gated && consumer.Spec.Approval != nil && consumer.Spec.Approval.Decision == servicesv1alpha1.ApprovalDecisionApproved:
		desired = servicesv1alpha1.ConsumerPhaseActive
	}

	consumer.Status.Phase = desired
	if desired == servicesv1alpha1.ConsumerPhaseActive && consumer.Status.EntitledAt == nil {
		now := metav1.Now()
		consumer.Status.EntitledAt = &now
	}

	if equalConsumerStatus(original, &consumer.Status) {
		return nil
	}
	if err := providerClient.Status().Update(ctx, consumer); err != nil {
		return fmt.Errorf("failed to update ServiceConsumer status: %w", err)
	}
	return nil
}

func equalConsumerStatus(a, b *servicesv1alpha1.ServiceConsumerStatus) bool {
	if a.Phase != b.Phase {
		return false
	}
	if (a.EntitledAt == nil) != (b.EntitledAt == nil) {
		return false
	}
	if a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	return true
}

func (r *ServiceEntitlementReconciler) setEntitlementStatus(ctx context.Context, consumerClient client.Client, entitlement *servicesv1alpha1.ServiceEntitlement, phase servicesv1alpha1.EntitlementPhase, reason, message string) error {
	original := entitlement.Status.DeepCopy()

	entitlement.Status.ObservedGeneration = entitlement.Generation
	entitlement.Status.Phase = phase
	if entitlement.Status.Origin == "" {
		entitlement.Status.Origin = servicesv1alpha1.EntitlementOriginDirect
	}
	if phase == servicesv1alpha1.EntitlementPhaseActive && entitlement.Status.EntitledAt == nil {
		now := metav1.Now()
		entitlement.Status.EntitledAt = &now
	}

	cond := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: entitlement.Generation,
	}
	if phase == servicesv1alpha1.EntitlementPhaseActive {
		cond.Status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&entitlement.Status.Conditions, cond)

	if equalEntitlementStatus(original, &entitlement.Status) {
		return nil
	}
	if err := consumerClient.Status().Update(ctx, entitlement); err != nil {
		return fmt.Errorf("failed to update ServiceEntitlement status: %w", err)
	}
	return nil
}

func (r *ServiceEntitlementReconciler) setRejectedStatus(ctx context.Context, consumerClient client.Client, entitlement *servicesv1alpha1.ServiceEntitlement, reason, message string) error {
	return r.setEntitlementStatus(ctx, consumerClient, entitlement, servicesv1alpha1.EntitlementPhaseRejected, reason, message)
}

func equalEntitlementStatus(a, b *servicesv1alpha1.ServiceEntitlementStatus) bool {
	if a.Phase != b.Phase || a.Origin != b.Origin || a.DependencyOf != b.DependencyOf {
		return false
	}
	if (a.EntitledAt == nil) != (b.EntitledAt == nil) {
		return false
	}
	if a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	if !conditionsEqual(a.Conditions, b.Conditions, ConditionTypeReady) {
		return false
	}
	return true
}

// ensureDependencies walks Service.spec.dependencies and creates a derived
// ServiceEntitlement in the consumer cluster for each one not already present.
// Dependency entitlements traverse the same reconcile path recursively.
func (r *ServiceEntitlementReconciler) ensureDependencies(ctx context.Context, consumerClient client.Client, svc *servicesv1alpha1.Service, parent *servicesv1alpha1.ServiceEntitlement) error {
	if parent.Status.Origin == servicesv1alpha1.EntitlementOriginDependency {
		// Don't recursively enroll dependencies of dependencies in this pass;
		// each dependency entitlement will run its own reconcile and pull in
		// its own dependencies. This keeps each reconcile narrow and avoids
		// long write chains in a single call.
		return nil
	}

	for _, dep := range svc.Spec.Dependencies {
		depName := dep.ServiceRef.Name
		var existing servicesv1alpha1.ServiceEntitlement
		err := consumerClient.Get(ctx, types.NamespacedName{Name: depName}, &existing)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to look up dependency entitlement %q: %w", depName, err)
		}

		depEntitlement := &servicesv1alpha1.ServiceEntitlement{
			ObjectMeta: metav1.ObjectMeta{Name: depName},
			Spec: servicesv1alpha1.ServiceEntitlementSpec{
				ServiceRef: servicesv1alpha1.ServiceRef{Name: depName},
			},
		}
		if err := consumerClient.Create(ctx, depEntitlement); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create dependency entitlement %q: %w", depName, err)
		}

		// Stamp Origin/DependencyOf on status so deletion logic can find this
		// entitlement as a child of the parent.
		fresh := &servicesv1alpha1.ServiceEntitlement{}
		if err := consumerClient.Get(ctx, types.NamespacedName{Name: depName}, fresh); err != nil {
			return fmt.Errorf("failed to re-read dependency entitlement %q: %w", depName, err)
		}
		fresh.Status.Origin = servicesv1alpha1.EntitlementOriginDependency
		fresh.Status.DependencyOf = parent.Name
		if err := consumerClient.Status().Update(ctx, fresh); err != nil {
			return fmt.Errorf("failed to stamp dependency origin on %q: %w", depName, err)
		}
	}
	return nil
}

func (r *ServiceEntitlementReconciler) reconcileDelete(ctx context.Context, consumerProject string, consumerClient client.Client, entitlement *servicesv1alpha1.ServiceEntitlement) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(entitlement, serviceEntitlementFinalizer) {
		return ctrl.Result{}, nil
	}

	// Resolve the provider project. The Service may have moved phase (or been
	// deleted) in the meantime; if so we still want to clean up the consumer.
	var svc servicesv1alpha1.Service
	if err := r.rootClient.Get(ctx, types.NamespacedName{Name: entitlement.Spec.ServiceRef.Name}, &svc); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get Service during finalize: %w", err)
	}

	if svc.Spec.Owner.ProducerProjectRef.Name != "" {
		providerCluster, err := r.Manager.GetCluster(ctx, svc.Spec.Owner.ProducerProjectRef.Name)
		if err != nil {
			logger.Info("provider cluster unavailable during finalize, requeuing", "err", err)
			return ctrl.Result{Requeue: true}, nil
		}
		providerClient := providerCluster.GetClient()
		consumerName := serviceConsumerName(svc.Spec.ServiceName, consumerProject)
		consumer := &servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: consumerName}}
		if err := providerClient.Delete(ctx, consumer); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete ServiceConsumer %q: %w", consumerName, err)
		}
	}

	// Best-effort cleanup of dependency entitlements that were spawned by this
	// parent. We only delete dependency entitlements whose dependencyOf points
	// at this entitlement; other parents may still need the same dependency.
	if entitlement.Status.Origin != servicesv1alpha1.EntitlementOriginDependency {
		var siblings servicesv1alpha1.ServiceEntitlementList
		if err := consumerClient.List(ctx, &siblings); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to list entitlements during finalize: %w", err)
		}
		for i := range siblings.Items {
			child := &siblings.Items[i]
			if child.Status.Origin != servicesv1alpha1.EntitlementOriginDependency {
				continue
			}
			if child.Status.DependencyOf != entitlement.Name {
				continue
			}
			if err := consumerClient.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("failed to delete dependency entitlement %q: %w", child.Name, err)
			}
		}
	}

	controllerutil.RemoveFinalizer(entitlement, serviceEntitlementFinalizer)
	if err := consumerClient.Update(ctx, entitlement); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}
	logger.Info("finalized ServiceEntitlement", "name", entitlement.Name)
	return ctrl.Result{}, nil
}

// serviceConsumerName derives a deterministic, DNS-safe name for the
// ServiceConsumer that mirrors a (service, consumer-project) pair. The hash
// keeps the name short enough for Kubernetes name validation regardless of
// how long either input is.
func serviceConsumerName(serviceName, consumerProject string) string {
	sum := sha256.Sum256([]byte(serviceName + "/" + consumerProject))
	return "sc-" + hex.EncodeToString(sum[:8])
}

// SetupWithManager registers the reconciler on the multicluster manager.
// WithEngageWithProviderClusters(true) — and *not* WithEngageWithLocalCluster —
// because ServiceEntitlements live in project virtual control planes, never
// the root cluster.
func (r *ServiceEntitlementReconciler) SetupWithManager(mgr mcmanager.Manager, rootClient client.Client) error {
	r.rootClient = rootClient
	r.Manager = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		Named("service-entitlement").
		For(&servicesv1alpha1.ServiceEntitlement{}, mcbuilder.WithEngageWithProviderClusters(true)).
		Complete(r)
}
