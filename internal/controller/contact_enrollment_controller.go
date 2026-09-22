// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/contactenrollment"
)

// contactEnrollmentFieldManager owns the ContactEnrolled condition, patched
// separately from any other status write on ServiceEntitlement — mirroring
// dependenciesConditionFieldManager — so it can never race the Ready/phase
// write ServiceEntitlementReconciler makes on the same object.
const contactEnrollmentFieldManager = "contact-enrollment"

// defaultContactNamespace is used when
// ContactEnrollmentReconciler.ContactNamespace is left empty.
const defaultContactNamespace = "milo-system"

// contactNotFoundRequeueInterval paces the retry while no CRM Contact has
// been found yet for a requester. Milo's UserContactController usually
// creates one quickly after a User exists, but this also watches Contact
// directly (see SetupWithManager), so the interval only bounds the case
// where that watch is missed.
const contactNotFoundRequeueInterval = 2 * time.Minute

// contactEnrollmentFailedRequeueInterval paces the retry after an
// unrecognized failure ensuring the ContactGroup exists or adding the
// membership.
const contactEnrollmentFailedRequeueInterval = 30 * time.Second

// ContactEnrollmentReconciler watches for ServiceEntitlements going Active on
// a service that has opted into CRM contact-group enrollment
// (Service.spec.contactEnrollment), and adds the requester's
// notification.miloapis.com Contact to the linked ContactGroup, creating the
// group if it doesn't exist yet.
//
// It is deliberately separate from ServiceEntitlementReconciler: CRM
// enrollment is a best-effort, externally-facing side effect, and a problem
// reaching Milo's contact API (or a Contact that hasn't shown up yet) must
// never block dependency enrollment or quota grants for the same
// entitlement. It writes no finalizer and performs no teardown — removing
// membership on revocation is explicitly out of scope for this iteration
// (see docs/enhancements/entitlement-contact-enrollment.md).
type ContactEnrollmentReconciler struct {
	// rootClient reads Service and reads/writes notification.miloapis.com
	// Contact, ContactGroup, ContactGroupMembership, and
	// ContactGroupMembershipRemoval objects. All of these live in Milo's root
	// control plane, not any project's virtual one — the same reason
	// ServiceEntitlementReconciler keeps a separate rootClient for Service.
	rootClient client.Client
	Manager    mcmanager.Manager
	Scheme     *runtime.Scheme

	// ContactNamespace is where Contacts live and where a ContactGroup is
	// created when a Service's contactEnrollment doesn't name one
	// explicitly. Defaults to "milo-system" when empty.
	ContactNamespace string
}

// +kubebuilder:rbac:groups=notification.miloapis.com,resources=contacts,verbs=get;list;watch
// +kubebuilder:rbac:groups=notification.miloapis.com,resources=contactgroups,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=notification.miloapis.com,resources=contactgroupmemberships,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=notification.miloapis.com,resources=contactgroupmembershipremovals,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=resourcemanager.miloapis.com,resources=projects,verbs=get;list;watch

func (r *ContactEnrollmentReconciler) contactNamespace() string {
	if r.ContactNamespace == "" {
		return defaultContactNamespace
	}
	return r.ContactNamespace
}

