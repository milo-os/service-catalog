// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/flowcontrol"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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

const labelServiceName = "services.miloapis.com/service-name"

const (
	// provisioningResyncInterval bounds how long a change no watch here observes
	// takes to reach consumer projects. Declaration edits do not wait for it,
	// because SetupWithManager watches ServiceConfiguration.
	provisioningResyncInterval = 5 * time.Minute

	// provisioningHeartbeatInterval caps how stale LastProvisioningEvaluation
	// may get when nothing else changed, so a dead fan-out stays detectable
	// without patching on every five-minute resync.
	provisioningHeartbeatInterval = time.Hour

	// provisioningFailureRetryInitial is how soon a declaration that failed to
	// install is tried again. Failures right after enablement are usually brief:
	// the owning service's API or webhook may not have reached the new project
	// yet. Each further failure doubles the wait, up to provisioningResyncInterval.
	provisioningFailureRetryInitial = 2 * time.Second

	// provisioningMaxConcurrentReconciles is how many entitlements this
	// controller provisions at once. A reconcile spends most of its time on API
	// round-trips to one consumer project; running them serially lets a slow or
	// rate-limited project hold up unrelated projects behind it.
	//
	// Five matches the other project fan-out controllers and keeps the extra API
	// pressure modest. Parallel reconciles remain isolated: every status patch
	// targets its triggering entitlement, while object pruning is restricted by
	// that entitlement's name and controller owner UID. Server-side apply makes
	// repeated writes atomic and idempotent.
	provisioningMaxConcurrentReconciles = 5

	// provisioningFieldManager identifies writes this reconciler makes, to
	// installed objects and to the entitlement's provisioning status. It
	// differs from the entitlement reconciler's manager so the two do not
	// contend over the ledger and the Ready condition.
	provisioningFieldManager = "services-operator-provisioning"

	// labelProvisionedResource records which spec.provisioning.resources[].name
	// produced an object. Pruning is scoped by it so a declaration that failed
	// to decode cannot cause another declaration's objects to be removed.
	//
	// With the service and entitlement labels beside it, this is what answers
	// "why is this object here": the provider no longer has to encode the answer
	// in the name, because it chooses the name.
	labelProvisionedResource = "services.miloapis.com/provisioned-resource"

	// maxProvisionedObjectsPerResource caps how many objects one declaration
	// may install into one project. It matches the schema's MaxItems and is
	// re-checked here for a document admitted under an earlier schema.
	// Exceeding it is refused and reported, never truncated: a truncated
	// fan-out looks like a working system.
	maxProvisionedObjectsPerResource = 32

	// Provisioned ledger reasons.
	reasonObjectInvalid  = "ObjectInvalid"
	reasonTooManyObjects = "TooManyObjects"
	reasonKindNotServed  = "KindNotServed"
	reasonApplyFailed    = "ApplyFailed"

	// reasonObjectExists marks a declaration with at least one object whose
	// name is already taken in the project by something the platform did not
	// install for this entitlement. That object is left exactly as it is; the
	// declaration's other objects still install.
	reasonObjectExists = "ObjectExists"

	// reasonObjectAdopted marks a declaration that manages an object which
	// existed before this entitlement did. Earlier versions of this controller
	// took such objects over. They are reported rather than released, because
	// releasing them would itself be an ownership change nobody asked for.
	reasonObjectAdopted = "ObjectAdopted"

	// reasonStatusNotApplied marks a declaration whose objects are installed
	// but whose declared status did not reach at least one of them. The state
	// stays Installed, because delivery succeeded and only the status is
	// missing.
	reasonStatusNotApplied = "StatusNotApplied"

	// maxLedgerMessageBytes matches the schema's MaxLength for a ledger entry's
	// message. A declaration that installs many objects collects one note per
	// object, and a failure message embeds the API server's own error; either
	// can exceed the limit. The API server refuses an over-long message, which
	// loses the whole ledger write, so writeStatus truncates every entry.
	maxLedgerMessageBytes = 1024

	// authorizationCaveat explains the gap to the consumer, on every installed
	// resource. It reaches the entitlement ledger, so the gap is visible in a
	// running system rather than only in a design document.
	authorizationCaveat = "This resource was installed by the platform, which is authorized to use " +
		"the objects it references. The project itself was not granted use of them, so the resource " +
		"works on the platform's authority rather than the project's."
)

