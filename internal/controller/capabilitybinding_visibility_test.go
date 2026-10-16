// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// Coverage for spec.visibility.entitlement (milo-os/service-catalog#93): an
// agent that asks for None and that the platform owner has allowlisted reaches
// every project, entitled or not; any other agent is gated on entitlement.

func cbAgentWithEntitlement(phase servicesv1alpha1.Phase, entitlement string) *servicesv1alpha1.ServiceAgent {
	a := cbAgentObject(phase)
	a.Spec.Visibility.Entitlement = entitlement
	return a
}

// allowlist is the operator config's capabilities.entitlementFreeAgents.
func allowlist(names ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

// newAllowlistedReconciler is newCapabilityReconciler with cbAgent allowlisted.
func newAllowlistedReconciler(root, consumer client.Client) *CapabilityBindingReconciler {
	r := newCapabilityReconciler(root, consumer)
	r.EntitlementFreeAgents = allowlist(cbAgent)
	return r
}

func newTestQueue() workqueue.TypedRateLimitingInterface[mcreconcile.Request] {
	return workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[mcreconcile.Request]())
}

// drain returns the cluster names queued, in no particular order.
func drain(q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) map[multicluster.ClusterName]struct{} {
	out := map[multicluster.ClusterName]struct{}{}
	for q.Len() > 0 {
		item, _ := q.Get()
		out[item.ClusterName] = struct{}{}
		q.Done(item)
	}
	return out
}

func TestCapabilityBinding_AllowlistedNoneReachesUnentitledProject(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient() // no ServiceEntitlement at all
	r := newAllowlistedReconciler(root, consumer)

	res, err := r.Reconcile(context.Background(), cbRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, ok := getCapabilityBinding(t, consumer, cbAgent)
	if !ok {
		t.Fatal("an allowlisted agent asking for None should reach a project with no entitlement")
	}
	if v, _, _ := unstructured.NestedString(got.Object, "spec", "configurationVersion"); v != "v1" {
		t.Errorf("configurationVersion = %q, want v1", v)
	}
	if res.RequeueAfter != capabilityBindingResyncInterval {
		t.Errorf("RequeueAfter = %v, want %v while an agent reaches every project", res.RequeueAfter, capabilityBindingResyncInterval)
	}
}

// Asking for None is not enough: an agent the platform owner has not
// allowlisted is gated on entitlement exactly as if it asked for Required.
func TestCapabilityBinding_NoneNotAllowlistedBehavesAsRequired(t *testing.T) {
	agent := cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone)
	config := cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished)

	unentitled := cbConsumerClient(existingCapabilityBinding(cbAgent))
	r := newCapabilityReconciler(cbRootClient(cbServiceObject(), agent.DeepCopy(), config.DeepCopy()), unentitled)
	r.EntitlementFreeAgents = allowlist("some-other-agent")
	res, err := r.Reconcile(context.Background(), cbRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, unentitled, cbAgent); ok {
		t.Error("a None agent that is not allowlisted should not reach an unentitled project")
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0: nothing reaches this project", res.RequeueAfter)
	}

	entitled := cbConsumerClient(cbEntitlementActive())
	r = newCapabilityReconciler(cbRootClient(cbServiceObject(), agent.DeepCopy(), config.DeepCopy()), entitled)
	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, entitled, cbAgent); !ok {
		t.Error("a None agent that is not allowlisted should still reach entitled projects")
	}
}

// The default stays exactly what it was: no entitlement, no binding — whether
// the field is unset (as on objects stored before it existed) or Required,
// and whether or not the agent is allowlisted.
func TestCapabilityBinding_EntitlementRequiredStillGates(t *testing.T) {
	for _, entitlement := range []string{"", servicesv1alpha1.VisibilityEntitlementRequired} {
		root := cbRootClient(
			cbServiceObject(),
			cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, entitlement),
			cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
		)
		consumer := cbConsumerClient()
		r := newAllowlistedReconciler(root, consumer)

		if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
			t.Fatalf("reconcile (%q): %v", entitlement, err)
		}
		if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
			t.Errorf("entitlement %q: an unentitled project should get no binding", entitlement)
		}
	}
}

