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
	// provisioningResyncInterval bounds how long a change no watch here observes
	// takes to reach consumer projects: a provider labelling, unlabelling, or
	// deleting a source object in its own project. Declaration edits do not
	// wait for it, because SetupWithManager watches ServiceConfiguration.
	provisioningResyncInterval = 5 * time.Minute

	// provisioningFieldManager identifies writes this reconciler makes, to
	// projected objects and to the entitlement's provisioning status. It
	// differs from the entitlement reconciler's manager so the two do not
	// contend over the ledger and the Ready condition.
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
	// truncated: a truncated fan-out looks like a working system.
	maxProvisionedObjectsPerResource = 100

	// Provisioned ledger reasons.
	reasonProjectionInvalid        = "ProjectionInvalid"
	reasonSourceProjectNotOwned    = "SourceProjectNotOwned"
	reasonKindNotServed            = "KindNotServed"
	reasonSelectorEmpty            = "SelectorEmpty"
	reasonSelectorMatchesTooMany   = "SelectorMatchesTooMany"
	reasonSourceProjectUnreachable = "SourceProjectUnreachable"
	reasonSourceListFailed         = "SourceListFailed"
	reasonApplyFailed              = "ApplyFailed"

	// authorizationCaveat explains the gap to the consumer, on every installed
	// resource. It reaches the entitlement ledger, so the gap is visible in a
	// running system rather than only in a design document.
	authorizationCaveat = "This reference was installed by the platform, which is authorized to use " +
		"the source object. The project itself was not granted use of it, so the reference works on " +
		"the platform's authority rather than the project's."
)