// ProvisioningReconciler installs the resources a service declares in
// spec.provisioning into every project holding an Active ServiceEntitlement for
// it, and removes them when the entitlement stops being Active.
//
// It generalizes the location-binding projection rather than sitting beside it:
// same gating on Active, same owner reference to the cluster-scoped
// entitlement, same label-scoped pruning, same periodic resync standing in for
// cross-plane events. Only the objects differ, coming from a declaration
// instead of being wired in. That is why this controller re-checks the
// declaration rather than trusting admission.
//
// It runs on the multicluster manager, so each reconcile is scoped to one
// engaged project cluster (req.ClusterName). ServiceConfiguration lives on the
// root cluster and is read through rootClient.
type ProvisioningReconciler struct {
	// rootClient reads cluster-scoped ServiceConfiguration objects from the
	// root key space; they live in no project VCP, so a per-cluster client
	// cannot see them. Mirrors LocationProjectionReconciler.
	rootClient    client.Client
	rootAPIReader client.Reader
	Manager       mcmanager.Manager
	Scheme        *runtime.Scheme

	// failureRetries paces retries of entitlements whose ledger holds a Failed
	// declaration, keyed by entitlement UID.
	failureRetries     *flowcontrol.Backoff
	failureRetriesOnce sync.Once
}

// failureRetryDelay records another failed pass for the entitlement and
// returns how long to wait before the next one.
func (r *ProvisioningReconciler) failureRetryDelay(uid types.UID) time.Duration {
	r.failureRetriesOnce.Do(r.initFailureRetries)
	r.failureRetries.Next(string(uid), r.failureRetries.Clock.Now())
	return r.failureRetries.Get(string(uid))
}

// clearFailureRetry forgets an entitlement's failure history once nothing it
// declares is failing, so a later failure starts again from the short delay.
func (r *ProvisioningReconciler) clearFailureRetry(uid types.UID) {
	r.failureRetriesOnce.Do(r.initFailureRetries)
	r.failureRetries.Reset(string(uid))
	r.failureRetries.GC()
}

func (r *ProvisioningReconciler) initFailureRetries() {
	r.failureRetries = flowcontrol.NewBackOff(provisioningFailureRetryInitial, provisioningResyncInterval)
}

