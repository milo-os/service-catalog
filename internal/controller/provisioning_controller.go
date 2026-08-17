// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/provisioning"
)

const (
	// provisioningResyncInterval bounds how long a change that cannot enqueue a
	// project-scoped reconcile takes to reach consumer projects — chiefly a
	// provider labelling, unlabelling, or deleting a source object in its own
	// project, which no watch here observes. Declaration edits do not wait for
	// it; SetupWithManager watches ServiceConfiguration directly.
	provisioningResyncInterval = 5 * time.Minute

	// provisioningFieldManager identifies writes this reconciler makes, both to
	// projected objects and to the entitlement's provisioning status. It is
	// distinct from the entitlement reconciler's manager so the two do not
	// contend for the ledger and the Ready condition respectively.
	provisioningFieldManager = "services-operator-provisioning"

	// labelProvisionedResource records which spec.provisioning.resources[].name
	// produced an object. Pruning is scoped by it so a declaration that failed
	// to resolve cannot cause another declaration's objects to be removed.
	labelProvisionedResource = "services.miloapis.com/provisioned-resource"

	// labelSourceObject records the source object a projection references,
	// making "why is this here" answerable from the object alone.
	labelSourceObject = "services.miloapis.com/source-object"

	// maxProvisionedObjectsPerResource caps how many objects one declaration
	// may install into one project. Exceeding it is refused and reported, never
	// truncated: a silent truncation is a correctness failure that looks like a
	// working system.
	maxProvisionedObjectsPerResource = 100

	// Provisioned ledger reasons.
	reasonKindNotAllowed           = "KindNotAllowed"
	reasonKindNotServed            = "KindNotServed"
	reasonSelectorEmpty            = "SelectorEmpty"
	reasonSelectorMatchesTooMany   = "SelectorMatchesTooMany"
	reasonSourceProjectUnreachable = "SourceProjectUnreachable"
	reasonSourceListFailed         = "SourceListFailed"
	reasonApplyFailed              = "ApplyFailed"
)