func (r *ContactEnrollmentReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	consumerProject := req.ClusterName
	if consumerProject == "" {
		return ctrl.Result{}, fmt.Errorf("ContactEnrollment reconcile invoked without a cluster name")
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

	// No teardown: an entitlement being deleted is left alone entirely, by
	// design (see the type doc comment).
	if !entitlement.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Enrollment follows approval; it doesn't anticipate it. A pending or
	// rejected entitlement gets no ContactEnrolled condition at all yet —
	// that isn't a failure, just not time yet — and the entitlement's own
	// transition to Active re-triggers this reconcile.
	if entitlement.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		return ctrl.Result{}, nil
	}

	svc, err := resolveService(ctx, r.rootClient, entitlement.Status.ServiceName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The Service disappeared after the entitlement went Active — an
			// edge case with nothing sensible to enroll into. Leave whatever
			// ContactEnrolled condition already exists as-is.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to resolve Service %q: %w", entitlement.Status.ServiceName, err)
	}

	if svc.Spec.ContactEnrollment == nil {
		return ctrl.Result{}, r.setCondition(ctx, consumerClient, &entitlement,
			metav1.ConditionTrue, servicesv1alpha1.ReasonContactEnrollmentNotConfigured,
			"This service isn't set up for CRM contact-group enrollment.")
	}

	contact, err := contactenrollment.ResolveContact(ctx, r.rootClient, r.contactNamespace(), entitlement.Spec.RequestedBy)
	if err != nil {
		switch {
		case errors.Is(err, contactenrollment.ErrRequesterUnknown):
			return ctrl.Result{}, r.setCondition(ctx, consumerClient, &entitlement,
				metav1.ConditionFalse, servicesv1alpha1.ReasonContactRequesterUnknown,
				"This entitlement has no recorded requester, so no CRM contact can be resolved.")
		case errors.Is(err, contactenrollment.ErrContactNotFound):
			logger.Info("no Contact found yet for requester; will retry")
			if condErr := r.setCondition(ctx, consumerClient, &entitlement,
				metav1.ConditionFalse, servicesv1alpha1.ReasonContactNotFound,
				"No CRM contact found yet for the requester; enrollment will complete once one appears."); condErr != nil {
				return ctrl.Result{}, condErr
			}
			return ctrl.Result{RequeueAfter: contactNotFoundRequeueInterval}, nil
		default:
			return ctrl.Result{}, fmt.Errorf("failed to resolve contact: %w", err)
		}
	}

	group, err := contactenrollment.EnsureGroup(ctx, r.rootClient, r.contactNamespace(), svc)
	if err != nil {
		condErr := r.setCondition(ctx, consumerClient, &entitlement,
			metav1.ConditionFalse, servicesv1alpha1.ReasonContactEnrollmentFailed,
			fmt.Sprintf("Couldn't ensure the contact group exists: %v", err))
		if condErr != nil {
			logger.Error(condErr, "failed to patch ContactEnrolled condition")
		}
		return ctrl.Result{RequeueAfter: contactEnrollmentFailedRequeueInterval}, nil
	}

	outcome, err := contactenrollment.EnsureMembership(ctx, r.rootClient, contact, group)
	if err != nil {
		condErr := r.setCondition(ctx, consumerClient, &entitlement,
			metav1.ConditionFalse, servicesv1alpha1.ReasonContactEnrollmentFailed,
			fmt.Sprintf("Couldn't enroll the contact: %v", err))
		if condErr != nil {
			logger.Error(condErr, "failed to patch ContactEnrolled condition")
		}
		return ctrl.Result{RequeueAfter: contactEnrollmentFailedRequeueInterval}, nil
	}

	switch outcome {
	case contactenrollment.OutcomeEnrolled:
		return ctrl.Result{}, r.setCondition(ctx, consumerClient, &entitlement,
			metav1.ConditionTrue, servicesv1alpha1.ReasonContactEnrolled,
			"The requester's contact was added to this service's contact group.")
	case contactenrollment.OutcomeOptedOut:
		return ctrl.Result{}, r.setCondition(ctx, consumerClient, &entitlement,
			metav1.ConditionTrue, servicesv1alpha1.ReasonContactOptedOut,
			"The requester previously opted out of this contact group; that opt-out is honored.")
	default:
		// EnsureMembership only returns OutcomeFailed together with a
		// non-nil error, which is handled above — this default is
		// defensive, not a reachable path.
		if condErr := r.setCondition(ctx, consumerClient, &entitlement,
			metav1.ConditionFalse, servicesv1alpha1.ReasonContactEnrollmentFailed,
			"Couldn't enroll the contact for an unrecognized reason."); condErr != nil {
			logger.Error(condErr, "failed to patch ContactEnrolled condition")
		}
		return ctrl.Result{RequeueAfter: contactEnrollmentFailedRequeueInterval}, nil
	}
}