func hasFailedResource(ledger []servicesv1alpha1.ProvisionedResourceStatus) bool {
	for i := range ledger {
		if ledger[i].State == servicesv1alpha1.ProvisionedResourceStateFailed {
			return true
		}
	}
	return false
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=resourcemanager.miloapis.com,resources=projects,verbs=get
// This controller holds no per-kind RBAC grants, and could not: the kinds are
// named by providers at runtime and the operator's certificate carries
// system:masters, so RBAC bounds nothing here. What bounds it is where the
// objects go — only projects that created an entitlement, only once it is
// Active — and the shape each object must take, both checked at admission and
// again before every write. Whether a given object is acceptable is the owning
// API's own decision, made when it accepts or rejects the write. The status
// subresource works the same way. There is no grant to add for it, because
// there is no grant here for the kinds themselves.

func (r *ProvisioningReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	consumerProject := req.ClusterName
	if consumerProject == "" {
		return ctrl.Result{}, fmt.Errorf("provisioning reconcile invoked without a cluster name")
	}

	project, err := getConsumerProject(ctx, r.rootClient, r.rootAPIReader, string(consumerProject))
	if err != nil {
		return ctrl.Result{}, err
	}

	consumerCluster, err := r.Manager.GetCluster(ctx, consumerProject)
	if err != nil {
		if errors.Is(err, multicluster.ErrClusterNotFound) {
			// The queued entitlement may be deleting and need explicit pruning.
			// Retry every confirmed Project until its control plane is reachable;
			// only a confirmed-absent Project is safe to forget.
			if project != nil {
				return ctrl.Result{RequeueAfter: projectLifecycleRetryInterval}, nil
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get consumer cluster %q: %w", consumerProject, err)
	}
	consumerClient := consumerCluster.GetClient()
	// Whether a name is already taken decides whether the platform may write
	// to it at all, so it is read live: a lagging cache would hand a customer's
	// object to the apply that follows.
	consumerReader := consumerCluster.GetAPIReader()

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

	if project == nil || !project.DeletionTimestamp.IsZero() {
		logger.V(1).Info("project is absent or terminating; skipping provisioning")
		return ctrl.Result{}, nil
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

	// What this project recorded receiving last time, so a declaration that
	// renamed or re-kinded an object still withdraws the old one.
	installed := installedKinds(&entitlement)

	ledger := make([]servicesv1alpha1.ProvisionedResourceStatus, 0, len(sc.Spec.Provisioning.Resources))
	for i := range sc.Spec.Provisioning.Resources {
		decl := &sc.Spec.Provisioning.Resources[i]
		ledger = append(ledger, r.reconcileResource(
			ctx, consumerClient, consumerReader, &entitlement, serviceName, decl, installed[decl.Name]))
	}

	// Objects installed by a declaration that is no longer there. The
	// entitlement's own ledger is what remembers them, so a withdrawn
	// declaration is still torn down without the platform holding a list of
	// every kind provisioning has ever written.
	if err := r.pruneUndeclared(ctx, consumerClient, &entitlement, sc.Spec.Provisioning.Resources); err != nil {
		return ctrl.Result{}, err
	}

	// A failed install is retried on its own short schedule. Waiting for the
	// resync left setup stalled for minutes behind failures that cleared within
	// seconds, because nothing else re-triggers this entitlement once it settles.
	requeue := provisioningResyncInterval
	if hasFailedResource(ledger) {
		requeue = r.failureRetryDelay(entitlement.UID)
	} else {
		r.clearFailureRetry(entitlement.UID)
	}
	return ctrl.Result{RequeueAfter: requeue},
		r.writeStatus(ctx, consumerClient, &entitlement, ledger, "", "")
}

// reconcileResource installs one declaration's objects and returns its ledger
// entry. It never returns an error: a failure against one declaration must not
// abandon the others, and every outcome has to be reportable to the consumer.
//
// installed is what this declaration put in this project on the last pass. It
// is carried on any path that does not converge, so a declaration that stops
// resolving does not erase the record teardown depends on.
func (r *ProvisioningReconciler) reconcileResource(
	ctx context.Context,
	consumerClient client.Client,
	consumerReader client.Reader,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	serviceName string,
	decl *servicesv1alpha1.ProvisionedResourceSpec,
	installed []schema.GroupVersionKind,
) servicesv1alpha1.ProvisionedResourceStatus {
	entry := servicesv1alpha1.ProvisionedResourceStatus{Name: decl.Name, Kinds: kindRefs(installed)}

	if len(decl.Objects) > maxProvisionedObjectsPerResource {
		entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
		entry.Reason = reasonTooManyObjects
		entry.Message = fmt.Sprintf(
			"%s declared %d objects for %q, above the limit of %d, so none were installed.",
			serviceName, len(decl.Objects), decl.Name, maxProvisionedObjectsPerResource)
		return entry
	}

	// Second enforcement point. Admission already refused an object the
	// platform will not write, but one admitted under an earlier schema stays
	// in etcd and the webhook can be absent from the cluster.
	objects := make([]provisioning.Object, 0, len(decl.Objects))
	for i := range decl.Objects {
		obj, err := provisioning.Decode(decl.Objects[i].RawExtension)
		if err != nil {
			entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
			entry.Reason = reasonObjectInvalid
			entry.Message = fmt.Sprintf("%s cannot install object %d of %q into this project: %s",
				serviceName, i+1, decl.Name, err.Error())
			return entry
		}
		objects = append(objects, obj)
	}

	var statusNotes, adoptedNotes, conflicts []string
	desired := make(map[schema.GroupVersionKind]map[string]struct{}, len(objects))
	for _, obj := range objects {
		result, err := r.upsert(ctx, consumerClient, consumerReader, entitlement, serviceName, decl.Name, obj)
		if result.statusNote != "" {
			statusNotes = append(statusNotes, result.statusNote)
		}
		if result.adoptedNote != "" {
			adoptedNotes = append(adoptedNotes, result.adoptedNote)
		}
		var exists *objectExistsError
		if errors.As(err, &exists) {
			// Not a reason to abandon the declaration: the object is left
			// alone and kept out of desired, and pruning cannot reach it
			// because it carries neither this entitlement's labels nor its
			// owner reference.
			conflicts = append(conflicts, exists.Error())
			continue
		}
		if err != nil {
			if apimeta.IsNoMatchError(err) {
				entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
				entry.Reason = reasonKindNotServed
				entry.Message = fmt.Sprintf(
					"%s could not install %s %q because this project's control plane does not serve that kind.",
					serviceName, obj.GVK.Kind, obj.Name)
				return entry
			}
			entry.State = servicesv1alpha1.ProvisionedResourceStateFailed
			entry.Reason = reasonApplyFailed
			entry.Message = truncateMessage(fmt.Sprintf("%s could not install %s %q: %s",
				serviceName, obj.GVK.Kind, obj.Name,
				customerErrorReason(ctx, err, "entitlement", entitlement.Name, "kind", obj.GVK.Kind, "name", obj.Name),
			), maxLedgerMessageBytes)
			return entry
		}
		if desired[obj.GVK] == nil {
			desired[obj.GVK] = map[string]struct{}{}
		}
		desired[obj.GVK][obj.Name] = struct{}{}
	}

	// Pruning is scoped to this declaration, so one that could not decode never
	// removes another's objects. It runs over the kinds installed last time as
	// well as this time, because renaming or re-kinding an object is now an
	// ordinary edit and the object under the old identity has to go.
	for _, gvk := range union(kindsOf(objects), installed) {
		if err := r.prune(ctx, consumerClient, entitlement, gvk, decl.Name, desired[gvk]); err != nil {
			entry.State = servicesv1alpha1.ProvisionedResourceStateFailed
			entry.Reason = reasonApplyFailed
			entry.Message = truncateMessage(fmt.Sprintf("%s could not remove withdrawn %s objects: %s",
				serviceName, gvk.Kind,
				customerErrorReason(ctx, err, "entitlement", entitlement.Name, "kind", gvk.Kind),
			), maxLedgerMessageBytes)
			return entry
		}
	}

	entry.Kinds = kindRefs(kindsOf(objects))
	entry.ObjectCount = int32(len(objects) - len(conflicts))

	// A name the project already uses will not free itself on a retry, so the
	// declaration is Unprovisionable until the customer renames or removes
	// their object, or the provider renames the declared one. The next resync
	// installs it once the name is free.
	if len(conflicts) > 0 {
		entry.State = servicesv1alpha1.ProvisionedResourceStateUnprovisionable
		entry.Reason = reasonObjectExists
		entry.Message = truncateMessage(strings.Join(conflicts, " "), maxLedgerMessageBytes)
		return entry
	}

	entry.State = servicesv1alpha1.ProvisionedResourceStateInstalled

	// AUTHORIZATION GAP: reported, not closed.
	//
	// An API that authorizes a cross-project reference does so against whoever
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

	// A status that did not land leaves the resource delivered. The object is
	// installed, owned, and reclaimable. The reason goes on the entry that
	// carries the object, so a consumer whose class never reports itself usable
	// finds the cause here.
	if len(statusNotes) > 0 {
		entry.Reason = reasonStatusNotApplied
		entry.Message = truncateMessage(strings.Join(statusNotes, " ")+" "+authorizationCaveat, maxLedgerMessageBytes)
	}

	// Adoption outranks a missing status: disabling the service deletes these
	// objects, and the customer created them.
	if len(adoptedNotes) > 0 {
		entry.Reason = reasonObjectAdopted
		entry.Message = truncateMessage(
			strings.Join(append(adoptedNotes, statusNotes...), " ")+" "+authorizationCaveat, maxLedgerMessageBytes)
	}
	return entry
}

// objectExistsError reports a declared object whose name is already taken in
// the consumer project by an object this entitlement did not install.
type objectExistsError struct {
	kind, name string
	// owner says who holds the name, for the consumer reading the ledger.
	owner string
}

func (e *objectExistsError) Error() string {
	return fmt.Sprintf("%s %q already exists in this project and %s, so it was left unchanged.",
		e.kind, e.name, e.owner)
}

// upsertResult carries what upsert has to tell the ledger about an object that
// did install.
type upsertResult struct {
	// statusNote is set when the object installed but its declared status did
	// not.
	statusNote string
	// adoptedNote is set when the object predates this entitlement.
	adoptedNote string
}

// claimExisting decides whether the platform may write to an object that
// already holds the declared name.
//
// It may only when this entitlement installed it. The controller owner
// reference is the authority, because garbage collection acts on it: an object
// controlled by anything else, including an earlier entitlement of the same
// name, is not this one's to change. The labels are the fallback for an object
// with no controller at all, which can only be this entitlement's if someone
// removed the owner reference afterwards.
func claimExisting(
	existing *unstructured.Unstructured,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	declName string,
	obj provisioning.Object,
) error {
	labels := existing.GetLabels()
	if ref := metav1.GetControllerOf(existing); ref != nil {
		if ref.UID == entitlement.UID {
			return nil
		}
		owner := "is managed by something other than the platform"
		if ref.Kind == "ServiceEntitlement" {
			owner = "was installed by the platform for another service"
			if ref.Name == entitlement.Name {
				owner = "belongs to an earlier enablement of this service that is still being removed"
			}
		}
		return &objectExistsError{kind: obj.GVK.Kind, name: obj.Name, owner: owner}
	}
	if labels[labelManagedBy] == labelManagedByValue &&
		labels[labelEntitlementName] == entitlement.Name &&
		labels[labelProvisionedResource] == declName {
		return nil
	}
	owner := "was not installed by the platform"
	if labels[labelManagedBy] == labelManagedByValue {
		owner = "was installed by the platform for another service"
	}
	return &objectExistsError{kind: obj.GVK.Kind, name: obj.Name, owner: owner}
}

// upsert writes one declared object into the consumer project, and its declared
// status alongside it.
//
// It writes only to a name that is free or that this entitlement already
// holds. An object someone else created under the declared name is left
// untouched and reported as an *objectExistsError: the forced apply below would
// otherwise rewrite it, label it, and hand it to the entitlement, so disabling
// the service would delete the customer's object.
//
// The apply is server-side and forced, so the fields the declaration states are
// the fields the object has: a consumer edit to any of them is reverted, and a
// field the declaration stops stating is removed. What the platform adds — the
// provisioning labels and the owner reference — it adds here, not in the
// declaration, which is why a provider cannot author either.
//
// The result's status note is non-empty when the object installed but its
// declared status did not. The object is usable by everything that does not
// read its status, so upsert reports that rather than failing.
func (r *ProvisioningReconciler) upsert(
	ctx context.Context,
	consumerClient client.Client,
	consumerReader client.Reader,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	serviceName string,
	declName string,
	obj provisioning.Object,
) (upsertResult, error) {
	var result upsertResult

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GVK)
	switch err := consumerReader.Get(ctx, client.ObjectKey{Name: obj.Name}, existing); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return result, err
	default:
		if err := claimExisting(existing, entitlement, declName, obj); err != nil {
			return result, err
		}
		// Nothing this entitlement installed can be older than it, so an
		// older object was someone else's before an earlier version of this
		// controller took it over. Releasing it here would be another silent
		// ownership change, so it stays managed and the ledger says so.
		created := existing.GetCreationTimestamp()
		if created.Before(&entitlement.CreationTimestamp) {
			log.FromContext(ctx).Info("provisioned object predates its entitlement and was adopted",
				"kind", obj.GVK.Kind, "name", obj.Name)
			result.adoptedNote = fmt.Sprintf(
				"%s %q existed before %s was enabled and was taken over by the platform; disabling the service will delete it.",
				obj.GVK.Kind, obj.Name, serviceName)
		}
	}

	u := obj.Unstructured()

	l := u.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[labelManagedBy] = labelManagedByValue
	l[labelServiceName] = encodeName(serviceName)
	l[labelEntitlementName] = entitlement.Name
	l[labelProvisionedResource] = declName
	u.SetLabels(l)

	// The cluster-scoped ServiceEntitlement owns what was provisioned for it,
	// so deleting the entitlement garbage-collects these objects. This is the
	// only teardown path that does not depend on the project purger, which does
	// not delete cluster-scoped resources.
	if err := controllerutil.SetControllerReference(entitlement, u, r.Scheme); err != nil {
		return result, fmt.Errorf("set controller reference: %w", err)
	}

	// This apply carries status so a kind with no status subresource, where
	// status is an ordinary field, is written in one call. A kind that serves
	// one drops status here, and upsertStatus writes it.
	if err := consumerClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(provisioningFieldManager), client.ForceOwnership); err != nil {
		return result, err
	}

	result.statusNote = r.upsertStatus(ctx, consumerClient, serviceName, obj)
	return result, nil
}

// upsertStatus applies the status a declaration states to the installed
// object's status subresource, and describes what stopped it when it cannot.
//
// A declaration that states no status is left alone. Writing an empty status
// would take ownership of a field the provider said nothing about.
//
// The controller tries the status endpoint instead of using discovery. Support
// varies by kind and by consumer control plane, so a discovery cache would need
// warming and invalidation to answer what one request answers. A kind with no
// status endpoint is not a failure, because status is an ordinary field there
// and the object write already carried it.
//
// This apply is not forced, unlike the object write. A conflict means another
// controller in the consumer's plane manages that status field, and forcing
// would start a write loop across every entitled project. The controller
// reports the conflict instead.
func (r *ProvisioningReconciler) upsertStatus(
	ctx context.Context,
	consumerClient client.Client,
	serviceName string,
	obj provisioning.Object,
) string {
	declared, stated := obj.Status()
	if !stated {
		return ""
	}

	// Only identity and status. Sending metadata through the status endpoint
	// would claim the object's labels a second time under a different
	// subresource, for no gain.
	status := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": obj.GVK.GroupVersion().String(),
		"kind":       obj.GVK.Kind,
		"metadata":   map[string]any{"name": obj.Name},
		"status":     declared,
	}}

	// Server-side apply makes the resync free. Re-applying an unchanged status
	// produces no write, so resourceVersion does not move and downstream
	// watchers see no event.
	err := consumerClient.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(status),
		client.FieldOwner(provisioningFieldManager))
	switch {
	case err == nil:
		return ""
	case apierrors.IsNotFound(err), apierrors.IsMethodNotSupported(err):
		// The kind serves no status endpoint. Status is an ordinary field
		// there, and the object write already carried the declared value.
		return ""
	case apierrors.IsConflict(err):
		return fmt.Sprintf(
			"%s declares a status for %s %q, but another controller in this project manages it, so the declared status was not applied.",
			serviceName, obj.GVK.Kind, obj.Name)
	default:
		return fmt.Sprintf("%s installed %s %q but could not apply its declared status: %s.",
			serviceName, obj.GVK.Kind, obj.Name,
			customerErrorReason(ctx, err, "kind", obj.GVK.Kind, "name", obj.Name, "subresource", "status"))
	}
}

