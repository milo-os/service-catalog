// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// serviceEntitlementFinalizer guards delete: the reconciler must remove the
// matching ServiceConsumer from the provider project and clean up dependency
// entitlements before allowing Kubernetes to garbage-collect the object.
const serviceEntitlementFinalizer = "services.miloapis.com/service-entitlement"

const (
	reasonEntitlementActive          = servicesv1alpha1.ReasonEntitlementActive
	reasonEntitlementPendingApproval = servicesv1alpha1.ReasonEntitlementPendingApproval
	reasonEntitlementRejected        = servicesv1alpha1.ReasonEntitlementRejected
	reasonServiceNotPublished        = servicesv1alpha1.ReasonServiceNotPublished
)

// pendingApprovalRequeueInterval bounds the latency between a provider
// recording an approval decision and the entitlement reflecting it. The
// decision lives on a ServiceConsumer in the provider project's control
// plane, which this controller doesn't watch; the consumer controller
// normally propagates it, but if that write is missed the only other
// trigger is the multi-hour cache resync.
const pendingApprovalRequeueInterval = 2 * time.Minute

// providerTeardownRequeueInterval bounds how often the finalize path re-checks
// whether the provider has finished tearing down the resources it created for
// this project. The entitlement's finalizer is held until the shared
// ServiceConsumer — gated by the provider's own deprovisioning finalizer — is
// gone, so this only paces the wait; it does not drive teardown itself.
const providerTeardownRequeueInterval = 10 * time.Second

// dependencyOfLabel records which parent entitlement pulled a dependency
// entitlement in. Set at create time and never rewritten, it's the provenance
// status.origin and status.dependencyOf are derived from.
const dependencyOfLabel = "services.miloapis.com/dependency-of"