// setCondition patches the ContactEnrolled condition on entitlement,
// separately from any other status write on the object (see
// contactEnrollmentFieldManager). No-ops if the condition is already exactly
// this value, matching the equality check ServiceEntitlementReconciler uses
// for DependenciesSatisfied.
func (r *ContactEnrollmentReconciler) setCondition(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	before := entitlement.DeepCopy()
	apimeta.SetStatusCondition(&entitlement.Status.Conditions, metav1.Condition{
		Type:               servicesv1alpha1.ConditionTypeContactEnrolled,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: entitlement.Generation,
	})
	if conditionsEqual(before.Status.Conditions, entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeContactEnrolled) {
		return nil
	}
	if err := consumerClient.Status().Patch(ctx, entitlement, client.MergeFrom(before),
		client.FieldOwner(contactEnrollmentFieldManager)); err != nil {
		return fmt.Errorf("failed to patch ContactEnrolled condition: %w", err)
	}
	return nil
}

// mapServiceToServiceEntitlements re-enqueues every ServiceEntitlement naming
// the changed Service. This is what makes "an operator links a ContactGroup
// after customers already registered" pick up existing Active entitlements —
// see contactEnrollmentServicePredicate for which Service changes trigger it.
func (r *ContactEnrollmentReconciler) mapServiceToServiceEntitlements(
	ctx context.Context,
	svc *servicesv1alpha1.Service,
) []mcreconcile.Request {
	return enqueueEntitlementsForServiceName(ctx, r.rootClient, r.Manager, svc.Spec.ServiceName)
}

// contactEnrollmentServicePredicate keeps the Service fan-out off the hot
// path: only a change to contactEnrollment itself is worth re-running every
// entitlement in every engaged project for.
func contactEnrollmentServicePredicate() predicate.TypedPredicate[*servicesv1alpha1.Service] {
	return predicate.TypedFuncs[*servicesv1alpha1.Service]{
		UpdateFunc: func(e event.TypedUpdateEvent[*servicesv1alpha1.Service]) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			return !apiequality.Semantic.DeepEqual(
				e.ObjectOld.Spec.ContactEnrollment, e.ObjectNew.Spec.ContactEnrollment)
		},
	}
}

// mapContactToServiceEntitlements re-enqueues every Active ServiceEntitlement
// requested by the User a changed or newly created Contact belongs to. This
// is what makes "the entitlement completes as soon as their contact record
// shows up" hold: without it, an entitlement stuck on ContactNotFound would
// only resolve on the next contactNotFoundRequeueInterval tick.
func (r *ContactEnrollmentReconciler) mapContactToServiceEntitlements(
	ctx context.Context,
	contact *notificationv1alpha1.Contact,
) []mcreconcile.Request {
	if contact.Spec.SubjectRef == nil || contact.Spec.SubjectRef.Kind != "User" || contact.Spec.SubjectRef.Name == "" {
		return nil
	}
	return enqueueEntitlementsForRequester(ctx, r.rootClient, r.Manager, contact.Spec.SubjectRef.Name)
}