// ProvisioningReconciler installs the resources a service declares in
// spec.provisioning into every project holding an Active ServiceEntitlement for
// it, and removes them when the entitlement stops being Active.
//
// It generalizes the location-binding projection rather than sitting beside it:
// same gating on Active, same owner reference to the cluster-scoped
// entitlement, same label-scoped pruning, same periodic resync standing in for
// cross-plane events. Only the target kind and the source objects differ,
// coming from a declaration instead of being wired in. That is why this
// controller re-checks the declaration rather than trusting admission.
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
// This controller holds no per-kind RBAC grants, and could not: the kinds are
// named by providers at runtime and the operator's certificate carries
// system:masters, so RBAC bounds nothing here. What bounds it is the shape of a
// projection and the provider's ownership of the source project, both checked
// at admission and again before every write. Whether a given reference is
// acceptable is the target API's own decision, made when it rejects the write.

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

	// Provisioning follows approval; it does not anticipate it. Any phase other
	// than Active tears down: pending approval, rejected, deleting.
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

	// A projection may read only out of the project the service is published
	// from. Admission enforces it too; it is re-derived here because that is
	// the check a document already in etcd can have been admitted without.
	var svc servicesv1alpha1.Service
	if err := r.rootClient.Get(ctx, client.ObjectKey{Name: serviceRefName}, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get Service %q: %w", serviceRefName, err)
	}
	producerProject := svc.Spec.Owner.ProducerProjectRef.Name

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
		ledger = append(ledger, r.reconcileResource(
			ctx, consumerClient, &entitlement, serviceName, producerProject, decl))
	}

	// Objects installed by a declaration that is no longer there. The
	// entitlement's own ledger is what remembers them, so a withdrawn
	// declaration is still torn down without the platform holding a list of
	// every kind provisioning has ever written.
	if err := r.pruneUndeclared(ctx, consumerClient, &entitlement, sc.Spec.Provisioning.Resources); err != nil {
		return ctrl.Result{}, err
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
	producerProject string,
	decl *servicesv1alpha1.ProvisionedResourceSpec,
) servicesv1alpha1.ProvisionedResourceStatus {
	kind := decl.Projection.Kind
	entry := servicesv1alpha1.ProvisionedResourceStatus{
		Name: decl.Name,
		Kind: &servicesv1alpha1.ProjectedKindRef{Group: kind.Group, Version: kind.Version, Kind: kind.Kind},
	}

	// Second enforcement point. Admission already refused a declaration that
	// does not resolve, but one admitted under an earlier schema stays in etcd
	// and the webhook can be absent from the cluster.
	projection, err := provisioning.Resolve(decl.Projection)
	if err != nil {
		entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
		entry.Reason = reasonProjectionInvalid
		entry.Message = fmt.Sprintf("%s cannot install %s.%s into this project: %s",
			serviceName, kind.Kind, kind.Group, err.Error())
		return entry
	}

	// The other half of that check. A service offers only what it holds in a
	// project it owns; without this the platform would read a project the
	// provider was never granted, with an identity nothing stops.
	if decl.Projection.SourceProject != producerProject {
		entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
		entry.Reason = reasonSourceProjectNotOwned
		entry.Message = fmt.Sprintf(
			"%s declared %q reading out of %q, which is not the project it is published from, so nothing was installed.",
			serviceName, decl.Name, decl.Projection.SourceProject)
		return entry
	}

	// Kubernetes converts an absent or empty selector to "match everything".
	// Projecting a provider's whole source project by omission fails silently
	// and widely, so it is refused.
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
	sourceList.SetGroupVersionKind(projection.ListGVK())
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
		if err := r.upsert(ctx, consumerClient, entitlement, serviceName, decl, projection, src, name); err != nil {
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

	// Pruning runs only after the source list succeeded, scoped to this
	// declaration, so a declaration that could not resolve never removes
	// another's objects.
	if err := r.prune(ctx, consumerClient, entitlement, projection.GVK, decl.Name, desired); err != nil {
		entry.State = servicesv1alpha1.ProvisionedResourceStateFailed
		entry.Reason = reasonApplyFailed
		entry.Message = fmt.Sprintf("%s could not remove withdrawn %s objects: %v", serviceName, kind.Kind, err)
		return entry
	}

	entry.State = servicesv1alpha1.ProvisionedResourceStateInstalled
	entry.ObjectCount = int32(len(desired))

	// AUTHORIZATION GAP: reported, not closed.
	//
	// A target API that authorizes the reference does so against whoever
	// creates the object. That is this operator, running as system:masters, so
	// the check passes and the consumer project never holds the permission.
	// These objects work on the installer's authority, not the consumer's.
	//
	// Closing the gap needs a platform-authored IAM binding, from a separate
	// typed fan-out where the provider names only its own resources and the
	// platform chooses the subject, scope, and verb. The target API's own check
	// would then accept the write independently. Until then the ledger records
	// the gap, because revoking access later changes nothing: the check runs at
	// create, not at use, so an installed reference keeps working.
	//
	// It is unconditional. Nothing establishes a consumer grant for any kind,
	// so claiming otherwise for a kind that happens not to check would report
	// on the target API's rigour rather than on what the platform did.
	entry.AuthorizationEstablished = false
	entry.Message = authorizationCaveat
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
	projection provisioning.Projection,
	source *unstructured.Unstructured,
	name string,
) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(projection.GVK)
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
			projection.Spec(decl.Projection.SourceProject, source.GetName()), "spec")
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
	gvk schema.GroupVersionKind,
	declName string,
	keep map[string]struct{},
) error {
	match := client.MatchingLabels{
		labelManagedBy:           labelManagedByValue,
		labelEntitlementName:     entitlement.Name,
		labelProvisionedResource: declName,
	}

	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(provisioning.ListGVK(gvk))
	if err := consumerClient.List(ctx, &list, match); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to list provisioned %s objects: %w", gvk.Kind, err)
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
			return fmt.Errorf("failed to delete withdrawn %s %q: %w", gvk.Kind, item.GetName(), err)
		}
	}
	return nil
}

// pruneAll removes everything provisioned for this entitlement.
//
// The kinds come from the entitlement's own ledger, not from the configuration:
// teardown runs when the entitlement stops being Active, which includes the
// case where nothing declares anything any more. What was installed here is
// what this project recorded receiving.
func (r *ProvisioningReconciler) pruneAll(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
) error {
	return r.sweep(ctx, consumerClient, entitlement, ledgerKinds(entitlement), nil)
}

