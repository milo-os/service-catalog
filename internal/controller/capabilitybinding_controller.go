// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// capabilityBindingGVK is what a customer's assistant reads to learn what it
// can do for that project. The kind belongs to the assistant, not the catalog,
// so it is written untyped and the catalog takes no dependency on that repo.
//
// Cluster-scoped on purpose: the project's control plane is already the
// boundary, and the assistant lists these without a namespace. Give one a
// namespace and the assistant never finds it.
var capabilityBindingGVK = schema.GroupVersionKind{
	Group:   "capabilities.assistant.miloapis.com",
	Version: "v1alpha1",
	Kind:    "CapabilityBinding",
}

const (
	// capabilityBindingResyncInterval bounds how long a change on the root
	// cluster (an agent publishing, a new reviewed configuration) takes to show
	// up in customer projects. The trigger we do get is the ServiceEntitlement
	// watch; multicluster-runtime gives us no clean way to turn a root-cluster
	// event into a project-scoped reconcile, so a periodic pass covers the rest
	// — the same trade-off LocationBinding makes.
	capabilityBindingResyncInterval = 5 * time.Minute

	// Labels recording what a projected binding came from, so it can be found
	// and pruned without reading the catalog. Only bindings carrying the shared
	// managed-by label are ever deleted, which is what keeps a hand-written
	// binding in a dev or test project safe.
	labelCapabilityServiceName  = "services.miloapis.com/service-name"
	labelCapabilityServiceAgent = "services.miloapis.com/service-agent"

	// capabilityBindingMaxConcurrentReconciles is how many projects this
	// controller writes into at once. One reconcile is one project and is
	// almost entirely API round-trips, so a serial controller mostly waits.
	// Publishing an agent across a fleet enqueues one request per project at
	// once; see locationBindingMaxConcurrentReconciles for the same reasoning.
	capabilityBindingMaxConcurrentReconciles = 5
)