// enqueueEntitlementsForRequester turns a requester's User object name into
// project-scoped requests for every ServiceEntitlement stamped with that
// requester, across every engaged project. Mirrors
// enqueueEntitlementsForServiceName's shape, keyed by requester instead of
// canonical service name.
func enqueueEntitlementsForRequester(
	ctx context.Context,
	rootClient client.Client,
	mgr mcmanager.Manager,
	requesterName string,
) []mcreconcile.Request {
	if requesterName == "" {
		return nil
	}

	var projects resourcemanagerv1alpha1.ProjectList
	if err := rootClient.List(ctx, &projects); err != nil {
		log.FromContext(ctx).Error(err, "list Projects for Contact fan-out", "requester", requesterName)
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
			client.MatchingFields{entitlementRequesterNameIndex: requesterName},
		); err != nil {
			log.FromContext(ctx).Error(err, "list ServiceEntitlements for Contact fan-out",
				"project", project, "requester", requesterName)
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

// SetupWithManager registers the reconciler on the multicluster manager.
// Mirrors ServiceEntitlementReconciler.SetupWithManager's structure: field
// indexes first, then the controller and its watches.
func (r *ContactEnrollmentReconciler) SetupWithManager(mcMgr mcmanager.Manager, rootMgr ctrl.Manager) error {
	r.rootClient = rootMgr.GetClient()
	r.Manager = mcMgr

	// Index ServiceEntitlement by spec.requestedBy.name. Propagates to every
	// engaged project cluster, same as entitlementServiceNameIndex.
	if err := mcMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&servicesv1alpha1.ServiceEntitlement{},
		entitlementRequesterNameIndex,
		entitlementRequesterNameIndexer,
	); err != nil {
		return fmt.Errorf("failed to index ServiceEntitlement by %s: %w", entitlementRequesterNameIndex, err)
	}

	// Index Contact by the same field keys contactenrollment.ResolveContact
	// lists against. These are also apiserver-side selectable fields on
	// Milo's own Contact type, but a local index lets this manager's cache
	// answer the query directly rather than round-tripping to Milo on every
	// lookup.
	if err := rootMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&notificationv1alpha1.Contact{},
		contactenrollment.ContactSubjectNameIndex,
		func(obj client.Object) []string {
			c := obj.(*notificationv1alpha1.Contact)
			if c.Spec.SubjectRef == nil || c.Spec.SubjectRef.Name == "" {
				return nil
			}
			return []string{c.Spec.SubjectRef.Name}
		},
	); err != nil {
		return fmt.Errorf("failed to index Contact by %s: %w", contactenrollment.ContactSubjectNameIndex, err)
	}
	if err := rootMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&notificationv1alpha1.Contact{},
		contactenrollment.ContactEmailIndex,
		func(obj client.Object) []string {
			c := obj.(*notificationv1alpha1.Contact)
			if c.Spec.Email == "" {
				return nil
			}
			return []string{c.Spec.Email}
		},
	); err != nil {
		return fmt.Errorf("failed to index Contact by %s: %w", contactenrollment.ContactEmailIndex, err)
	}

	// Index ContactGroupMembershipRemoval by spec.contactRef.name so
	// EnsureMembership's opt-out pre-check doesn't need a full list scan.
	if err := rootMgr.GetFieldIndexer().IndexField(
		context.Background(),
		&notificationv1alpha1.ContactGroupMembershipRemoval{},
		contactenrollment.MembershipRemovalContactRefNameIndex,
		func(obj client.Object) []string {
			rm := obj.(*notificationv1alpha1.ContactGroupMembershipRemoval)
			if rm.Spec.ContactRef.Name == "" {
				return nil
			}
			return []string{rm.Spec.ContactRef.Name}
		},
	); err != nil {
		return fmt.Errorf("failed to index ContactGroupMembershipRemoval by %s: %w", contactenrollment.MembershipRemovalContactRefNameIndex, err)
	}

	return mcbuilder.ControllerManagedBy(mcMgr).
		Named("contact-enrollment").
		For(&servicesv1alpha1.ServiceEntitlement{}, mcbuilder.WithEngageWithProviderClusters(true)).
		WatchesRawSource(source.TypedKind(
			rootMgr.GetCache(),
			&servicesv1alpha1.Service{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapServiceToServiceEntitlements),
			contactEnrollmentServicePredicate(),
		)).
		WatchesRawSource(source.TypedKind(
			rootMgr.GetCache(),
			&notificationv1alpha1.Contact{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapContactToServiceEntitlements),
		)).
		Complete(r)
}