// ProvisioningReconciler installs the resources a service declares in
// spec.provisioning into every project holding an Active ServiceEntitlement for
// it, and removes them when the entitlement stops being Active.
//
// It is a generalization of the location-binding projection rather than a
// second mechanism beside it: same gating on Active, same owner reference to
// the cluster-scoped entitlement, same label-scoped pruning, same periodic
// resync standing in for cross-plane events. The difference is that the target
// kind and the source objects come from a declaration instead of being wired in
// — which is exactly why the allowlist below is re-checked here and not left to
// admission alone.
//
// It runs on the multicluster manager, so each reconcile is scoped to one
// engaged project cluster (req.ClusterName). ServiceConfiguration lives on the
// root cluster and is read through rootClient; source objects live in the
// provider's own project and are read through that project's engaged cluster.
type ProvisioningReconciler struct {
	// rootClient reads cluster-scoped ServiceConfiguration objects from the
	// root key space; they live in no project VCP, so a per-cluster client
	// cannot see them. Mirrors LocationBindingReconciler.
	rootClient client.Client
	Manager    mcmanager.Manager
	Scheme     *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch
// The kinds below are the platform provisioning allowlist. This grant is not
// what bounds provisioning: the operator authenticates with a certificate
// carrying the system:masters organization, so RBAC does not constrain it. The
// binding enforcement is internal/provisioning.Lookup, applied at admission and
// again before every write here. The grant is kept narrow and in step with the
// allowlist anyway, because it documents the intended surface and becomes a
// real ceiling the moment that identity is narrowed.
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=ipclasses,verbs=get;list;watch;create;update;patch;delete

func (r *ProvisioningReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	consumerProject := req.ClusterName
	if consumerProject == "" {
		return ctrl.Result{}, fmt.Errorf("provisioning reconcile invoked without a cluster name")
	}

	consumerCluster, err := r.Manager.GetCluster(ctx, consumerProject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get consumer cluster %q: %w", consumerProject, err)
	}
	consumerClient := consumerCluster.GetClient()

	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(ctx, req.NamespacedName, &entitlement); err != nil {
		if apierrors.IsNotFound(err) {
			// Projected objects carry a controller owner reference to the
			// entitlement and are reclaimed by garbage collection.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ServiceEntitlement: %w", err)
	}

	// Provisioning follows approval; it does not anticipate it. Anything other
	// than Active — pending approval, rejected, deleting — tears down.
	if !entitlement.DeletionTimestamp.IsZero() ||
		entitlement.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		if err := r.pruneAll(ctx, consumerClient, &entitlement); err != nil {
			return ctrl.Result{}, err
		}
		if !entitlement.DeletionTimestamp.IsZero() {
			// A terminating entitlement is not a reporting surface.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.writeStatus(ctx, consumerClient, &entitlement, nil,
			servicesv1alpha1.ReasonEntitlementNotActive,
			"Resources are installed once this service is enabled and any approval has been granted.")
	}

	serviceRefName := entitlement.Spec.ServiceRef.Name

	sc, err := activePublishedConfiguration(ctx, r.rootClient, serviceRefName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if sc == nil {
		// Nothing declares what this project is owed yet. Do not prune on the
		// strength of a lookup that found nothing; retry.
		logger.V(1).Info("no published ServiceConfiguration for service yet, requeuing", "service", serviceRefName)
		return ctrl.Result{RequeueAfter: provisioningResyncInterval}, nil
	}

	serviceName := sc.Status.ServiceName
	if serviceName == "" {
		serviceName = serviceRefName
	}

	if sc.Spec.Provisioning == nil || len(sc.Spec.Provisioning.Resources) == 0 {
		if err := r.pruneAll(ctx, consumerClient, &entitlement); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: provisioningResyncInterval}, r.writeStatus(
			ctx, consumerClient, &entitlement, nil,
			servicesv1alpha1.ReasonNothingToProvision,
			fmt.Sprintf("%s does not install any resources into projects.", serviceName))
	}

	ledger := make([]servicesv1alpha1.ProvisionedResourceStatus, 0, len(sc.Spec.Provisioning.Resources))
	for i := range sc.Spec.Provisioning.Resources {
		decl := &sc.Spec.Provisioning.Resources[i]
		ledger = append(ledger, r.reconcileResource(ctx, consumerClient, &entitlement, serviceName, decl))
	}

	return ctrl.Result{RequeueAfter: provisioningResyncInterval},
		r.writeStatus(ctx, consumerClient, &entitlement, ledger, "", "")
}

// reconcileResource installs one declaration's objects and returns its ledger
// entry. It never returns an error: a failure against one declaration must not
// abandon the others, and every outcome has to be reportable to the consumer.
func (r *ProvisioningReconciler) reconcileResource(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	serviceName string,
	decl *servicesv1alpha1.ProvisionedResourceSpec,
) servicesv1alpha1.ProvisionedResourceStatus {
	kind := decl.Projection.Kind
	entry := servicesv1alpha1.ProvisionedResourceStatus{
		Name: decl.Name,
		Kind: &servicesv1alpha1.GVKRef{Group: kind.Group, Kind: kind.Kind},
	}

	// Allowlist enforcement, second layer. Admission already rejected an
	// unlisted kind, but a ServiceConfiguration admitted under an older or
	// wider allowlist stays in etcd, and the webhook can be absent from the
	// cluster entirely. This check is what actually bounds what gets written.
	allowed, err := provisioning.Lookup(kind)
	if err != nil {
		entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
		entry.Reason = reasonKindNotAllowed
		entry.Message = fmt.Sprintf("%s cannot install %s.%s into this project: %s",
			serviceName, kind.Kind, kind.Group, err.Error())
		return entry
	}

	// An absent or empty selector matches everything in Kubernetes' default
	// conversion. Projecting a provider's entire source project by omission is
	// never the intent and fails silently and widely, so it is refused.
	selector, err := metav1.LabelSelectorAsSelector(&decl.Projection.Selector)
	if err != nil || selector.Empty() {
		entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
		entry.Reason = reasonSelectorEmpty
		entry.Message = fmt.Sprintf("%s declared %q without a selector, so no %s objects were installed.",
			serviceName, decl.Name, kind.Kind)
		return entry
	}

	sourceCluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(decl.Projection.SourceProject))
	if err != nil {
		entry.State = servicesv1alpha1.ProvisionedResourceStateFailed
		entry.Reason = reasonSourceProjectUnreachable
		entry.Message = fmt.Sprintf("%s could not be reached to read the %s objects it offers; retrying.",
			serviceName, kind.Kind)
		return entry
	}

	var sourceList unstructured.UnstructuredList
	sourceList.SetGroupVersionKind(listGVK(allowed))
	if err := sourceCluster.GetClient().List(ctx, &sourceList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		if apimeta.IsNoMatchError(err) {
			entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
			entry.Reason = reasonKindNotServed
			entry.Message = fmt.Sprintf("%s could not install %s because its own project does not serve that kind.",
				serviceName, kind.Kind)
			return entry
		}
		entry.State = servicesv1alpha1.ProvisionedResourceStateFailed
		entry.Reason = reasonSourceListFailed
		entry.Message = fmt.Sprintf("%s could not list the %s objects it offers; retrying.", serviceName, kind.Kind)
		return entry
	}

	if len(sourceList.Items) > maxProvisionedObjectsPerResource {
		entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
		entry.Reason = reasonSelectorMatchesTooMany
		entry.Message = fmt.Sprintf(
			"%s selected %d %s objects for %q, above the limit of %d, so none were installed.",
			serviceName, len(sourceList.Items), kind.Kind, decl.Name, maxProvisionedObjectsPerResource)
		return entry
	}

	desired := make(map[string]struct{}, len(sourceList.Items))
	for i := range sourceList.Items {
		src := &sourceList.Items[i]
		name := provisionedObjectName(serviceName, src.GetName())
		if err := r.upsert(ctx, consumerClient, entitlement, serviceName, decl, allowed, src, name); err != nil {
			if apimeta.IsNoMatchError(err) {
				entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
				entry.Reason = reasonKindNotServed
				entry.Message = fmt.Sprintf(
					"%s could not install %s %q because this project's control plane does not serve that kind.",
					serviceName, kind.Kind, src.GetName())
				return entry
			}
			entry.State = servicesv1alpha1.ProvisionedResourceStateFailed
			entry.Reason = reasonApplyFailed
			entry.Message = fmt.Sprintf("%s could not install %s %q: %v", serviceName, kind.Kind, src.GetName(), err)
			return entry
		}
		desired[name] = struct{}{}
	}

	// Pruning happens only after the source list succeeded, and is scoped to
	// this declaration, so a declaration that could not resolve never causes
	// another's objects to be removed.
	if err := r.prune(ctx, consumerClient, entitlement, allowed, decl.Name, desired); err != nil {
		entry.State = servicesv1alpha1.ProvisionedResourceStateFailed
		entry.Reason = reasonApplyFailed
		entry.Message = fmt.Sprintf("%s could not remove withdrawn %s objects: %v", serviceName, kind.Kind, err)
		return entry
	}

	entry.State = servicesv1alpha1.ProvisionedResourceStateInstalled
	entry.ObjectCount = int32(len(desired))

	// AUTHORIZATION GAP — deliberately reported, not worked around.
	//
	// Where the target API authorizes the reference itself, it does so against
	// whoever creates the object. That is this operator, whose certificate
	// carries the system:masters organization, so the check passes trivially
	// and the consumer project never actually holds the permission. The objects
	// below exist and function on the strength of the installer's authority
	// rather than the consumer's.
	//
	// This is not closed by anything in this version. Closing it requires the
	// platform to establish a real grant — a platform-authored IAM binding, in
	// a separate typed fan-out where the provider names only its own resources
	// and the platform chooses the subject, scope, and verb — so that the
	// target API's own check would independently accept the write. Until then
	// the fact is recorded on the ledger rather than left implicit, because
	// revoking access later does not undo it: the check runs at create, not at
	// use, so an already-installed reference keeps working.
	entry.AuthorizationEstablished = !allowed.TargetAPIAuthorizesSource
	if allowed.TargetAPIAuthorizesSource {
		entry.Message = allowed.AuthorizationCaveat
	}
	return entry
}