// truncateMessage keeps a ledger message inside the schema's limit and marks
// where it was cut.
func truncateMessage(message string, limit int) string {
	if len(message) <= limit {
		return message
	}
	const ellipsis = "..."
	cut := limit - len(ellipsis)
	// Never split a multi-byte character: the result must stay valid UTF-8.
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + ellipsis
}

// prune deletes objects this declaration previously installed that it no longer
// declares.
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
	return r.sweep(ctx, consumerClient, entitlement, allInstalledKinds(entitlement), nil)
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
	for i := range declared {
		keep[declared[i].Name] = struct{}{}
	}
	// The ledger still describes the previous reconcile at this point, and a
	// withdrawn declaration names no kind, so the ledger is the only record of
	// what it installed.
	return r.sweep(ctx, consumerClient, entitlement, allInstalledKinds(entitlement), keep)
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

// installedKinds is what each declaration recorded installing in this project,
// deduplicated, keyed by declaration name.
func installedKinds(entitlement *servicesv1alpha1.ServiceEntitlement) map[string][]schema.GroupVersionKind {
	out := make(map[string][]schema.GroupVersionKind, len(entitlement.Status.ProvisionedResources))
	for _, entry := range entitlement.Status.ProvisionedResources {
		out[entry.Name] = union(nil, gvksOf(entry.Kinds))
	}
	return out
}

