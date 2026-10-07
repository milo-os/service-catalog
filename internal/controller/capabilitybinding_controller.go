// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
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
	// up in customer projects. The triggers we do get are the
	// ServiceEntitlement watch, a project being engaged, and changes to an
	// agent that reaches every project or to its configurations; a periodic
	// pass covers the rest — the same trade-off LocationBinding makes. A
	// project with no Active entitlement only takes part in that pass while
	// some agent reaches every project, so with none the fleet of unentitled
	// projects costs nothing after the first pass.
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
//	gate 1: the project's ServiceEntitlement for the service is Active, or
//	        the agent asks for spec.visibility.entitlement None and the
//	        platform owner has allowlisted it in EntitlementFreeAgents (#93);
//	        on that path the Service must also be Published
//	gate 2: a ServiceAgent for that service is Published
//	gate 3: that agent has a Published ServiceAgentConfiguration; the newest
//	        one (creation time, ties broken by name) supplies the content
//
// When any of them stops being true the binding is deleted, and the project's
// assistant simply stops offering that service's help. Setting an agent back
// to Required, or taking it off the allowlist, is gate 1 closing in every
// project without an entitlement.
//
// ServiceEntitlements live in the project; Service, ServiceAgent, and
// ServiceAgentConfiguration live on the root cluster and are read through
// rootClient.
type CapabilityBindingReconciler struct {
	// rootClient reads the cluster-scoped catalog objects. They live outside
	// every project, so a project's own client cannot see them. It is the
	// multicluster manager's local client, whose cache also feeds the
	// ServiceAgent and ServiceAgentConfiguration watches: a reconcile those
	// watches trigger must read from the same cache, or it can act on the
	// state from before the event.
	rootClient client.Client
	Manager    mcmanager.Manager
	Scheme     *runtime.Scheme

	// EntitlementFreeAgents is the platform owner's allowlist, from operator
	// config: the ServiceAgents, by name, whose request for
	// spec.visibility.entitlement None is honoured. Any other agent is gated
	// on entitlement whatever it asks for.
	EntitlementFreeAgents map[string]struct{}

	// warnedAgents remembers which agents we have already complained about, so
	// a misconfigured one is reported once rather than on every pass over every
	// project.
	warnedAgents sync.Map

	// projects holds the projects this controller is currently engaged with,
	// keyed by cluster name, so a change to an agent that reaches every
	// project can be fanned out to all of them, and a request for a project
	// that has since gone away can be dropped. See projectEngagementSource.
	projects sync.Map
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
		// A project disengaged or deleted while its request waited in the
		// queue can never be reached again; the provider reports that as a
		// plain error, so retrying would back off forever. If it is engaged
		// again, engagement enqueues it afresh.
		if _, tracked := r.projects.Load(consumerProject); !tracked {
			logger.V(1).Info("project is no longer engaged; dropping its request")
			return ctrl.Result{}, nil
		}
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
	entitled := make(map[string]struct{}, len(entitlementList.Items))
	for i := range entitlementList.Items {
		e := &entitlementList.Items[i]
		if e.DeletionTimestamp.IsZero() && e.Status.Phase == servicesv1alpha1.EntitlementPhaseActive {
			entitled[e.Spec.ServiceRef.Name] = struct{}{}
		}
	}

	var agentList servicesv1alpha1.ServiceAgentList
	if err := r.rootClient.List(ctx, &agentList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceAgents: %w", err)
	}
	// Keep the order stable so repeated passes do the same work in the same
	// order rather than following list order.
	sort.Slice(agentList.Items, func(i, j int) bool { return agentList.Items[i].Name < agentList.Items[j].Name })

	// An agent naming a service that is not in the catalog can never reach a
	// customer, and the loop below would simply skip it. That is impossible to
	// tell apart from "nobody is entitled to it", so say so plainly — once.
	r.warnUnresolvableAgents(ctx, agentList.Items)

	// Services are read once per pass, however many agents name them.
	services := make(map[string]*servicesv1alpha1.Service)
	desired := make(map[string]struct{})
	reachesEveryProject := false

	for i := range agentList.Items {
		agent := &agentList.Items[i]
		serviceRefName := agent.Spec.ServiceRef.Name

		// Gate 1.
		_, isEntitled := entitled[serviceRefName]
		if !isEntitled && !r.waivesEntitlement(agent) {
			continue
		}
		// Gate 2.
		if agent.Spec.Phase != servicesv1alpha1.PhasePublished {
			continue
		}
		if r.waivesEntitlement(agent) {
			reachesEveryProject = true
		}

		// The service supplies the full name tool usage is billed against.
		// Without it there is nothing worth projecting; retry on the next pass.
		svc, seen := services[serviceRefName]
		if !seen {
			var got servicesv1alpha1.Service
			if err := r.rootClient.Get(ctx, types.NamespacedName{Name: serviceRefName}, &got); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to get Service %q: %w", serviceRefName, err)
				}
				logger.V(1).Info("agent's service not in the catalog yet", "service", serviceRefName, "agent", agent.Name)
			} else {
				svc = &got
			}
			services[serviceRefName] = svc
		}
		if svc == nil {
			continue
		}
		// Without an entitlement nothing else vouches for the service, so it
		// must be live. An entitlement can only be made Active for a
		// Published service, which is what the other path relies on.
		if !isEntitled && svc.Spec.Phase != servicesv1alpha1.PhasePublished {
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

		if err := r.upsertCapabilityBinding(ctx, consumerClient, svc, agent, sac); err != nil {
			return ctrl.Result{}, err
		}
		desired[agent.Name] = struct{}{}
	}

	if err := r.pruneCapabilityBindings(ctx, consumerClient, desired); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciled capability bindings",
		"entitledServices", len(entitled), "bindings", len(desired))

	// A project with nothing but platform-wide agents to receive needs the
	// periodic pass only while such an agent exists; otherwise the triggers
	// above are enough, and thousands of unentitled projects would otherwise
	// be swept every interval for nothing.
	if len(entitled) == 0 && !reachesEveryProject {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: capabilityBindingResyncInterval}, nil
}