// upsert writes one projected object. The consumer-facing object is a reference
// and nothing else: it carries no copied data, so nothing has to propagate when
// the source changes and a consumer cannot repoint it at another provider's
// objects.
func (r *ProvisioningReconciler) upsert(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	serviceName string,
	decl *servicesv1alpha1.ProvisionedResourceSpec,
	allowed provisioning.AllowedKind,
	source *unstructured.Unstructured,
	name string,
) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(objectGVK(allowed))
	u.SetName(name)

	_, err := controllerutil.CreateOrUpdate(ctx, consumerClient, u, func() error {
		l := u.GetLabels()
		if l == nil {
			l = map[string]string{}
		}
		l[labelManagedBy] = labelManagedByValue
		l[labelServiceName] = encodeName(serviceName)
		l[labelEntitlementName] = entitlement.Name
		l[labelProvisionedResource] = decl.Name
		l[labelSourceObject] = source.GetName()
		u.SetLabels(l)

		// The cluster-scoped ServiceEntitlement owns what was provisioned for
		// it, so deleting the entitlement garbage-collects these objects. This
		// is the only teardown path that does not depend on the project purger,
		// which does not delete cluster-scoped resources.
		if err := controllerutil.SetControllerReference(entitlement, u, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return unstructured.SetNestedMap(u.Object,
			allowed.ReferenceSpec(decl.Projection.SourceProject, source.GetName()), "spec")
	})
	return err
}