// The allowlist waives only the entitlement. A Draft agent, one with no
// reviewed configuration, or one whose Service is missing or not Published
// still reaches no unentitled project.
func TestCapabilityBinding_AllowlistedNoneKeepsOtherGates(t *testing.T) {
	serviceInPhase := func(phase servicesv1alpha1.Phase) *servicesv1alpha1.Service {
		svc := cbServiceObject()
		svc.Spec.Phase = phase
		return svc
	}
	none := func(phase servicesv1alpha1.Phase) *servicesv1alpha1.ServiceAgent {
		return cbAgentWithEntitlement(phase, servicesv1alpha1.VisibilityEntitlementNone)
	}
	published := func() *servicesv1alpha1.ServiceAgentConfiguration {
		return cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished)
	}
	cases := map[string][]client.Object{
		"draft agent":                {cbServiceObject(), none(servicesv1alpha1.PhaseDraft), published()},
		"no published configuration": {cbServiceObject(), none(servicesv1alpha1.PhasePublished), cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhaseDraft)},
		"service not in catalog":     {none(servicesv1alpha1.PhasePublished), published()},
		"service still Draft":        {serviceInPhase(servicesv1alpha1.PhaseDraft), none(servicesv1alpha1.PhasePublished), published()},
		"service Deprecated":         {serviceInPhase(servicesv1alpha1.PhaseDeprecated), none(servicesv1alpha1.PhasePublished), published()},
		"service Retired":            {serviceInPhase(servicesv1alpha1.PhaseRetired), none(servicesv1alpha1.PhasePublished), published()},
	}
	for name, objs := range cases {
		root := cbRootClient(objs...)
		consumer := cbConsumerClient()
		r := newAllowlistedReconciler(root, consumer)

		if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
			t.Fatalf("%s: reconcile: %v", name, err)
		}
		if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
			t.Errorf("%s: no binding should be projected", name)
		}
	}
}

// The Published-service check applies only where the allowlist stands in for
// an entitlement: an entitled project keeps its binding for a Deprecated
// service, as it always has.
func TestCapabilityBinding_EntitledProjectKeepsDeprecatedService(t *testing.T) {
	svc := cbServiceObject()
	svc.Spec.Phase = servicesv1alpha1.PhaseDeprecated
	root := cbRootClient(
		svc,
		cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newAllowlistedReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); !ok {
		t.Error("an entitled project should keep the binding for a Deprecated service")
	}
}

// An entitled project gets one binding, not two, when the agent is also
// allowlisted.
func TestCapabilityBinding_AllowlistedWithEntitlementProjectsOnce(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newAllowlistedReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := countCapabilityBindings(t, consumer); n != 1 {
		t.Errorf("bindings = %d, want 1", n)
	}
}

