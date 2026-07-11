// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

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

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// agentBindingResyncInterval bounds how long a gate change on the
	// root cluster (a ServiceAgent publishing, a new Published
	// ServiceAgentConfiguration, a configuration deprecating) takes to
	// propagate into project bindings. The primary trigger is the
	// ServiceEntitlement watch; multicluster-runtime does not give us a
	// clean way to enqueue project-scoped reconciles from root-cluster
	// object events, so a periodic resync covers the cross-cluster gates
	// instead — the same trade-off LocationBinding makes.
	agentBindingResyncInterval = 5 * time.Minute

	// AgentBinding metadata labels. labelBindingServiceName records the
	// canonical reverse-DNS service the binding belongs to;
	// labelBindingServiceAgent mirrors the projected agent for
	// label-selector discovery; labelBindingEntitlement records the
	// owning ServiceEntitlement so bindings can be pruned even after the
	// entitlement object is gone (envtest and other environments without
	// the garbage collector rely on this — the controller owner
	// reference is the production backstop, not the primary cleanup
	// path).
	labelBindingServiceName  = "services.miloapis.com/service-name"
	labelBindingServiceAgent = "services.miloapis.com/service-agent"
	labelBindingEntitlement  = "services.miloapis.com/entitlement"

	// reasonAllAgentGatesOpen is the Ready=True reason on a projected
	// binding: entitlement Active, agent Published, and a Published
	// configuration selected.
	reasonAllAgentGatesOpen = "AllAgentGatesOpen"
)