// pruneUndeclared removes objects installed by a declaration the configuration
// no longer carries. Per-declaration pruning cannot: it is scoped by the
// declaration's own label, and a withdrawn declaration is not iterated.
func (r *ProvisioningReconciler) pruneUndeclared(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	declared []servicesv1alpha1.ProvisionedResourceSpec,
) error {
	keep := make(map[string]struct{}, len(declared))
	kinds := make(map[schema.GroupVersionKind]struct{}, len(declared))
	for i := range declared {
		keep[declared[i].Name] = struct{}{}
		// A declaration that does not resolve installed nothing, and naming a
		// versionless kind here would make the sweep itself fail.
		if kind := declared[i].Projection.Kind; kind.Version != "" {
			kinds[schema.GroupVersionKind{Group: kind.Group, Version: kind.Version, Kind: kind.Kind}] = struct{}{}
		}
	}
	// The ledger still describes the previous reconcile at this point, so a
	// declaration withdrawn since is named there and nowhere else.
	for _, gvk := range ledgerKinds(entitlement) {
		kinds[gvk] = struct{}{}
	}

	all := make([]schema.GroupVersionKind, 0, len(kinds))
	for gvk := range kinds {
		all = append(all, gvk)
	}
	return r.sweep(ctx, consumerClient, entitlement, all, keep)
}

// sweep deletes every object this entitlement owns, of the given kinds, whose
// declaration is not in keep. A nil keep removes everything.
func (r *ProvisioningReconciler) sweep(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	kinds []schema.GroupVersionKind,
	keep map[string]struct{},
) error {
	for _, gvk := range kinds {
		var list unstructured.UnstructuredList
		list.SetGroupVersionKind(provisioning.ListGVK(gvk))
		if err := consumerClient.List(ctx, &list, client.MatchingLabels{
			labelManagedBy:       labelManagedByValue,
			labelEntitlementName: entitlement.Name,
		}); err != nil {
			if apimeta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("failed to list provisioned %s objects: %w", gvk.Kind, err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			if !ownedBy(item.GetOwnerReferences(), entitlement.UID) {
				continue
			}
			if _, ok := keep[item.GetLabels()[labelProvisionedResource]]; ok {
				continue
			}
			if err := consumerClient.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete provisioned %s %q: %w", gvk.Kind, item.GetName(), err)
			}
		}
	}
	return nil
}

// ledgerKinds is what this project recorded receiving, deduplicated.
func ledgerKinds(entitlement *servicesv1alpha1.ServiceEntitlement) []schema.GroupVersionKind {
	seen := make(map[schema.GroupVersionKind]struct{})
	var out []schema.GroupVersionKind
	for _, entry := range entitlement.Status.ProvisionedResources {
		if entry.Kind == nil || entry.Kind.Version == "" {
			continue
		}
		gvk := schema.GroupVersionKind{Group: entry.Kind.Group, Version: entry.Kind.Version, Kind: entry.Kind.Kind}
		if _, dup := seen[gvk]; dup {
			continue
		}
		seen[gvk] = struct{}{}
		out = append(out, gvk)
	}
	return out
}

// writeStatus records the ledger and the Provisioned condition on the
// entitlement, in the consumer's own control plane.
//
// The condition is patched rather than written with the rest of the status:
// ServiceEntitlementReconciler owns Ready and ProjectSuspensionPropagation owns
// Suspended on the same object, so a full status update would have the three
// clobbering each other. Ready stays untouched here, because a delivery failure
// is not a denial of access.
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
func provisionedObjectName(serviceName, sourceName string) string {
	return encodeName(serviceName) + "-" + encodeName(sourceName)
}

// activePublishedConfiguration returns the ServiceConfiguration that governs a
// service: the most recently created Published one, breaking ties on the higher
// name for determinism.
//
// The two existing fan-outs disagree: the location reconciler selects the
// latest Published, the quota fan-out takes the first Published in list order.
// This follows the location rule, which does not depend on list ordering, and
// which this reconciler generalizes. Moving the quota fan-out onto it is a
// separate change.
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
// A root-cluster watch on ServiceConfiguration sits on top: without it a
// selector change would reach entitled projects only on the next resync, up to
// provisioningResyncInterval later. The map function fans a root event into
// project-scoped requests, so it needs the root manager's cache directly;
// mchandler would overwrite ClusterName with the local cluster.
//
// Source objects in provider projects cannot enqueue a consumer request, so a
// newly labelled source object converges on the resync.
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