// Setting the agent back to Required, or dropping it from the allowlist,
// withdraws it from every project without an Active entitlement and leaves
// entitled projects alone.
func TestCapabilityBinding_GateCloseWithdrawsFromUnentitledProjects(t *testing.T) {
	for _, how := range []string{"set Required", "drop from allowlist"} {
		ctx := context.Background()
		root := cbRootClient(
			cbServiceObject(),
			cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone),
			cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
		)
		unentitled := cbConsumerClient()
		entitled := cbConsumerClient(cbEntitlementActive())

		mgr := newTestManager()
		mgr.add(cbProject, unentitled)
		mgr.add("entitled-proj", entitled)
		r := &CapabilityBindingReconciler{
			rootClient:            root,
			Manager:               mgr,
			Scheme:                capabilityScheme(),
			EntitlementFreeAgents: allowlist(cbAgent),
		}

		reconcileBoth := func() {
			t.Helper()
			for _, p := range []string{cbProject, "entitled-proj"} {
				req := cbRequest()
				req.ClusterName = multicluster.ClusterName(p)
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatalf("%s: reconcile %s: %v", how, p, err)
				}
			}
		}

		reconcileBoth()
		if _, ok := getCapabilityBinding(t, unentitled, cbAgent); !ok {
			t.Fatalf("%s: the allowlisted agent should reach the unentitled project", how)
		}

		switch how {
		case "set Required":
			var stored servicesv1alpha1.ServiceAgent
			if err := root.Get(ctx, types.NamespacedName{Name: cbAgent}, &stored); err != nil {
				t.Fatalf("get agent: %v", err)
			}
			stored.Spec.Visibility.Entitlement = servicesv1alpha1.VisibilityEntitlementRequired
			if err := root.Update(ctx, &stored); err != nil {
				t.Fatalf("update agent: %v", err)
			}
		case "drop from allowlist":
			r.EntitlementFreeAgents = allowlist()
		}

		reconcileBoth()
		if _, ok := getCapabilityBinding(t, unentitled, cbAgent); ok {
			t.Errorf("%s: the binding should be removed from the unentitled project", how)
		}
		if _, ok := getCapabilityBinding(t, entitled, cbAgent); !ok {
			t.Errorf("%s: the binding should stay in the entitled project", how)
		}
	}
}

// With no agent reaching every project, an unentitled project is reconciled
// when it is engaged and then left alone until something triggers it, rather
// than swept every interval.
func TestCapabilityBinding_UnentitledProjectNotRequeuedWithoutPlatformWideAgent(t *testing.T) {
	cases := map[string][]client.Object{
		"no agents":                   {cbServiceObject()},
		"Required agent":              {cbServiceObject(), cbAgentObject(servicesv1alpha1.PhasePublished), cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished)},
		"allowlisted but Draft agent": {cbServiceObject(), cbAgentWithEntitlement(servicesv1alpha1.PhaseDraft, servicesv1alpha1.VisibilityEntitlementNone)},
	}
	for name, objs := range cases {
		r := newAllowlistedReconciler(cbRootClient(objs...), cbConsumerClient())
		res, err := r.Reconcile(context.Background(), cbRequest())
		if err != nil {
			t.Fatalf("%s: reconcile: %v", name, err)
		}
		if res.RequeueAfter != 0 {
			t.Errorf("%s: RequeueAfter = %v, want 0", name, res.RequeueAfter)
		}
	}
}

// A pass over a project that is already up to date writes nothing, on either
// path into the project.
func TestCapabilityBinding_SettledPassMakesNoWrites(t *testing.T) {
	cases := map[string][]client.Object{
		"entitled":    {cbEntitlementActive()},
		"allowlisted": nil,
	}
	for name, projectObjs := range cases {
		var writes int
		consumer := fake.NewClientBuilder().
			WithScheme(capabilityScheme()).
			WithObjects(projectObjs...).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++
					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					writes++
					return c.Patch(ctx, obj, patch, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					writes++
					return c.Delete(ctx, obj, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					writes++
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			}).
			Build()
		root := cbRootClient(
			cbServiceObject(),
			cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone),
			cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
		)
		r := newAllowlistedReconciler(root, consumer)

		if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
			t.Fatalf("%s: first reconcile: %v", name, err)
		}
		if writes != 1 {
			t.Fatalf("%s: first pass made %d writes, want 1 (the create)", name, writes)
		}
		writes = 0
		if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
			t.Fatalf("%s: second reconcile: %v", name, err)
		}
		if writes != 0 {
			t.Errorf("%s: settled pass made %d writes, want 0", name, writes)
		}
	}
}