// AgentBindingReconciler projects Published ServiceAgents into entitled
// consumer projects as consumer-facing, cluster-scoped AgentBinding
// objects. It is the place where the three agent gates are combined:
//
//	gate 1: the project's ServiceEntitlement for the service is Active
//	gate 2: a ServiceAgent for the service is Published
//	gate 3: at least one Published ServiceAgentConfiguration exists for
//	        that agent; the LATEST Published one (creation timestamp,
//	        ties broken by name) supplies the projected spec
//
// One binding is projected per Published agent, named after the agent.
// The spec is fully denormalized from the active configuration so
// consumers never read platform objects. Bindings are pruned as soon as
// any gate closes.
//
// It runs on the multicluster manager so each reconcile is scoped to
// one engaged project cluster (req.ClusterName), where it reads the
// project's ServiceEntitlements and writes AgentBindings. Service,
// ServiceAgent, and ServiceAgentConfiguration all live on the root
// cluster and are read through rootClient — mirroring
// ServiceEntitlementReconciler.
type AgentBindingReconciler struct {
	// rootClient reads cluster-scoped Service, ServiceAgent, and
	// ServiceAgentConfiguration objects from the root key space. These
	// do not live in any project VCP, so the per-cluster client cannot
	// see them.
	rootClient client.Client
	Manager    mcmanager.Manager
	Scheme     *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagentconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=agentbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.miloapis.com,resources=agentbindings/status,verbs=get;update;patch

func (r *AgentBindingReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	consumerProject := req.ClusterName
	if consumerProject == "" {
		return ctrl.Result{}, fmt.Errorf("AgentBinding reconcile invoked without a cluster name")
	}

	consumerCluster, err := r.Manager.GetCluster(ctx, consumerProject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get consumer cluster %q: %w", consumerProject, err)
	}
	consumerClient := consumerCluster.GetClient()

	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(ctx, req.NamespacedName, &entitlement); err != nil {
		if apierrors.IsNotFound(err) {
			// The entitlement is gone. Its bindings carry a controller
			// owner reference, but garbage collection is not guaranteed
			// to run everywhere the operator does (envtest has no GC),
			// so prune actively by the entitlement label.
			return ctrl.Result{}, r.pruneBindingsForEntitlementName(ctx, consumerClient, req.Name)
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ServiceEntitlement: %w", err)
	}

	// Gate 1: only an Active entitlement projects agents. Anything else
	// (pending approval, rejected, deleting) tears its bindings down.
	if !entitlement.DeletionTimestamp.IsZero() ||
		entitlement.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		if err := r.pruneBindingsForEntitlementName(ctx, consumerClient, entitlement.Name); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	serviceRefName := entitlement.Spec.ServiceRef.Name

	// Resolve the Service for the canonical reverse-DNS name projected
	// onto bindings. Without it there is nothing to denormalize; treat
	// as a closed gate and retry on the resync interval.
	var svc servicesv1alpha1.Service
	if err := r.rootClient.Get(ctx, types.NamespacedName{Name: serviceRefName}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.pruneBindingsForEntitlementName(ctx, consumerClient, entitlement.Name); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: agentBindingResyncInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Service %q: %w", serviceRefName, err)
	}

	// Gate 2: Published ServiceAgents for the service.
	var agentList servicesv1alpha1.ServiceAgentList
	if err := r.rootClient.List(ctx, &agentList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceAgents: %w", err)
	}

	desired := make(map[string]struct{})
	for i := range agentList.Items {
		agent := &agentList.Items[i]
		if agent.Spec.ServiceRef.Name != serviceRefName {
			continue
		}
		if agent.Spec.Phase != servicesv1alpha1.PhasePublished {
			continue
		}

		// Gate 3: the active (latest Published) configuration for the
		// agent. No Published configuration means nothing is
		// projectable for this agent.
		sac, err := r.latestPublishedAgentConfiguration(ctx, agent.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if sac == nil {
			continue
		}

		if err := r.upsertBinding(ctx, consumerClient, &entitlement, &svc, agent, sac); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to upsert AgentBinding %q: %w", agent.Name, err)
		}
		desired[agent.Name] = struct{}{}
	}

	// Prune any binding we own that is no longer backed by an open set
	// of gates (agent unpublished, last Published configuration
	// deprecated, or the agent/config removed).
	if err := r.pruneBindings(ctx, consumerClient, &entitlement, desired); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciled agent bindings", "service", serviceRefName, "bindings", len(desired))
	return ctrl.Result{RequeueAfter: agentBindingResyncInterval}, nil
}

// latestPublishedAgentConfiguration returns the most recently created
// Published ServiceAgentConfiguration for the named agent, or nil if
// none exists. Selecting the latest published document (rather than a
// pinned reference) is the agreed configuration-selection behavior,
// matching what LocationBinding does for ServiceConfiguration.
func (r *AgentBindingReconciler) latestPublishedAgentConfiguration(
	ctx context.Context,
	agentName string,
) (*servicesv1alpha1.ServiceAgentConfiguration, error) {
	var list servicesv1alpha1.ServiceAgentConfigurationList
	if err := r.rootClient.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("failed to list ServiceAgentConfigurations: %w", err)
	}
	var latest *servicesv1alpha1.ServiceAgentConfiguration
	for i := range list.Items {
		sac := &list.Items[i]
		if sac.Spec.ServiceAgentRef.Name != agentName {
			continue
		}
		if sac.Spec.Phase != servicesv1alpha1.PhasePublished {
			continue
		}
		if latest == nil || moreRecentAgentConfiguration(sac, latest) {
			latest = sac
		}
	}
	return latest, nil
}

// moreRecentAgentConfiguration reports whether a should win over b as
// the active configuration: later creation timestamp, breaking ties on
// the higher name for determinism.
func moreRecentAgentConfiguration(a, b *servicesv1alpha1.ServiceAgentConfiguration) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name > b.Name
	}
	return a.CreationTimestamp.After(b.CreationTimestamp.Time)
}