// waivesEntitlement reports whether the agent asks to reach every project and
// the platform owner has allowlisted it, so its request is honoured.
func (r *CapabilityBindingReconciler) waivesEntitlement(agent *servicesv1alpha1.ServiceAgent) bool {
	if !agent.RequestsNoEntitlement() {
		return false
	}
	_, ok := r.EntitlementFreeAgents[agent.Name]
	return ok
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
// the project's entitlement withdrawn, or an agent that reached every project
// set back to Required. Only bindings carrying the shared
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
// Four things enqueue a project:
//
//   - a ServiceEntitlement change in that project, via Watches rather than For
//     so every entitlement in a project collapses onto one reconcile;
//   - the project being engaged (projectEngagementSource), so a project with
//     no entitlements at all, including one created after an agent was made
//     platform-wide, still gets the agents that reach every project;
//   - a change to an allowlisted ServiceAgent that asks, or asked, for None
//     (agentVisibilityHandler), which enqueues every engaged project;
//   - a change to a ServiceAgentConfiguration of an agent that reaches every
//     project (agentConfigurationHandler), likewise.
//
// All of them use the same request per project, so they collapse in the
// queue, and capabilityBindingMaxConcurrentReconciles bounds how many projects
// are written at once however many are queued. Other root-cluster changes — an
// entitled agent publishing, a new reviewed configuration for one — are picked
// up by the periodic pass. The allowlist is read at startup only; changing it
// restarts the operator, and engagement then enqueues every project.
//
// Catalog objects are read through the local manager's client, the same cache
// the ServiceAgent and ServiceAgentConfiguration watches are fed from.
func (r *CapabilityBindingReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.rootClient = mgr.GetLocalManager().GetClient()
	r.Manager = mgr
	c, err := mcbuilder.ControllerManagedBy(mgr).
		Named("capability-binding").
		Watches(
			&servicesv1alpha1.ServiceEntitlement{},
			mchandler.TypedEnqueueRequestsFromMapFunc[client.Object, mcreconcile.Request](mapServiceEntitlementToProjectRequest),
			mcbuilder.WithEngageWithProviderClusters(true),
		).
		// ServiceAgents and their configurations live on the root cluster,
		// which is this manager's local cluster, never one of its projects.
		Watches(
			&servicesv1alpha1.ServiceAgent{},
			func(multicluster.ClusterName, cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
				return r.agentVisibilityHandler()
			},
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false),
		).
		Watches(
			&servicesv1alpha1.ServiceAgentConfiguration{},
			func(multicluster.ClusterName, cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
				return r.agentConfigurationHandler()
			},
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false),
		).
		WithOptions(controller.TypedOptions[mcreconcile.Request]{
			MaxConcurrentReconciles: capabilityBindingMaxConcurrentReconciles,
		}).
		Build(r)
	if err != nil {
		return err
	}
	return c.MultiClusterWatch(projectEngagementSource{r: r})
}

// projectRequest is the one request every trigger uses for a project.
func projectRequest(project multicluster.ClusterName) mcreconcile.Request {
	return mcreconcile.Request{
		Request: reconcile.Request{
			NamespacedName: types.NamespacedName{Name: locationBindingRequestName},
		},
		ClusterName: project,
	}
}

// projectEngagementSource enqueues a project once when the manager engages
// it, and remembers it until it is disengaged.
//
// Without it a project with no ServiceEntitlements is never reconciled: the
// entitlement watch has nothing to report, so it would never receive an agent
// that needs no entitlement. Engagement is the right moment rather than the
// Project object appearing on the root cluster, because only then can its
// control plane be written, and because it covers exactly the projects this
// manager serves. Under consumer-scoped projection that is the consumer
// projects of this operator's own services, not every project.
//
// After a restart every engaged project is enqueued once; the shared request
// key and capabilityBindingMaxConcurrentReconciles keep that bounded, a pass
// over a project with nothing to change makes no writes, and an unentitled
// project is not requeued unless some agent reaches every project.
type projectEngagementSource struct {
	r *CapabilityBindingReconciler
}