// A project the manager engages is reconciled once, even with no
// entitlements to report, so a new project receives platform-wide agents.
// Disengaging it stops it being tracked.
func TestCapabilityBinding_EngagedProjectIsReconciled(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	newProject := cbConsumerClient()
	mgr := newTestManager()
	mgr.add("new-proj", newProject)
	r := &CapabilityBindingReconciler{
		rootClient:            root,
		Manager:               mgr,
		Scheme:                capabilityScheme(),
		EntitlementFreeAgents: allowlist(cbAgent),
	}

	src, engage, err := projectEngagementSource{r: r}.ForCluster("new-proj", nil)
	if err != nil || !engage {
		t.Fatalf("ForCluster: engage=%v err=%v", engage, err)
	}
	engCtx, disengage := context.WithCancel(context.Background())
	q := newTestQueue()
	if err := src.Start(engCtx, q); err != nil {
		t.Fatalf("start: %v", err)
	}

	if q.Len() != 1 {
		t.Fatalf("queue length = %d, want 1", q.Len())
	}
	req, _ := q.Get()
	q.Done(req)
	if req.ClusterName != "new-proj" || req.Name != locationBindingRequestName {
		t.Fatalf("queued %v, want the project request for new-proj", req)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, newProject, cbAgent); !ok {
		t.Error("a newly engaged project should get the platform-wide agent")
	}

	if _, ok := r.projects.Load(multicluster.ClusterName("new-proj")); !ok {
		t.Fatal("engaged project should be tracked")
	}
	disengage()
	waitUntilUntracked(t, r, "new-proj")
}

// A request for a project that has since been disengaged is dropped, not
// retried forever: the provider can no longer reach it, and engagement would
// enqueue it again if it came back.
func TestCapabilityBinding_DisengagedProjectRequestIsDropped(t *testing.T) {
	mgr := newTestManager() // "gone-proj" is not engaged
	r := &CapabilityBindingReconciler{rootClient: cbRootClient(), Manager: mgr, Scheme: capabilityScheme()}

	engCtx, disengage := context.WithCancel(context.Background())
	r.trackProject(engCtx, "gone-proj")

	req := cbRequest()
	req.ClusterName = "gone-proj"
	// Still tracked but unreachable: a real failure, worth retrying.
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Error("an engaged but unreachable project should be retried")
	}

	disengage()
	waitUntilUntracked(t, r, "gone-proj")
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("a disengaged project's request should be dropped, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0", res.RequeueAfter)
	}
}

func waitUntilUntracked(t *testing.T, r *CapabilityBindingReconciler, name multicluster.ClusterName) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := r.projects.Load(name); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("disengaged project %q should stop being tracked", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The root cluster is engaged under the empty name and is not a project.
func TestCapabilityBinding_EngagementSourceSkipsRootCluster(t *testing.T) {
	_, engage, err := projectEngagementSource{r: &CapabilityBindingReconciler{}}.ForCluster("", nil)
	if err != nil || engage {
		t.Errorf("ForCluster(\"\"): engage=%v err=%v, want false, nil", engage, err)
	}
}

func trackedReconciler(root client.Client) *CapabilityBindingReconciler {
	r := &CapabilityBindingReconciler{rootClient: root, EntitlementFreeAgents: allowlist(cbAgent)}
	for _, p := range []string{"proj-a", "proj-b", "proj-c"} {
		r.trackProject(context.Background(), multicluster.ClusterName(p))
	}
	return r
}

type fanOutCase struct {
	name string
	fire func(q workqueue.TypedRateLimitingInterface[mcreconcile.Request])
	want int
}

func runFanOutCases(t *testing.T, cases []fanOutCase) {
	t.Helper()
	for _, tc := range cases {
		q := newTestQueue()
		tc.fire(q)
		if got := len(drain(q)); got != tc.want {
			t.Errorf("%s: enqueued %d projects, want %d", tc.name, got, tc.want)
		}
		q.ShutDown()
	}
}

// A change to an allowlisted agent that asks, or asked, for None fans out to
// every engaged project; a change to any other agent does not.
func TestCapabilityBinding_AgentVisibilityFanOut(t *testing.T) {
	r := trackedReconciler(cbRootClient())
	h := r.agentVisibilityHandler()
	ctx := context.Background()

	none := cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone)
	none.Generation = 1
	required := cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementRequired)
	required.Generation = 2
	requiredSameGen := required.DeepCopy()
	requiredSameGen.Generation = 1
	unlisted := none.DeepCopy()
	unlisted.Name = "not-allowlisted"

	type q = workqueue.TypedRateLimitingInterface[mcreconcile.Request]
	runFanOutCases(t, []fanOutCase{
		{"create None", func(q q) { h.Create(ctx, event.TypedCreateEvent[client.Object]{Object: none}, q) }, 3},
		{"create None, not allowlisted", func(q q) { h.Create(ctx, event.TypedCreateEvent[client.Object]{Object: unlisted}, q) }, 0},
		{"create Required", func(q q) { h.Create(ctx, event.TypedCreateEvent[client.Object]{Object: required}, q) }, 0},
		{"None to Required", func(q q) {
			h.Update(ctx, event.TypedUpdateEvent[client.Object]{ObjectOld: none, ObjectNew: required}, q)
		}, 3},
		{"Required to None", func(q q) {
			h.Update(ctx, event.TypedUpdateEvent[client.Object]{ObjectOld: required, ObjectNew: none}, q)
		}, 3},
		{"Required edit", func(q q) {
			h.Update(ctx, event.TypedUpdateEvent[client.Object]{ObjectOld: requiredSameGen, ObjectNew: required}, q)
		}, 0},
		{"no spec change", func(q q) {
			h.Update(ctx, event.TypedUpdateEvent[client.Object]{ObjectOld: none, ObjectNew: none}, q)
		}, 0},
		{"delete None", func(q q) { h.Delete(ctx, event.TypedDeleteEvent[client.Object]{Object: none}, q) }, 3},
		{"delete Required", func(q q) { h.Delete(ctx, event.TypedDeleteEvent[client.Object]{Object: required}, q) }, 0},
	})
}