// upsertBinding creates or updates the AgentBinding for one agent,
// projecting the denormalized spec from the active configuration, and
// reconciles its Ready condition. Metadata/spec are written via
// CreateOrUpdate; the status condition is written separately (and only
// when it changes) so an already-settled binding is a no-op.
func (r *AgentBindingReconciler) upsertBinding(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	svc *servicesv1alpha1.Service,
	agent *servicesv1alpha1.ServiceAgent,
	sac *servicesv1alpha1.ServiceAgentConfiguration,
) error {
	binding := &servicesv1alpha1.AgentBinding{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name},
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, consumerClient, binding, func() error {
		labels := binding.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[labelManagedBy] = labelManagedByValue
		labels[labelBindingServiceName] = svc.Spec.ServiceName
		labels[labelBindingServiceAgent] = agent.Name
		labels[labelBindingEntitlement] = entitlement.Name
		binding.SetLabels(labels)

		// The cluster-scoped ServiceEntitlement owns its projected
		// bindings so that garbage collection reclaims them if the
		// operator misses a delete; the reconciler's label-based prune
		// is the primary cleanup path.
		if err := controllerutil.SetControllerReference(entitlement, binding, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		binding.Spec = servicesv1alpha1.AgentBindingSpec{
			ServiceRef:           servicesv1alpha1.ServiceRef{Name: svc.Name},
			ServiceName:          svc.Spec.ServiceName,
			ServiceAgentRef:      servicesv1alpha1.ServiceAgentReference{Name: agent.Name},
			ConfigurationVersion: sac.Spec.Version,
			Knowledge:            sac.Spec.Knowledge.DeepCopy(),
			Tools:                sac.Spec.Tools.DeepCopy(),
			Authority:            sac.Spec.Authority.DeepCopy(),
		}
		return nil
	}); err != nil {
		return err
	}

	return r.setBindingReady(ctx, consumerClient, binding)
}

// setBindingReady updates the binding's Ready condition, writing to the
// status subresource only when the condition actually changes. A
// binding only exists while every gate is open, so Ready is always True
// here; gate closure deletes the binding instead of flipping the
// condition.
func (r *AgentBindingReconciler) setBindingReady(
	ctx context.Context,
	consumerClient client.Client,
	binding *servicesv1alpha1.AgentBinding,
) error {
	newStatus := binding.Status.DeepCopy()
	newStatus.ObservedGeneration = binding.Generation
	apimeta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: binding.Generation,
		Reason:             reasonAllAgentGatesOpen,
		Message:            "Agent is projected into this project.",
	})

	if binding.Status.ObservedGeneration == newStatus.ObservedGeneration &&
		conditionsEqual(binding.Status.Conditions, newStatus.Conditions, ConditionTypeReady) {
		return nil
	}
	binding.Status = *newStatus
	if err := consumerClient.Status().Update(ctx, binding); err != nil {
		return fmt.Errorf("failed to update AgentBinding status: %w", err)
	}
	return nil
}

// pruneBindings deletes AgentBindings owned by the entitlement that are
// not in the keep set. A nil keep set deletes every owned binding (used
// when a project-level gate closes).
func (r *AgentBindingReconciler) pruneBindings(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	keep map[string]struct{},
) error {
	var list servicesv1alpha1.AgentBindingList
	if err := consumerClient.List(ctx, &list, client.MatchingLabels{
		labelManagedBy:          labelManagedByValue,
		labelBindingEntitlement: entitlement.Name,
	}); err != nil {
		return fmt.Errorf("failed to list AgentBindings: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if _, ok := keep[item.Name]; ok {
			continue
		}
		if err := consumerClient.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete stale AgentBinding %q: %w", item.Name, err)
		}
	}
	return nil
}

// pruneBindingsForEntitlementName deletes every managed AgentBinding
// labeled for the named entitlement. Used when the entitlement is not
// Active or no longer exists, where only its name is known.
func (r *AgentBindingReconciler) pruneBindingsForEntitlementName(
	ctx context.Context,
	consumerClient client.Client,
	entitlementName string,
) error {
	return r.pruneBindings(ctx, consumerClient, &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: entitlementName},
	}, nil)
}

// SetupWithManager registers the reconciler on the multicluster
// manager.
//
// The primary watch is ServiceEntitlement, scoped to engaged project
// clusters (WithEngageWithProviderClusters), mirroring
// ServiceEntitlementReconciler. The other gates depend on ServiceAgent
// and ServiceAgentConfiguration objects on the root cluster;
// multicluster-runtime has no clean way to translate a root-cluster
// object event into a project-scoped reconcile request, so those gates
// are picked up by the periodic resync configured via RequeueAfter
// rather than by additional watches.
func (r *AgentBindingReconciler) SetupWithManager(mgr mcmanager.Manager, rootClient client.Client) error {
	r.rootClient = rootClient
	r.Manager = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		Named("agent-binding").
		For(&servicesv1alpha1.ServiceEntitlement{}, mcbuilder.WithEngageWithProviderClusters(true)).
		Complete(r)
}