// dependenciesConditionFieldManager owns the DependenciesSatisfied condition,
// which is patched separately from the main status Update so it doesn't race
// the Ready/phase write that precedes dependency enrollment.
const dependenciesConditionFieldManager = "service-entitlement-dependencies"

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
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=quota.miloapis.com,resources=resourcegrants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccountbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=offers,verbs=get;list;watch

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
		return r.reconcileDelete(ctx, string(consumerProject), consumerClient, &entitlement)
	}

	if !controllerutil.ContainsFinalizer(&entitlement, serviceEntitlementFinalizer) {
		controllerutil.AddFinalizer(&entitlement, serviceEntitlementFinalizer)
		if err := consumerClient.Update(ctx, &entitlement); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	svc, err := r.resolveService(ctx, entitlement.Spec.ServiceRef.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setRejectedStatus(ctx, consumerClient, &entitlement,
				reasonServiceNotPublished, "The requested service could not be found.")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Service %q: %w", entitlement.Spec.ServiceRef.Name, err)
	}

	if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
		return ctrl.Result{}, r.setRejectedStatus(ctx, consumerClient, &entitlement,
			reasonServiceNotPublished,
			fmt.Sprintf("The service %q isn't published yet, so it can't be enabled.",
				svc.Spec.ServiceName))
	}

	providerProject := svc.Spec.Owner.ProducerProjectRef.Name
	providerCluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(providerProject))
	if err != nil {
		// Provider project may not be engaged yet; requeue.
		logger.Info("provider cluster not yet available, requeuing", "providerProject", providerProject, "err", err)
		return ctrl.Result{Requeue: true}, nil
	}
	providerClient := providerCluster.GetClient()

	var project resourcemanagerv1alpha1.Project
	var projectSuspended projectSuspension
	if err := r.rootClient.Get(ctx, client.ObjectKey{Name: string(consumerProject)}, &project); err == nil {
		projectSuspended = suspensionFromProject(&project)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get project: %w", err)
	}

	gated := svc.Spec.EnablementPolicy != nil && svc.Spec.EnablementPolicy.Mode == servicesv1alpha1.EnablementModeGatedByProvider

	consumerName := serviceConsumerName(svc.Spec.ServiceName, string(consumerProject))
	consumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: consumerName},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, providerClient, consumer, func() error {
		// Mirror the caller's ref verbatim; the canonical name lives on
		// status.serviceName. Both fields are immutable after creation — the
		// webhook enforces this for everyone, including this controller — so
		// only set them on create.
		if consumer.CreationTimestamp.IsZero() {
			consumer.Spec.ServiceRef = entitlement.Spec.ServiceRef
			consumer.Spec.ConsumerProjectRef = servicesv1alpha1.ConsumerProjectRef{Name: string(consumerProject)}
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to upsert ServiceConsumer %q in provider %q: %w", consumerName, providerProject, err)
	}
	logger.V(1).Info("upserted ServiceConsumer", "name", consumerName, "providerProject", providerProject, "op", op)

	if err := r.reconcileConsumerStatus(ctx, providerClient, consumer, gated, svc.Spec.ServiceName, projectSuspended); err != nil {
		return ctrl.Result{}, err
	}

	// Set entitlement status. If the consumer was already approved/denied by
	// the provider (re-reconcile triggered by the consumer controller), reflect
	// that decision back onto the entitlement.
	desiredPhase := servicesv1alpha1.EntitlementPhaseActive
	reason := reasonEntitlementActive
	message := "This service is enabled and ready to use."
	switch {
	case gated && consumer.Spec.Approval == nil:
		desiredPhase = servicesv1alpha1.EntitlementPhasePendingApproval
		reason = reasonEntitlementPendingApproval
		message = "Waiting for the service provider to approve this request."
	case gated && consumer.Spec.Approval != nil && consumer.Spec.Approval.Decision == servicesv1alpha1.ApprovalDecisionDenied:
		desiredPhase = servicesv1alpha1.EntitlementPhaseRejected
		reason = reasonEntitlementRejected
		message = "The service provider denied this request."
	}

	if err := r.setEntitlementStatus(ctx, consumerClient, &entitlement, desiredPhase, reason, message, svc.Spec.ServiceName, projectSuspended); err != nil {
		return ctrl.Result{}, err
	}

	// Only enroll dependencies and provision quota once the parent is Active.
	// Dependency entitlements created earlier (while gated) would race the
	// parent approval; defer creation until the parent is unblocked.
	if desiredPhase == servicesv1alpha1.EntitlementPhaseActive {
		if err := r.ensureDependencies(ctx, consumerClient, svc, &entitlement); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.ensureQuotaGrants(ctx, consumerClient, string(consumerProject), &entitlement, svc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Bound approval latency while waiting on the provider's decision.
	if desiredPhase == servicesv1alpha1.EntitlementPhasePendingApproval {
		return ctrl.Result{RequeueAfter: pendingApprovalRequeueInterval}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ServiceEntitlementReconciler) reconcileConsumerStatus(ctx context.Context, providerClient client.Client, consumer *servicesv1alpha1.ServiceConsumer, gated bool, canonicalServiceName string, projectSuspended projectSuspension) error {
	// Patched separately from the Update below (see
	// patchConsumerSuspendedCondition): ProjectSuspensionPropagationReconciler
	// writes the same condition independently, on its own Get/patch cycle, so
	// folding it into this function's own before/after diff would race it.
	if err := patchConsumerSuspendedCondition(ctx, providerClient, consumer, projectSuspended); err != nil {
		return fmt.Errorf("failed to apply Suspended condition on ServiceConsumer: %w", err)
	}

	original := consumer.Status.DeepCopy()
	consumer.Status.ObservedGeneration = consumer.Generation
	consumer.Status.ServiceName = canonicalServiceName

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

	if apiequality.Semantic.DeepEqual(original, &consumer.Status) {
		return nil
	}
	if err := providerClient.Status().Update(ctx, consumer); err != nil {
		return fmt.Errorf("failed to update ServiceConsumer status: %w", err)
	}
	return nil
}

func (r *ServiceEntitlementReconciler) setEntitlementStatus(ctx context.Context, consumerClient client.Client, entitlement *servicesv1alpha1.ServiceEntitlement, phase servicesv1alpha1.EntitlementPhase, reason, message, canonicalServiceName string, projectSuspended projectSuspension) error {
	// See the matching comment in reconcileConsumerStatus: patched separately
	// rather than folded into the Update below.
	if err := patchEntitlementSuspendedCondition(ctx, consumerClient, entitlement, projectSuspended); err != nil {
		return fmt.Errorf("failed to apply Suspended condition on ServiceEntitlement: %w", err)
	}

	original := entitlement.Status.DeepCopy()

	entitlement.Status.ObservedGeneration = entitlement.Generation
	entitlement.Status.Phase = phase
	if canonicalServiceName != "" {
		entitlement.Status.ServiceName = canonicalServiceName
	}
	// Derive origin from the create-time label rather than defaulting on an
	// empty value. This reconcile runs concurrently with the parent's stamp on
	// a freshly created dependency entitlement, and defaulting to Direct there
	// permanently orphans it from the parent that's holding it open.
	if parentName := entitlement.Labels[dependencyOfLabel]; parentName != "" {
		entitlement.Status.Origin = servicesv1alpha1.EntitlementOriginDependency
		entitlement.Status.DependencyOf = parentName
	} else if entitlement.Status.Origin == "" {
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
	return r.setEntitlementStatus(ctx, consumerClient, entitlement, servicesv1alpha1.EntitlementPhaseRejected, reason, message, "", projectSuspension{})
}

func equalEntitlementStatus(a, b *servicesv1alpha1.ServiceEntitlementStatus) bool {
	if a.Phase != b.Phase || a.Origin != b.Origin || a.DependencyOf != b.DependencyOf || a.ServiceName != b.ServiceName {
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
//
// This runs for every entitlement, including entitlements this controller
// created to satisfy someone else's dependency. That is what carries a chain
// past its first hop: the derived entitlement's own reconcile enrolls the next
// level down. An earlier version skipped dependency-origin entitlements here to
// keep a single pass narrow, but status.origin records how an entitlement came
// to exist rather than which pass is running, so the later reconcile it deferred
// to took the same early return and the chain stopped at one level.
//
// Each pass still only enrolls this service's direct dependencies; depth comes
// from the next entitlement's own reconcile, so no single call fans out further
// than one level. A dependency cycle terminates for the same reason: entitlement
// names are derived from the dependency Service's object name, so once a cycle's
// entitlements exist, every later pass finds them by name and makes no write.
func (r *ServiceEntitlementReconciler) ensureDependencies(ctx context.Context, consumerClient client.Client, svc *servicesv1alpha1.Service, parent *servicesv1alpha1.ServiceEntitlement) error {
	// Keep going past a dependency that can't be enrolled instead of returning
	// on the first failure: the project should end up with every dependency it
	// can have, and DependenciesSatisfied should name every one it can't.
	var failed []string
	var errs []error
	for _, dep := range svc.Spec.Dependencies {
		if err := r.ensureDependency(ctx, consumerClient, dep, parent); err != nil {
			failed = append(failed, dep.ServiceRef.Name)
			errs = append(errs, err)
		}
	}

	if err := r.setDependenciesCondition(ctx, consumerClient, parent, len(svc.Spec.Dependencies), failed, errs); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ensureDependency enrolls the consumer project in a single declared
// dependency, creating the derived ServiceEntitlement if it isn't there yet.
func (r *ServiceEntitlementReconciler) ensureDependency(ctx context.Context, consumerClient client.Client, dep servicesv1alpha1.ServiceDependency, parent *servicesv1alpha1.ServiceEntitlement) error {
	depSvc, err := r.resolveService(ctx, dep.ServiceRef.Name)
	if err != nil {
		return fmt.Errorf("failed to resolve dependency service %q: %w", dep.ServiceRef.Name, err)
	}
	depCanonical := depSvc.Spec.ServiceName

	// Admission resolves spec.serviceRef.name by metadata.name only (see
	// internal/validation/serviceentitlement.go), so the derived entitlement
	// has to reference the dependency's object name. Naming the canonical
	// service here gets the create rejected outright for every Service whose
	// object name differs from spec.serviceName — the norm in this catalog.
	depEntitlementName := depSvc.Name

	var existing servicesv1alpha1.ServiceEntitlement
	err = consumerClient.Get(ctx, types.NamespacedName{Name: depEntitlementName}, &existing)
	if err == nil {
		// Already enrolled. Re-apply the provenance stamp if it's ours and
		// didn't take — see stampDependencyOrigin. An entitlement the consumer
		// created directly is left alone even if it happens to satisfy this
		// dependency: it isn't ours to hold open on the parent's behalf.
		if existing.Labels[dependencyOfLabel] == parent.Name {
			return r.stampDependencyOrigin(ctx, consumerClient, &existing, parent.Name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to look up dependency entitlement %q: %w", depEntitlementName, err)
	}

	// The Get returned NotFound, but an entitlement for the same dependency
	// service may exist under a different metadata.name (e.g. created by the
	// user directly using the canonical service name). Check by the stamped
	// canonical name via the field index to avoid creating a duplicate.
	var existingByCanonical servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(ctx, &existingByCanonical,
		client.MatchingFields{entitlementServiceNameIndex: depCanonical}); err != nil {
		return fmt.Errorf("failed to list entitlements while checking for duplicate dep %q: %w", depCanonical, err)
	}
	if len(existingByCanonical.Items) > 0 {
		return nil
	}

	depEntitlement := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name: depEntitlementName,
			// Provenance lives on the object, not only on status. Status is
			// written by several reconciles at once; a label set at create
			// time is the one record of "this controller created this to
			// satisfy that parent" that nothing else can race.
			Labels: map[string]string{dependencyOfLabel: parent.Name},
		},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: depSvc.Name},
		},
	}
	if err := consumerClient.Create(ctx, depEntitlement); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create dependency entitlement %q: %w", depEntitlementName, err)
		}
		// Lost a create race; the next pass finds it by name and stamps it.
		return nil
	}

	// Stamp from the object the API server just returned rather than reading
	// it back: the consumer client is cache-backed and the create hasn't
	// reached that cache yet, so a read-back here returns NotFound.
	return r.stampDependencyOrigin(ctx, consumerClient, depEntitlement, parent.Name)
}

// stampDependencyOrigin records on status that this entitlement is held open
// by a parent, which is what delete protection and parent teardown key on.
//
// Patched, not updated: the entitlement's own reconcile writes the whole
// status independently, and an optimistically-locked update loses that race
// often enough to matter. The same derivation runs in setEntitlementStatus
// from the label, so a status write that lands after this one restores it
// rather than reverting it to Direct.
func (r *ServiceEntitlementReconciler) stampDependencyOrigin(ctx context.Context, consumerClient client.Client, dep *servicesv1alpha1.ServiceEntitlement, parentName string) error {
	if dep.Status.Origin == servicesv1alpha1.EntitlementOriginDependency && dep.Status.DependencyOf == parentName {
		return nil
	}
	before := dep.DeepCopy()
	dep.Status.Origin = servicesv1alpha1.EntitlementOriginDependency
	dep.Status.DependencyOf = parentName
	if err := consumerClient.Status().Patch(ctx, dep, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("failed to stamp dependency origin on %q: %w", dep.Name, err)
	}
	return nil
}

// setDependenciesCondition reports dependency enrollment on the parent
// entitlement. Without it a project whose dependency enrollment failed is
// indistinguishable from one that succeeded: Ready is already True by this
// point, because this entitlement's own access really was granted.
func (r *ServiceEntitlementReconciler) setDependenciesCondition(ctx context.Context, consumerClient client.Client, parent *servicesv1alpha1.ServiceEntitlement, declared int, failed []string, errs []error) error {
	cond := metav1.Condition{
		Type:               servicesv1alpha1.ConditionTypeDependenciesSatisfied,
		Status:             metav1.ConditionTrue,
		Reason:             servicesv1alpha1.ReasonDependenciesSatisfied,
		Message:            "Every service this one depends on is enabled in this project.",
		ObservedGeneration: parent.Generation,
	}
	switch {
	case declared == 0:
		cond.Reason = servicesv1alpha1.ReasonNoDependencies
		cond.Message = "This service doesn't depend on any other service."
	case len(failed) > 0:
		reasons := make([]string, 0, len(errs))
		for _, err := range errs {
			reasons = append(reasons, err.Error())
		}
		cond.Status = metav1.ConditionFalse
		cond.Reason = servicesv1alpha1.ReasonDependencyEnrollmentFailed
		cond.Message = fmt.Sprintf("Couldn't enable %s: %s",
			strings.Join(failed, ", "), strings.Join(reasons, "; "))
	}

	before := parent.DeepCopy()
	apimeta.SetStatusCondition(&parent.Status.Conditions, cond)
	if conditionsEqual(before.Status.Conditions, parent.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied) {
		return nil
	}
	if err := consumerClient.Status().Patch(ctx, parent, client.MergeFrom(before),
		client.FieldOwner(dependenciesConditionFieldManager)); err != nil {
		return fmt.Errorf("failed to patch DependenciesSatisfied condition: %w", err)
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
	svc, svcErr := r.resolveService(ctx, entitlement.Spec.ServiceRef.Name)
	if svcErr != nil && !apierrors.IsNotFound(svcErr) {
		return ctrl.Result{}, fmt.Errorf("failed to get Service during finalize: %w", svcErr)
	}
	if svc == nil {
		svc = &servicesv1alpha1.Service{}
	}

	// consumerGated tracks whether we must hold this entitlement's finalizer
	// until the provider confirms teardown. It is set when we (as the last
	// referencing entitlement) delete the shared ServiceConsumer: a provider that
	// projects resources gates the ServiceConsumer's deletion with its own
	// finalizer, so the ServiceConsumer lingers until teardown succeeds. Holding
	// the entitlement finalizer until the ServiceConsumer is gone keeps this
	// project's control plane — and thus the ServiceConsumer — alive for the
	// provider to observe, chaining the teardown guarantee up to project deletion.
	consumerGated := false
	if svc.Spec.Owner.ProducerProjectRef.Name != "" {
		providerCluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(svc.Spec.Owner.ProducerProjectRef.Name))
		if err != nil {
			logger.Info("provider cluster unavailable during finalize, requeuing", "err", err)
			return ctrl.Result{Requeue: true}, nil
		}
		providerClient := providerCluster.GetClient()

		// Reference-count the shared ServiceConsumer before deleting it.
		// Multiple ServiceEntitlements in the same consumer project that
		// reference the same service all map to a single ServiceConsumer
		// (keyed by serviceName + consumerProject in serviceConsumerName).
		// Deleting the consumer while other entitlements still reference the
		// same service would prematurely tear it down — and, for the
		// consumer-scoped provider, trigger a destructive teardown of
		// still-entitled projected resources. Only delete once the LAST
		// referencing entitlement is being finalized. We skip siblings that
		// are themselves terminating so concurrent deletes converge safely.
		var siblings servicesv1alpha1.ServiceEntitlementList
		if err := consumerClient.List(ctx, &siblings); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to list entitlements during finalize: %w", err)
		}
		stillReferenced := false
		for i := range siblings.Items {
			sibling := &siblings.Items[i]
			if sibling.Name == entitlement.Name {
				continue // the entitlement currently being finalized
			}
			if !sibling.DeletionTimestamp.IsZero() {
				continue // a terminating sibling is on its way out too
			}
			if sibling.Spec.ServiceRef.Name != entitlement.Spec.ServiceRef.Name {
				continue // references a different service → different ServiceConsumer
			}
			stillReferenced = true
			break
		}

		if stillReferenced {
			logger.Info("ServiceConsumer still referenced by other entitlements; skipping delete",
				"service", svc.Spec.ServiceName, "consumerProject", consumerProject)
		} else {
			consumerName := serviceConsumerName(svc.Spec.ServiceName, consumerProject)
			consumer := &servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: consumerName}}
			if err := providerClient.Delete(ctx, consumer); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("failed to delete ServiceConsumer %q: %w", consumerName, err)
			}

			// If the ServiceConsumer is still present, the provider's
			// deprovisioning finalizer is holding its deletion until teardown
			// confirms. Re-read to distinguish "gone" (proceed) from "finalizing"
			// (wait). A provider that projects nothing sets no finalizer, so the
			// object is already gone here and this is a NotFound no-op.
			var remaining servicesv1alpha1.ServiceConsumer
			err := providerClient.Get(ctx, types.NamespacedName{Name: consumerName}, &remaining)
			switch {
			case apierrors.IsNotFound(err):
				// Fully torn down; safe to finalize.
			case err != nil:
				return ctrl.Result{}, fmt.Errorf("failed to re-read ServiceConsumer %q during finalize: %w", consumerName, err)
			default:
				consumerGated = true
			}
		}
	}

	// Best-effort cleanup of dependency entitlements that were spawned by this
	// parent. We only delete dependency entitlements whose dependencyOf points
	// at this entitlement; other parents may still need the same dependency.
	//
	// This runs for dependency-origin entitlements too, so a chain unwinds the
	// way it was built: each child's own finalize releases the level below it.
	// dependencyOf is stamped once by whichever entitlement created the child,
	// which already existed at that moment, so the relation is a forest and the
	// cascade terminates.
	var children servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(ctx, &children); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list entitlements during finalize: %w", err)
	}
	for i := range children.Items {
		child := &children.Items[i]
		if child.Name == entitlement.Name {
			continue
		}
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

	// Clean up any ResourceGrants that were provisioned for this entitlement.
	if err := r.pruneQuotaGrants(ctx, consumerClient, entitlement); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to prune quota grants: %w", err)
	}

	// Deprovisioning gate: hold the finalizer until the provider has confirmed
	// teardown, which it signals by dropping its own finalizer and letting the
	// ServiceConsumer be garbage-collected. This keeps the project's control
	// plane alive so the provider can observe and complete teardown, making a
	// completed project deletion proof that no provider resources remain.
	if consumerGated {
		logger.Info("waiting for provider to confirm teardown before finalizing ServiceEntitlement",
			"service", svc.Spec.ServiceName, "consumerProject", consumerProject)
		return ctrl.Result{RequeueAfter: providerTeardownRequeueInterval}, nil
	}

	controllerutil.RemoveFinalizer(entitlement, serviceEntitlementFinalizer)
	if err := consumerClient.Update(ctx, entitlement); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}
	logger.Info("finalized ServiceEntitlement", "name", entitlement.Name)
	return ctrl.Result{}, nil
}