// A new configuration for an agent that reaches every project fans out to
// every engaged project at once, rather than waiting for the periodic pass.
func TestCapabilityBinding_AgentConfigurationFanOut(t *testing.T) {
	unlisted := cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone)
	unlisted.Name = "not-allowlisted"
	r := trackedReconciler(cbRootClient(
		cbAgentWithEntitlement(servicesv1alpha1.PhasePublished, servicesv1alpha1.VisibilityEntitlementNone),
		unlisted,
	))
	h := r.agentConfigurationHandler()
	ctx := context.Background()

	v1 := cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhaseDraft)
	v1.Generation = 1
	v1Published := cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished)
	v1Published.Generation = 2
	ofUnlisted := v1Published.DeepCopy()
	ofUnlisted.Spec.ServiceAgentRef.Name = unlisted.Name
	ofMissing := v1Published.DeepCopy()
	ofMissing.Spec.ServiceAgentRef.Name = "no-such-agent"

	type q = workqueue.TypedRateLimitingInterface[mcreconcile.Request]
	runFanOutCases(t, []fanOutCase{
		{"create", func(q q) { h.Create(ctx, event.TypedCreateEvent[client.Object]{Object: v1Published}, q) }, 3},
		{"publish", func(q q) {
			h.Update(ctx, event.TypedUpdateEvent[client.Object]{ObjectOld: v1, ObjectNew: v1Published}, q)
		}, 3},
		{"no spec change", func(q q) {
			h.Update(ctx, event.TypedUpdateEvent[client.Object]{ObjectOld: v1Published, ObjectNew: v1Published}, q)
		}, 0},
		{"delete", func(q q) { h.Delete(ctx, event.TypedDeleteEvent[client.Object]{Object: v1Published}, q) }, 3},
		{"agent not allowlisted", func(q q) { h.Create(ctx, event.TypedCreateEvent[client.Object]{Object: ofUnlisted}, q) }, 0},
		{"agent missing", func(q q) { h.Create(ctx, event.TypedCreateEvent[client.Object]{Object: ofMissing}, q) }, 0},
	})
}

func countCapabilityBindings(t *testing.T, c client.Client) int {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(capabilityBindingGVK.GroupVersion().WithKind(capabilityBindingGVK.Kind + "List"))
	if err := c.List(context.Background(), list); err != nil {
		t.Fatalf("list CapabilityBindings: %v", err)
	}
	return len(list.Items)
}