// CapabilityBindingReconciler publishes what a project's AI assistant can do
// into that project's own control plane, as CapabilityBinding objects.
//
// A reconcile covers one project (req.ClusterName) and recomputes the whole set
// from scratch, not just whatever triggered it. Three things must all be true
// for an agent to reach a project:
//
//	gate 1: the project's ServiceEntitlement for the service is Active
//	gate 2: a ServiceAgent for that service is Published
//	gate 3: that agent has a Published ServiceAgentConfiguration; the newest
//	        one (creation time, ties broken by name) supplies the content
//
// When any of them stops being true the binding is deleted, and the project's
// assistant simply stops offering that service's help.
//
// ServiceEntitlements live in the project; Service, ServiceAgent, and
// ServiceAgentConfiguration live on the root cluster and are read through
// rootClient.
type CapabilityBindingReconciler struct {
	// rootClient reads the cluster-scoped catalog objects. They live outside
	// every project, so a project's own client cannot see them.
	rootClient client.Client
	Manager    mcmanager.Manager
	Scheme     *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagents/status,verbs=get
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagentconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceagentconfigurations/status,verbs=get
// +kubebuilder:rbac:groups=capabilities.assistant.miloapis.com,resources=capabilitybindings,verbs=get;list;watch;create;update;patch;delete

func (r *CapabilityBindingReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	consumerProject := req.ClusterName
	if consumerProject == "" {
		return ctrl.Result{}, fmt.Errorf("CapabilityBinding reconcile invoked without a cluster name")
	}

	consumerCluster, err := r.Manager.GetCluster(ctx, consumerProject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get consumer cluster %q: %w", consumerProject, err)
	}
	consumerClient := consumerCluster.GetClient()

	// The event names one entitlement, but what a project's assistant can do
	// depends on every Active entitlement it holds, so recompute all of them. A
	// deleted or suspended entitlement needs no special case: it is simply
	// absent below, and the prune at the end takes its bindings away.
	var entitlementList servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(ctx, &entitlementList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceEntitlements: %w", err)
	}
	active := make([]*servicesv1alpha1.ServiceEntitlement, 0, len(entitlementList.Items))
	for i := range entitlementList.Items {
		e := &entitlementList.Items[i]
		if e.DeletionTimestamp.IsZero() && e.Status.Phase == servicesv1alpha1.EntitlementPhaseActive {
			active = append(active, e)
		}
	}
	// Keep the order stable so repeated passes do the same work in the same
	// order rather than following list order.
	sort.Slice(active, func(i, j int) bool {
		return active[i].Spec.ServiceRef.Name < active[j].Spec.ServiceRef.Name
	})

	var agentList servicesv1alpha1.ServiceAgentList
	if err := r.rootClient.List(ctx, &agentList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceAgents: %w", err)
	}
	sort.Slice(agentList.Items, func(i, j int) bool { return agentList.Items[i].Name < agentList.Items[j].Name })

	desired := make(map[string]struct{})

	for _, entitlement := range active {
		serviceRefName := entitlement.Spec.ServiceRef.Name

		// The service supplies the full name tool usage is billed against.
		// Without it there is nothing worth projecting; retry on the next pass.
		var svc servicesv1alpha1.Service
		if err := r.rootClient.Get(ctx, types.NamespacedName{Name: serviceRefName}, &svc); err != nil {
			if apierrors.IsNotFound(err) {
				logger.V(1).Info("entitled service not in the catalog yet", "service", serviceRefName)
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to get Service %q: %w", serviceRefName, err)
		}

		for i := range agentList.Items {
			agent := &agentList.Items[i]
			if agent.Spec.ServiceRef.Name != serviceRefName {
				continue
			}
			// Gate 2.
			if agent.Spec.Phase != servicesv1alpha1.PhasePublished {
				continue
			}
			// Gate 3.
			sac, err := r.latestPublishedAgentConfiguration(ctx, agent.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			if sac == nil {
				continue
			}

			if err := r.upsertCapabilityBinding(ctx, consumerClient, &svc, agent, sac); err != nil {
				return ctrl.Result{}, err
			}
			desired[agent.Name] = struct{}{}
		}
	}

	if err := r.pruneCapabilityBindings(ctx, consumerClient, desired); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciled capability bindings",
		"activeEntitlements", len(active), "bindings", len(desired))
	return ctrl.Result{RequeueAfter: capabilityBindingResyncInterval}, nil
}

// latestPublishedAgentConfiguration returns the newest Published configuration
// for the named agent, or nil if it has none.
func (r *CapabilityBindingReconciler) latestPublishedAgentConfiguration(
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
		// A blank version would leave a projected binding with nothing to
		// trace the content back to, and the assistant rejects it outright.
		if sac.Spec.Version == "" {
			continue
		}
		if latest == nil || moreRecentAgentConfiguration(sac, latest) {
			latest = sac
		}
	}
	return latest, nil
}

// moreRecentAgentConfiguration reports whether a should win over b: created
// later, ties broken on the higher name so the choice is stable.
func moreRecentAgentConfiguration(a, b *servicesv1alpha1.ServiceAgentConfiguration) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name > b.Name
	}
	return a.CreationTimestamp.After(b.CreationTimestamp.Time)
}

// capabilityBindingSpec mirrors the assistant's own CapabilityBinding spec.
// The field names here are the contract between the two repos: a tag that
// drifts fails no build, it just drops that piece of content from every
// customer's assistant.
type capabilityBindingSpec struct {
	ServiceRef           servicesv1alpha1.ServiceRef            `json:"serviceRef"`
	ServiceName          string                                 `json:"serviceName"`
	ServiceAgentRef      servicesv1alpha1.ServiceAgentReference `json:"serviceAgentRef"`
	ConfigurationVersion string                                 `json:"configurationVersion"`
	Knowledge            *servicesv1alpha1.AgentKnowledge       `json:"knowledge,omitempty"`
	Tools                *servicesv1alpha1.AgentTools           `json:"tools,omitempty"`
	Skills               []servicesv1alpha1.AgentSkill          `json:"skills,omitempty"`
	Authority            *servicesv1alpha1.AgentAuthority       `json:"authority,omitempty"`
	ReportingProject     string                                 `json:"reportingProject,omitempty"`
}