// resolveService looks up a Service by canonical name (spec.serviceName) first,
// falling back to Kubernetes object name for backward-compatibility with
// entitlements created before the canonical-name convention was enforced.
func (r *ServiceEntitlementReconciler) resolveService(ctx context.Context, nameOrCanonical string) (*servicesv1alpha1.Service, error) {
	return resolveService(ctx, r.rootClient, nameOrCanonical)
}

// resolveService looks up a Service by canonical name (spec.serviceName) first,
// falling back to Kubernetes object name for backward-compatibility with
// entitlements created before the canonical-name convention was enforced.
// Shared with ProjectSuspensionPropagationReconciler so both controllers
// resolve a ServiceEntitlement's serviceRef the same way.
func resolveService(ctx context.Context, rootClient client.Client, nameOrCanonical string) (*servicesv1alpha1.Service, error) {
	var list servicesv1alpha1.ServiceList
	if err := rootClient.List(ctx, &list, client.MatchingFields{"spec.serviceName": nameOrCanonical}); err != nil {
		// Propagate transient errors (cache not synced, API unavailable, etc.)
		// rather than silently falling through to the name-based Get, which
		// could return the wrong Service or a misleading NotFound.
		return nil, fmt.Errorf("failed to list Services by spec.serviceName %q: %w", nameOrCanonical, err)
	}
	if len(list.Items) > 0 {
		return &list.Items[0], nil
	}
	// Backward-compat: try by Kubernetes object name.
	var svc servicesv1alpha1.Service
	if err := rootClient.Get(ctx, types.NamespacedName{Name: nameOrCanonical}, &svc); err != nil {
		return nil, err
	}
	return &svc, nil
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
//
// BillingEntitlement quota gating also depends on BillingAccountBinding,
// BillingEntitlement, and Offer objects on the root cluster. Those are
// watched via WatchesRawSource against rootMgr's cache so map funcs can set
// ClusterName to the affected project (mchandler.TypedEnqueueRequestsFromMapFunc
// would overwrite it with the local cluster name).
func (r *ServiceEntitlementReconciler) SetupWithManager(mcMgr mcmanager.Manager, rootMgr ctrl.Manager) error {
	r.rootClient = rootMgr.GetClient()
	r.Manager = mcMgr

	// Index ServiceEntitlement objects by the canonical service name stamped
	// on status.serviceName. The multicluster manager propagates the index to
	// every engaged project cluster. Registered once here; the ServiceConsumer
	// reconciler relies on this index too.
	if err := mcMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&servicesv1alpha1.ServiceEntitlement{},
		entitlementServiceNameIndex,
		entitlementServiceNameIndexer,
	); err != nil {
		return fmt.Errorf("failed to index ServiceEntitlement by %s: %w", entitlementServiceNameIndex, err)
	}

	// Index Service objects by spec.serviceName so entitlements can resolve
	// their serviceRef by canonical name without a full list scan.
	if err := rootMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&servicesv1alpha1.Service{},
		"spec.serviceName",
		func(obj client.Object) []string {
			svc := obj.(*servicesv1alpha1.Service)
			if svc.Spec.ServiceName == "" {
				return nil
			}
			return []string{svc.Spec.ServiceName}
		},
	); err != nil {
		return fmt.Errorf("failed to index Service by spec.serviceName: %w", err)
	}

	// Index ServiceConfiguration objects by spec.serviceRef.name so
	// ensureQuotaGrants can efficiently find the configuration for a given
	// Service without a full list scan.
	if err := rootMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&servicesv1alpha1.ServiceConfiguration{},
		"spec.serviceRef.name",
		func(obj client.Object) []string {
			sc := obj.(*servicesv1alpha1.ServiceConfiguration)
			if sc.Spec.ServiceRef.Name == "" {
				return nil
			}
			return []string{sc.Spec.ServiceRef.Name}
		},
	); err != nil {
		return fmt.Errorf("failed to index ServiceConfiguration by spec.serviceRef.name: %w", err)
	}

	// Billing indexes for quota gating (project → binding → BA → BE → Offer).
	if err := rootMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&billingv1alpha1.BillingAccountBinding{},
		bindingProjectRefIndex,
		func(obj client.Object) []string {
			b := obj.(*billingv1alpha1.BillingAccountBinding)
			if b.Spec.ProjectRef.Name == "" {
				return nil
			}
			return []string{b.Spec.ProjectRef.Name}
		},
	); err != nil {
		return fmt.Errorf("failed to index BillingAccountBinding by %s: %w", bindingProjectRefIndex, err)
	}
	if err := rootMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&billingv1alpha1.BillingAccountBinding{},
		bindingBillingAccountRefIndex,
		func(obj client.Object) []string {
			b := obj.(*billingv1alpha1.BillingAccountBinding)
			if b.Spec.BillingAccountRef.Name == "" {
				return nil
			}
			return []string{b.Spec.BillingAccountRef.Name}
		},
	); err != nil {
		return fmt.Errorf("failed to index BillingAccountBinding by %s: %w", bindingBillingAccountRefIndex, err)
	}
	if err := rootMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&billingv1alpha1.BillingEntitlement{},
		billingEntitlementOfferIndex,
		func(obj client.Object) []string {
			be := obj.(*billingv1alpha1.BillingEntitlement)
			if be.Spec.OfferRef.Name == "" {
				return nil
			}
			return []string{be.Spec.OfferRef.Name}
		},
	); err != nil {
		return fmt.Errorf("failed to index BillingEntitlement by %s: %w", billingEntitlementOfferIndex, err)
	}

	return mcbuilder.ControllerManagedBy(mcMgr).
		Named("service-entitlement").
		For(&servicesv1alpha1.ServiceEntitlement{}, mcbuilder.WithEngageWithProviderClusters(true)).
		WatchesRawSource(source.TypedKind(
			rootMgr.GetCache(),
			&billingv1alpha1.BillingEntitlement{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapBillingEntitlementToServiceEntitlements),
		)).
		WatchesRawSource(source.TypedKind(
			rootMgr.GetCache(),
			&billingv1alpha1.BillingAccountBinding{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapBillingAccountBindingToServiceEntitlements),
		)).
		WatchesRawSource(source.TypedKind(
			rootMgr.GetCache(),
			&billingv1alpha1.Offer{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapOfferToServiceEntitlements),
		)).
		WatchesRawSource(source.TypedKind(
			rootMgr.GetCache(),
			&servicesv1alpha1.ServiceConfiguration{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapServiceConfigurationToServiceEntitlements),
		)).
		WatchesRawSource(source.TypedKind(
			rootMgr.GetCache(),
			&servicesv1alpha1.Service{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapServiceToServiceEntitlements),
			serviceEnrollmentPredicate(),
		)).
		Complete(r)
}