// allInstalledKinds is everything this project recorded receiving, across all
// declarations.
func allInstalledKinds(entitlement *servicesv1alpha1.ServiceEntitlement) []schema.GroupVersionKind {
	var out []schema.GroupVersionKind
	for _, entry := range entitlement.Status.ProvisionedResources {
		out = union(out, gvksOf(entry.Kinds))
	}
	return out
}

func gvksOf(refs []servicesv1alpha1.ProvisionedKindRef) []schema.GroupVersionKind {
	out := make([]schema.GroupVersionKind, 0, len(refs))
	for _, ref := range refs {
		if ref.Version == "" || ref.Kind == "" {
			// A versionless kind would make the sweep itself fail.
			continue
		}
		out = append(out, schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind})
	}
	return out
}

func kindsOf(objects []provisioning.Object) []schema.GroupVersionKind {
	out := make([]schema.GroupVersionKind, 0, len(objects))
	for _, obj := range objects {
		out = append(out, obj.GVK)
	}
	return union(nil, out)
}

func kindRefs(gvks []schema.GroupVersionKind) []servicesv1alpha1.ProvisionedKindRef {
	if len(gvks) == 0 {
		return nil
	}
	out := make([]servicesv1alpha1.ProvisionedKindRef, 0, len(gvks))
	for _, gvk := range gvks {
		out = append(out, servicesv1alpha1.ProvisionedKindRef{
			Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind,
		})
	}
	return out
}