// prune deletes objects this declaration previously installed that the source
// selector no longer resolves to.
//
// The query is scoped by service and entitlement as well as managed-by. Pruning
// on the shared managed-by label alone would let one service's fan-out remove
// another's objects.
func (r *ProvisioningReconciler) prune(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	allowed provisioning.AllowedKind,
	declName string,
	keep map[string]struct{},
) error {
	match := client.MatchingLabels{
		labelManagedBy:           labelManagedByValue,
		labelEntitlementName:     entitlement.Name,
		labelProvisionedResource: declName,
	}

	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(listGVK(allowed))
	if err := consumerClient.List(ctx, &list, match); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to list provisioned %s objects: %w", allowed.Kind, err)
	}

	for i := range list.Items {
		item := &list.Items[i]
		if !ownedBy(item.GetOwnerReferences(), entitlement.UID) {
			continue
		}
		if _, ok := keep[item.GetName()]; ok {
			continue
		}
		if err := consumerClient.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete withdrawn %s %q: %w", allowed.Kind, item.GetName(), err)
		}
	}
	return nil
}

// pruneAll removes everything provisioned for this entitlement, across every
// allowlisted kind.
//
// It sweeps the whole allowlist rather than only the kinds the current
// configuration declares, so that removing a declaration — or removing a kind
// from the allowlist — still tears down what it installed.
func (r *ProvisioningReconciler) pruneAll(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
) error {
	for _, allowed := range provisioning.All() {
		var list unstructured.UnstructuredList
		list.SetGroupVersionKind(listGVK(allowed))
		if err := consumerClient.List(ctx, &list, client.MatchingLabels{
			labelManagedBy:       labelManagedByValue,
			labelEntitlementName: entitlement.Name,
		}); err != nil {
			if apimeta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("failed to list provisioned %s objects: %w", allowed.Kind, err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			if !ownedBy(item.GetOwnerReferences(), entitlement.UID) {
				continue
			}
			if err := consumerClient.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete provisioned %s %q: %w", allowed.Kind, item.GetName(), err)
			}
		}
	}
	return nil
}

// writeStatus records the ledger and the Provisioned condition on the
// entitlement, in the consumer's own control plane.
//
// The condition is patched rather than written with the rest of the status:
// ServiceEntitlementReconciler owns Ready and ProjectSuspensionPropagation
// owns Suspended on the same object, and a full status update would have the
// three clobbering each other. Ready is deliberately untouched here — a
// delivery failure is not a denial of access.
func (r *ProvisioningReconciler) writeStatus(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	ledger []servicesv1alpha1.ProvisionedResourceStatus,
	overrideReason, overrideMessage string,
) error {
	before := entitlement.DeepCopy()

	sort.Slice(ledger, func(i, j int) bool { return ledger[i].Name < ledger[j].Name })
	entitlement.Status.ProvisionedResources = ledger
	now := metav1.Now()
	entitlement.Status.LastProvisioningEvaluation = &now

	status := metav1.ConditionTrue
	reason, message := overrideReason, overrideMessage
	if reason == "" {
		installed := 0
		var firstProblem string
		for _, e := range ledger {
			if e.State == servicesv1alpha1.ProvisionedResourceStateInstalled {
				installed++
				continue
			}
			if firstProblem == "" {
				firstProblem = e.Message
			}
		}
		switch {
		case installed == len(ledger):
			reason = servicesv1alpha1.ReasonProvisioned
			message = fmt.Sprintf("%d of %d declared resources installed.", installed, len(ledger))
		case installed == 0:
			status = metav1.ConditionFalse
			reason = servicesv1alpha1.ReasonNotProvisioned
			message = firstProblem
		default:
			status = metav1.ConditionFalse
			reason = servicesv1alpha1.ReasonPartiallyProvisioned
			message = fmt.Sprintf("%d of %d declared resources installed. %s",
				installed, len(ledger), firstProblem)
		}
	} else if overrideReason == servicesv1alpha1.ReasonEntitlementNotActive {
		status = metav1.ConditionFalse
	}

	apimeta.SetStatusCondition(&entitlement.Status.Conditions, metav1.Condition{
		Type:               servicesv1alpha1.ConditionTypeProvisioned,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: entitlement.Generation,
	})

	if err := consumerClient.Status().Patch(ctx, entitlement, client.MergeFrom(before),
		client.FieldOwner(provisioningFieldManager)); err != nil {
		return fmt.Errorf("failed to patch ServiceEntitlement provisioning status: %w", err)
	}
	return nil
}