// buildCapabilityBindingSpec copies the reviewed content onto the shape the
// assistant reads.
func buildCapabilityBindingSpec(
	svc *servicesv1alpha1.Service,
	agent *servicesv1alpha1.ServiceAgent,
	sac *servicesv1alpha1.ServiceAgentConfiguration,
) (map[string]any, error) {
	spec := capabilityBindingSpec{
		ServiceRef:           servicesv1alpha1.ServiceRef{Name: svc.Name},
		ServiceName:          svc.Spec.ServiceName,
		ServiceAgentRef:      servicesv1alpha1.ServiceAgentReference{Name: agent.Name},
		ConfigurationVersion: sac.Spec.Version,
		Knowledge:            sac.Spec.Knowledge.DeepCopy(),
		Tools:                sac.Spec.Tools.DeepCopy(),
		Authority:            sac.Spec.Authority.DeepCopy(),
		ReportingProject:     sac.Spec.ReportingProject,
	}
	if len(sac.Spec.Skills) > 0 {
		spec.Skills = make([]servicesv1alpha1.AgentSkill, len(sac.Spec.Skills))
		copy(spec.Skills, sac.Spec.Skills)
	}

	out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return nil, fmt.Errorf("failed to build CapabilityBinding spec: %w", err)
	}
	return out, nil
}

// upsertCapabilityBinding writes one project's binding for one agent. It is
// named after the agent, so a hand-written binding of the same name converges
// onto the catalog's content instead of sitting alongside it as a duplicate.
//
// Status is deliberately not touched: the assistant owns it, and that status is
// the only way a provider learns their content was accepted.
func (r *CapabilityBindingReconciler) upsertCapabilityBinding(
	ctx context.Context,
	consumerClient client.Client,
	svc *servicesv1alpha1.Service,
	agent *servicesv1alpha1.ServiceAgent,
	sac *servicesv1alpha1.ServiceAgentConfiguration,
) error {
	spec, err := buildCapabilityBindingSpec(svc, agent, sac)
	if err != nil {
		return err
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(capabilityBindingGVK)
	u.SetName(agent.Name)

	if _, err := controllerutil.CreateOrUpdate(ctx, consumerClient, u, func() error {
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[labelManagedBy] = labelManagedByValue
		labels[labelCapabilityServiceName] = svc.Spec.ServiceName
		labels[labelCapabilityServiceAgent] = agent.Name
		u.SetLabels(labels)

		// Nothing in the project owns this binding — the catalog objects that
		// decide whether it should exist live on another cluster, where an
		// owner reference cannot point. The prune below is the cleanup path,
		// and a reference left over from an earlier design would cascade-delete
		// a binding a project still needs.
		u.SetOwnerReferences(nil)
		return unstructured.SetNestedMap(u.Object, spec, "spec")
	}); err != nil {
		if apimeta.IsNoMatchError(err) {
			// The project's control plane has no CapabilityBinding CRD, so no
			// assistant reads one there. Nothing to do.
			return nil
		}
		return fmt.Errorf("failed to upsert CapabilityBinding %q: %w", agent.Name, err)
	}
	return nil
}

// pruneCapabilityBindings deletes the bindings this operator wrote that are no
// longer wanted — an agent unpublished, its last reviewed configuration pulled,
// or the project's entitlement withdrawn. Only bindings carrying the shared
// managed-by label are considered, so a hand-written one is left alone.
func (r *CapabilityBindingReconciler) pruneCapabilityBindings(
	ctx context.Context,
	consumerClient client.Client,
	keep map[string]struct{},
) error {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(capabilityBindingGVK.GroupVersion().WithKind(capabilityBindingGVK.Kind + "List"))
	if err := consumerClient.List(ctx, &list,
		client.MatchingLabelsSelector{Selector: managedByFanoutSelector},
	); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to list CapabilityBindings: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if _, ok := keep[item.GetName()]; ok {
			continue
		}
		if err := consumerClient.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete stale CapabilityBinding %q: %w", item.GetName(), err)
		}
	}
	return nil
}

// SetupWithManager registers the reconciler on the multicluster manager.
//
// The watch is ServiceEntitlement in engaged project clusters, via Watches
// rather than For so every entitlement in a project collapses onto one
// reconcile. The agent and configuration gates live on the root cluster, which
// we have no clean way to enqueue from, so they are picked up by the periodic
// pass instead.
func (r *CapabilityBindingReconciler) SetupWithManager(mgr mcmanager.Manager, rootClient client.Client) error {
	r.rootClient = rootClient
	r.Manager = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		Named("capability-binding").
		Watches(
			&servicesv1alpha1.ServiceEntitlement{},
			mchandler.TypedEnqueueRequestsFromMapFunc[client.Object, mcreconcile.Request](mapServiceEntitlementToProjectRequest),
			mcbuilder.WithEngageWithProviderClusters(true),
		).
		WithOptions(controller.TypedOptions[mcreconcile.Request]{
			MaxConcurrentReconciles: capabilityBindingMaxConcurrentReconciles,
		}).
		Complete(r)
}