// union appends the kinds of b that a does not already hold, preserving order
// so the ledger and the sweep are stable across reconciles.
func union(a, b []schema.GroupVersionKind) []schema.GroupVersionKind {
	seen := make(map[schema.GroupVersionKind]struct{}, len(a)+len(b))
	out := make([]schema.GroupVersionKind, 0, len(a)+len(b))
	for _, list := range [][]schema.GroupVersionKind{a, b} {
		for _, gvk := range list {
			if _, dup := seen[gvk]; dup {
				continue
			}
			seen[gvk] = struct{}{}
			out = append(out, gvk)
		}
	}
	return out
}

// writeStatus records the ledger and the Provisioned condition on the
// entitlement, in the consumer's own control plane.
//
// Only provisioningStatusOwner's fields are applied: other controllers own
// Ready, Suspended and setup on the same object. Ready stays untouched here,
// because a delivery failure is not a denial of access.
func (r *ProvisioningReconciler) writeStatus(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	ledger []servicesv1alpha1.ProvisionedResourceStatus,
	overrideReason, overrideMessage string,
) error {
	before := entitlement.DeepCopy()

	sort.Slice(ledger, func(i, j int) bool { return ledger[i].Name < ledger[j].Name })
	for i := range ledger {
		ledger[i].Message = truncateMessage(ledger[i].Message, maxLedgerMessageBytes)
	}
	entitlement.Status.ProvisionedResources = ledger

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

	// Nothing changed: skip the write, or every resync bumps resourceVersion
	// and re-triggers every other controller watching ServiceEntitlement.
	changed := !apiequality.Semantic.DeepEqual(before.Status.ProvisionedResources, entitlement.Status.ProvisionedResources) ||
		!conditionsEqual(before.Status.Conditions, entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned)
	stale := before.Status.LastProvisioningEvaluation == nil ||
		time.Since(before.Status.LastProvisioningEvaluation.Time) >= provisioningHeartbeatInterval
	if !changed && !stale {
		return nil
	}

	now := metav1.Now()
	entitlement.Status.LastProvisioningEvaluation = &now

	if err := applyEntitlementStatus(ctx, consumerClient, provisioningStatusOwner, before, entitlement); err != nil {
		return fmt.Errorf("failed to apply ServiceEntitlement provisioning status: %w", err)
	}
	return nil
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
	r.rootAPIReader = rootMgr.GetAPIReader()
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
		WithOptions(controller.TypedOptions[mcreconcile.Request]{
			MaxConcurrentReconciles: provisioningMaxConcurrentReconciles,
		}).
		Complete(requeueOnConflict(r))
}