// provisionedObjectName derives the installed object's name from the canonical
// service name and the source object.
//
// The provider does not choose it. Two services therefore cannot contend for
// one object, and a service cannot shadow a name a consumer already relies on.
// Note this differs from the worked example in the enhancement, which shows the
// source name reused verbatim; the derived form is what the document's own
// naming control requires, and collision-freedom was judged to outrank the
// nicer name.
func provisionedObjectName(serviceName, sourceName string) string {
	return encodeName(serviceName) + "-" + encodeName(sourceName)
}

func objectGVK(a provisioning.AllowedKind) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: a.Group, Version: a.Version, Kind: a.Kind}
}

func listGVK(a provisioning.AllowedKind) schema.GroupVersionKind {
	return objectGVK(a).GroupVersion().WithKind(a.Kind + "List")
}

// activePublishedConfiguration returns the ServiceConfiguration that governs a
// service: the most recently created Published one, breaking ties on the higher
// name for determinism.
//
// The two existing fan-outs disagree on this — the location reconciler selects
// the latest Published, the quota fan-out takes the first Published in list
// order. This follows the location rule, because it is deterministic and
// independent of list ordering whereas "first returned" is neither, and because
// this reconciler is a generalization of that one. Reconciling the quota
// fan-out onto the same rule is a separate change and is not made here.
func activePublishedConfiguration(
	ctx context.Context,
	rootClient client.Client,
	serviceRefName string,
) (*servicesv1alpha1.ServiceConfiguration, error) {
	var list servicesv1alpha1.ServiceConfigurationList
	if err := rootClient.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("failed to list ServiceConfigurations: %w", err)
	}
	var latest *servicesv1alpha1.ServiceConfiguration
	for i := range list.Items {
		sc := &list.Items[i]
		if sc.Spec.ServiceRef.Name != serviceRefName || sc.Spec.Phase != servicesv1alpha1.PhasePublished {
			continue
		}
		if latest == nil || moreRecent(sc, latest) {
			latest = sc
		}
	}
	return latest, nil
}

// SetupWithManager registers the reconciler on the multicluster manager.
//
// The primary watch is ServiceEntitlement, scoped to engaged project clusters.
// A root-cluster watch on ServiceConfiguration is added on top, because a
// provider editing its declaration is a first-class trigger: without it a
// selector change would only reach entitled projects on the next resync, up to
// provisioningResyncInterval later. The map function fans a root event out into
// project-scoped requests, which is why it needs the root manager's cache
// directly — mchandler would overwrite ClusterName with the local cluster.
//
// Source objects in provider projects still have no path to enqueue a consumer
// request, so a newly labelled source object converges on the resync.
func (r *ProvisioningReconciler) SetupWithManager(mgr mcmanager.Manager, rootMgr ctrl.Manager) error {
	r.rootClient = rootMgr.GetClient()
	r.Manager = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		Named("service-provisioning").
		For(&servicesv1alpha1.ServiceEntitlement{}, mcbuilder.WithEngageWithProviderClusters(true)).
		WatchesRawSource(source.TypedKind(
			rootMgr.GetCache(),
			&servicesv1alpha1.ServiceConfiguration{},
			handler.TypedEnqueueRequestsFromMapFunc(
				func(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) []mcreconcile.Request {
					return enqueueEntitlementsForConfiguration(ctx, r.rootClient, r.Manager, sc)
				}),
		)).
		Complete(r)
}