func (s projectEngagementSource) ForCluster(
	name multicluster.ClusterName,
	_ cluster.Cluster,
) (source.TypedSource[mcreconcile.Request], bool, error) {
	// The root cluster is engaged under the empty name on the all-projects
	// manager. It is not a project and has nothing to project into.
	if name == "" {
		return nil, false, nil
	}
	return source.TypedFunc[mcreconcile.Request](func(ctx context.Context, q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) error {
		s.r.trackProject(ctx, name)
		q.Add(projectRequest(name))
		return nil
	}), true, nil
}

// trackProject records an engaged project until ctx, the engagement's own
// context, ends. A token guards against a quick re-engagement being forgotten
// when the earlier engagement's context closes.
func (r *CapabilityBindingReconciler) trackProject(ctx context.Context, name multicluster.ClusterName) {
	token := new(struct{})
	r.projects.Store(name, token)
	context.AfterFunc(ctx, func() { r.projects.CompareAndDelete(name, token) })
}

// enqueueAllProjects asks for a pass over every engaged project.
func (r *CapabilityBindingReconciler) enqueueAllProjects(q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) {
	r.projects.Range(func(key, _ any) bool {
		q.Add(projectRequest(key.(multicluster.ClusterName)))
		return true
	})
}

// agentVisibilityHandler fans a ServiceAgent change out to every engaged
// project when it affects every project: the agent is allowlisted and asks
// for None now, or did before the change. That covers such an agent being
// created, set back to Required, published, retired, or deleted. Other agents
// only reach the projects entitled to them and are left to the periodic pass,
// as before. Updates that leave the spec alone (status, labels) are ignored.
func (r *CapabilityBindingReconciler) agentVisibilityHandler() handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	platformWide := func(obj client.Object) bool {
		agent, ok := obj.(*servicesv1alpha1.ServiceAgent)
		return ok && r.waivesEntitlement(agent)
	}
	return handler.TypedFuncs[client.Object, mcreconcile.Request]{
		CreateFunc: func(_ context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) {
			if platformWide(e.Object) {
				r.enqueueAllProjects(q)
			}
		},
		UpdateFunc: func(_ context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) {
			if e.ObjectOld.GetGeneration() == e.ObjectNew.GetGeneration() {
				return
			}
			if platformWide(e.ObjectOld) || platformWide(e.ObjectNew) {
				r.enqueueAllProjects(q)
			}
		},
		DeleteFunc: func(_ context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) {
			if platformWide(e.Object) {
				r.enqueueAllProjects(q)
			}
		},
	}
}

// agentConfigurationHandler fans a ServiceAgentConfiguration change out to
// every engaged project when its agent is allowlisted and asks for None, so a
// new reviewed version of a platform-wide agent reaches projects without an
// entitlement as soon as it is published rather than on the periodic pass.
// Configurations of other agents are left to the periodic pass, as before.
func (r *CapabilityBindingReconciler) agentConfigurationHandler() handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	platformWide := func(ctx context.Context, objs ...client.Object) bool {
		for _, obj := range objs {
			sac, ok := obj.(*servicesv1alpha1.ServiceAgentConfiguration)
			if !ok {
				continue
			}
			var agent servicesv1alpha1.ServiceAgent
			if err := r.rootClient.Get(ctx, types.NamespacedName{Name: sac.Spec.ServiceAgentRef.Name}, &agent); err != nil {
				continue
			}
			if r.waivesEntitlement(&agent) {
				return true
			}
		}
		return false
	}
	return handler.TypedFuncs[client.Object, mcreconcile.Request]{
		CreateFunc: func(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) {
			if platformWide(ctx, e.Object) {
				r.enqueueAllProjects(q)
			}
		},
		UpdateFunc: func(ctx context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) {
			if e.ObjectOld.GetGeneration() == e.ObjectNew.GetGeneration() {
				return
			}
			if platformWide(ctx, e.ObjectOld, e.ObjectNew) {
				r.enqueueAllProjects(q)
			}
		},
		DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) {
			if platformWide(ctx, e.Object) {
				r.enqueueAllProjects(q)
			}
		},
	}
}

// warnUnresolvableAgents reports a Published agent whose serviceRef names no
// catalog entry. Such an agent is silently skipped when bindings are built, so
// without this the only symptom is that it never appears in any project.
func (r *CapabilityBindingReconciler) warnUnresolvableAgents(ctx context.Context, agents []servicesv1alpha1.ServiceAgent) {
	logger := log.FromContext(ctx)
	for i := range agents {
		agent := &agents[i]
		if agent.Spec.Phase != servicesv1alpha1.PhasePublished {
			continue
		}
		var svc servicesv1alpha1.Service
		err := r.rootClient.Get(ctx, types.NamespacedName{Name: agent.Spec.ServiceRef.Name}, &svc)
		if err == nil {
			r.warnedAgents.Delete(agent.Name)
			continue
		}
		if !apierrors.IsNotFound(err) {
			continue
		}
		if _, seen := r.warnedAgents.LoadOrStore(agent.Name, struct{}{}); seen {
			continue
		}
		logger.Info("published agent names a service that is not in the catalog; it will reach no customer",
			"agent", agent.Name, "serviceRef", agent.Spec.ServiceRef.Name)
	}
}
