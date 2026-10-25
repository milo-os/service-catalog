// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// projectionPassHarness stands in for the informer and workqueue in front of
// LocationProjectionReconciler. Every ServiceEntitlement write, by the scripted
// controllers or by the reconciler itself, becomes a watch event that the
// predicate under test admits or drops, and an admitted event queues one more
// project pass. Passes run as soon as they are queued, so no two events
// coalesce: the count is the worst case a real queue can only improve on.
type projectionPassHarness struct {
	t         *testing.T
	ctx       context.Context
	store     client.WithWatch
	consumer  client.Client
	r         *LocationProjectionReconciler
	predicate predicate.Predicate
	seen      map[string]*servicesv1alpha1.ServiceEntitlement
	queued    bool
	passes    int
	// failSetupApply, when set, fails the reconciler's setup write for the
	// named entitlement.
	failSetupApply string
}

func newProjectionPassHarness(t *testing.T, filtered bool) *projectionPassHarness {
	t.Helper()
	h := &projectionPassHarness{t: t, ctx: context.Background(), seen: map[string]*servicesv1alpha1.ServiceEntitlement{}}
	h.store = projectionConsumerClient()
	h.consumer = interceptor.NewClient(h.store, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return h.observeAfter(c.Create(ctx, obj, opts...))
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return h.observeAfter(c.Update(ctx, obj, opts...))
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return h.observeAfter(c.Patch(ctx, obj, patch, opts...))
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			return h.observeAfter(c.SubResource(sub).Update(ctx, obj, opts...))
		},
		SubResourceApply: func(ctx context.Context, c client.Client, sub string, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
			if h.failSetupApply != "" {
				for _, opt := range opts {
					if owner, ok := opt.(client.FieldOwner); ok && string(owner) == setupStatusOwner.manager &&
						applyConfigurationName(obj) == h.failSetupApply {
						return errors.New("process stopped")
					}
				}
			}
			return h.observeAfter(c.SubResource(sub).Apply(ctx, obj, opts...))
		},
	})

	root := projectionRootClient(
		setupService("compute", "compute.miloapis.com", "networking"),
		setupService("networking", "networking.miloapis.com", "ipam"),
		setupService("ipam", "ipam.miloapis.com"),
		passHarnessConfig("compute"), passHarnessConfig("networking"), passHarnessConfig("ipam"),
	)
	mgr := newTestManager()
	mgr.add(projectionProject, h.consumer)
	h.r = &LocationProjectionReconciler{rootClient: root, rootAPIReader: root, Manager: mgr, Scheme: projectionScheme()}
	h.predicate = predicate.Funcs{}
	if filtered {
		h.r.writes = newProjectionWriteLedger()
		h.predicate = entitlementProjectionPredicate(h.r.writes)
	}
	return h
}

func passHarnessConfig(service string) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: service, UID: types.UID(service + "-config")},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: service},
			Phase:      servicesv1alpha1.PhasePublished,
		},
	}
}

func applyConfigurationName(obj runtime.ApplyConfiguration) string {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return ""
	}
	return (&unstructured.Unstructured{Object: u}).GetName()
}

// observeAfter turns whatever changed in the store into watch events, as an
// informer would, once a write has succeeded.
func (h *projectionPassHarness) observeAfter(err error) error {
	if err != nil {
		return err
	}
	var list servicesv1alpha1.ServiceEntitlementList
	if err := h.store.List(h.ctx, &list); err != nil {
		h.t.Fatal(err)
	}
	present := map[string]bool{}
	for i := range list.Items {
		current := list.Items[i].DeepCopy()
		present[current.Name] = true
		previous, ok := h.seen[current.Name]
		switch {
		case !ok:
			h.queued = h.queued || h.predicate.Create(event.CreateEvent{Object: current})
		case previous.ResourceVersion != current.ResourceVersion:
			h.queued = h.queued || h.predicate.Update(event.UpdateEvent{ObjectOld: previous, ObjectNew: current})
		}
		h.seen[current.Name] = current
	}
	for name, previous := range h.seen {
		if !present[name] {
			h.queued = h.queued || h.predicate.Delete(event.DeleteEvent{Object: previous})
			delete(h.seen, name)
		}
	}
	return nil
}

// restart models a new process: an empty ledger, and watches that start with
// a create event for every existing entitlement.
func (h *projectionPassHarness) restart() {
	h.r.writes = newProjectionWriteLedger()
	h.predicate = entitlementProjectionPredicate(h.r.writes)
	h.seen = map[string]*servicesv1alpha1.ServiceEntitlement{}
	h.queued = false
	if err := h.observeAfter(nil); err != nil {
		h.t.Fatal(err)
	}
}

// settle runs queued passes until no admitted event is left.
func (h *projectionPassHarness) settle() error {
	for h.queued {
		h.queued = false
		h.passes++
		if _, err := h.r.Reconcile(h.ctx, mcreconcile.Request{
			Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: locationProjectionRequestName}},
			ClusterName: projectionProject,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (h *projectionPassHarness) step(write func()) {
	h.t.Helper()
	write()
	if err := h.settle(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *projectionPassHarness) get(name string) *servicesv1alpha1.ServiceEntitlement {
	h.t.Helper()
	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := h.store.Get(h.ctx, types.NamespacedName{Name: name}, &entitlement); err != nil {
		h.t.Fatal(err)
	}
	return &entitlement
}

func (h *projectionPassHarness) create(name, dependencyOf string) {
	h.t.Helper()
	entitlement := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
		Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: name}},
	}
	if dependencyOf != "" {
		entitlement.Labels = map[string]string{"services.miloapis.com/dependency-of": dependencyOf}
	}
	if err := h.consumer.Create(h.ctx, entitlement); err != nil {
		h.t.Fatal(err)
	}
}

func (h *projectionPassHarness) addFinalizer(name string) {
	h.t.Helper()
	entitlement := h.get(name)
	entitlement.Finalizers = append(entitlement.Finalizers, serviceEntitlementFinalizer)
	if err := h.consumer.Update(h.ctx, entitlement); err != nil {
		h.t.Fatal(err)
	}
}

func (h *projectionPassHarness) writeStatus(owner entitlementStatusOwner, name string, mutate func(*servicesv1alpha1.ServiceEntitlement)) {
	h.t.Helper()
	entitlement := h.get(name)
	before := entitlement.DeepCopy()
	mutate(entitlement)
	if err := applyEntitlementStatus(h.ctx, h.consumer, owner, before, entitlement); err != nil {
		h.t.Fatal(err)
	}
}

func setCondition(conditionType string, status metav1.ConditionStatus, reason string) func(*servicesv1alpha1.ServiceEntitlement) {
	return func(e *servicesv1alpha1.ServiceEntitlement) {
		apimeta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{
			Type: conditionType, Status: status, Reason: reason, Message: reason, ObservedGeneration: e.Generation,
		})
	}
}

// enableDependencyChain replays the status writes the other controllers make
// while compute -> networking -> IPAM is enabled, in the order staging showed
// them: each entitlement is authorized, evaluated for provisioning while its
// dependencies are pending, and then completes from the leaf upward.
func (h *projectionPassHarness) enableDependencyChain() {
	authorize := func(name string) {
		h.step(func() { h.addFinalizer(name) })
		h.step(func() {
			h.writeStatus(entitlementReadyOwner, name, func(e *servicesv1alpha1.ServiceEntitlement) {
				e.Status.Phase = servicesv1alpha1.EntitlementPhaseActive
				e.Status.ServiceName = name + ".miloapis.com"
				e.Status.ObservedGeneration = e.Generation
				setCondition(servicesv1alpha1.ConditionTypeReady, metav1.ConditionTrue, "Entitled")(e)
			})
		})
		h.step(func() {
			h.writeStatus(suspendedConditionOwner, name, setCondition(ConditionTypeSuspended, metav1.ConditionFalse, "ProjectActive"))
		})
	}
	evaluate := func(name string, status metav1.ConditionStatus, reason string) {
		h.step(func() {
			h.writeStatus(provisioningStatusOwner, name, func(e *servicesv1alpha1.ServiceEntitlement) {
				now := metav1.NewTime(time.Now().Add(time.Duration(h.passes) * time.Second))
				e.Status.LastProvisioningEvaluation = &now
				setCondition(servicesv1alpha1.ConditionTypeProvisioned, status, reason)(e)
				if status == metav1.ConditionTrue {
					e.Status.ProvisionedResources = []servicesv1alpha1.ProvisionedResourceStatus{{Name: name + "-resources"}}
				}
			})
		})
	}
	dependencies := func(name string, status metav1.ConditionStatus, reason string) {
		h.step(func() {
			h.writeStatus(dependenciesConditionOwner, name, setCondition(servicesv1alpha1.ConditionTypeDependenciesSatisfied, status, reason))
		})
	}

	h.step(func() { h.create("compute", "") })
	authorize("compute")
	dependencies("compute", metav1.ConditionFalse, "DependenciesPending")
	evaluate("compute", metav1.ConditionFalse, "WaitingForDependencies")

	h.step(func() { h.create("networking", "compute") })
	h.step(func() {
		h.writeStatus(dependencyOriginOwner, "networking", func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Origin = servicesv1alpha1.EntitlementOrigin("Dependency")
			e.Status.DependencyOf = "compute"
		})
	})
	authorize("networking")
	dependencies("networking", metav1.ConditionFalse, "DependenciesPending")
	evaluate("networking", metav1.ConditionFalse, "WaitingForDependencies")

	h.step(func() { h.create("ipam", "networking") })
	authorize("ipam")
	dependencies("ipam", metav1.ConditionTrue, "DependenciesSatisfied")
	evaluate("ipam", metav1.ConditionFalse, "Installing")
	evaluate("ipam", metav1.ConditionTrue, "Provisioned")

	dependencies("networking", metav1.ConditionTrue, "DependenciesSatisfied")
	evaluate("networking", metav1.ConditionFalse, "Installing")
	evaluate("networking", metav1.ConditionTrue, "Provisioned")
	// A re-evaluation that changes nothing but its timestamp.
	evaluate("networking", metav1.ConditionTrue, "Provisioned")

	dependencies("compute", metav1.ConditionTrue, "DependenciesSatisfied")
	evaluate("compute", metav1.ConditionFalse, "Installing")
	evaluate("compute", metav1.ConditionTrue, "Provisioned")
}

func (h *projectionPassHarness) requireSetupComplete(names ...string) {
	h.t.Helper()
	for _, name := range names {
		entitlement := h.get(name)
		if !apimeta.IsStatusConditionTrue(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete) {
			h.t.Errorf("%s SetupComplete = %#v, want True", name,
				apimeta.FindStatusCondition(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete))
		}
	}
}

// TestLocationProjectionPassesForDependencyChain counts the project passes a
// compute -> networking -> IPAM setup costs with and without the event filter.
func TestLocationProjectionPassesForDependencyChain(t *testing.T) {
	unfiltered := newProjectionPassHarness(t, false)
	unfiltered.enableDependencyChain()
	unfiltered.requireSetupComplete("ipam", "networking", "compute")

	filtered := newProjectionPassHarness(t, true)
	filtered.enableDependencyChain()
	filtered.requireSetupComplete("ipam", "networking", "compute")

	t.Logf("project passes for compute -> networking -> IPAM: %d unfiltered, %d filtered", unfiltered.passes, filtered.passes)
	// Pinned so a regression in the filter shows up as a changed count.
	if unfiltered.passes != 44 {
		t.Errorf("unfiltered passes = %d, want 44", unfiltered.passes)
	}
	// One pass per write that changes something a pass reads; none for the
	// reconciler's own echoes or for writes to fields it never reads.
	if filtered.passes != 19 {
		t.Errorf("filtered passes = %d, want 19", filtered.passes)
	}
}

// TestLocationProjectionRecoversFromStopBetweenDependencyAndParent stops the
// process after a dependency's setup write lands and before its parent's
// does. The dependency's echo is the reconciler's own write and is dropped,
// so it is the restart that must finish the parent.
func TestLocationProjectionRecoversFromStopBetweenDependencyAndParent(t *testing.T) {
	h := newProjectionPassHarness(t, true)
	h.step(func() { h.create("compute", "") })
	h.step(func() { h.create("networking", "compute") })
	h.step(func() { h.create("ipam", "networking") })
	complete := func(name string) {
		h.writeStatus(entitlementReadyOwner, name, func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Phase = servicesv1alpha1.EntitlementPhaseActive
			setCondition(servicesv1alpha1.ConditionTypeReady, metav1.ConditionTrue, "Entitled")(e)
		})
		h.writeStatus(dependenciesConditionOwner, name, setCondition(servicesv1alpha1.ConditionTypeDependenciesSatisfied, metav1.ConditionTrue, "DependenciesSatisfied"))
		h.writeStatus(provisioningStatusOwner, name, setCondition(servicesv1alpha1.ConditionTypeProvisioned, metav1.ConditionTrue, "Provisioned"))
	}
	complete("compute")
	complete("networking")
	complete("ipam")
	h.failSetupApply = "compute"
	// The pass writes IPAM and networking, then stops at compute. A stopped
	// process does not retry.
	if err := h.settle(); err == nil {
		t.Fatal("expected the pass to stop at compute")
	}
	if h.queued {
		t.Fatal("the echo of the pass's own writes queued another pass")
	}
	h.requireSetupComplete("ipam", "networking")
	if apimeta.IsStatusConditionTrue(h.get("compute").Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete) {
		t.Fatal("compute completed before the restart")
	}

	h.failSetupApply = ""
	h.restart()
	if err := h.settle(); err != nil {
		t.Fatal(err)
	}
	h.requireSetupComplete("ipam", "networking", "compute")
}

func TestEntitlementProjectionPredicate(t *testing.T) {
	ledger := newProjectionWriteLedger()
	p := entitlementProjectionPredicate(ledger)
	base := setupEntitlement("compute", "compute.miloapis.com", metav1.ConditionFalse, "DependenciesPending")
	base.UID = "compute-uid"
	base.ResourceVersion = "1"
	base.Generation = 1
	setCondition(ConditionTypeSuspended, metav1.ConditionFalse, "ProjectActive")(base)
	setCondition(servicesv1alpha1.ConditionTypeSetupComplete, metav1.ConditionFalse, "DependenciesPending")(base)
	base.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{RequestedAt: metav1.NewTime(time.Unix(100, 0))}

	update := func(mutate func(*servicesv1alpha1.ServiceEntitlement)) bool {
		next := base.DeepCopy()
		next.ResourceVersion = "2"
		mutate(next)
		return p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: next})
	}
	// condition changes one field of one condition, leaving every other field,
	// including its transition time, as it was.
	condition := func(conditionType string, mutate func(*metav1.Condition)) func(*servicesv1alpha1.ServiceEntitlement) {
		return func(e *servicesv1alpha1.ServiceEntitlement) {
			c := apimeta.FindStatusCondition(e.Status.Conditions, conditionType)
			if c == nil {
				t.Fatalf("base has no %s condition", conditionType)
			}
			mutate(c)
		}
	}
	flip := func(c *metav1.Condition) {
		if c.Status == metav1.ConditionTrue {
			c.Status = metav1.ConditionFalse
		} else {
			c.Status = metav1.ConditionTrue
		}
	}
	reason := func(c *metav1.Condition) { c.Reason = "Changed" }
	moved := func(c *metav1.Condition) { c.LastTransitionTime = metav1.NewTime(time.Unix(200, 0)) }

	ownSetup := base.DeepCopy()
	setCondition(servicesv1alpha1.ConditionTypeSetupComplete, metav1.ConditionTrue, "AllStagesComplete")(ownSetup)
	ownSetup.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{RequestedAt: metav1.NewTime(time.Unix(100, 0)), AuthorizedAt: &metav1.Time{Time: time.Unix(150, 0)}}
	ledger.recordSetup(projectionProject, ownSetup)

	for name, tc := range map[string]struct {
		mutate func(*servicesv1alpha1.ServiceEntitlement)
		want   bool
	}{
		// Inputs, each changed on its own.
		"generation":   {func(e *servicesv1alpha1.ServiceEntitlement) { e.Generation++ }, true},
		"service ref":  {func(e *servicesv1alpha1.ServiceEntitlement) { e.Spec.ServiceRef.Name = "storage" }, true},
		"service name": {func(e *servicesv1alpha1.ServiceEntitlement) { e.Status.ServiceName = "storage.miloapis.com" }, true},
		"provider activation": {func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Spec.ProviderActivation = &servicesv1alpha1.ProviderActivation{RequestedAt: metav1.NewTime(time.Unix(50, 0))}
		}, true},
		"phase": {func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Phase = servicesv1alpha1.EntitlementPhase("Revoked")
		}, true},
		"deleting": {func(e *servicesv1alpha1.ServiceEntitlement) {
			now := metav1.Now()
			e.DeletionTimestamp = &now
		}, true},
		"ready removed": {func(e *servicesv1alpha1.ServiceEntitlement) {
			apimeta.RemoveStatusCondition(&e.Status.Conditions, servicesv1alpha1.ConditionTypeReady)
		}, true},
		"ready status":                  {condition(servicesv1alpha1.ConditionTypeReady, flip), true},
		"ready reason":                  {condition(servicesv1alpha1.ConditionTypeReady, reason), true},
		"ready transition time":         {condition(servicesv1alpha1.ConditionTypeReady, moved), true},
		"dependencies satisfied status": {condition(servicesv1alpha1.ConditionTypeDependenciesSatisfied, flip), true},
		"dependencies satisfied reason": {condition(servicesv1alpha1.ConditionTypeDependenciesSatisfied, reason), true},
		"dependencies satisfied removed": {func(e *servicesv1alpha1.ServiceEntitlement) {
			apimeta.RemoveStatusCondition(&e.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied)
		}, true},
		"provisioned status": {condition(servicesv1alpha1.ConditionTypeProvisioned, flip), true},
		"provisioned reason": {condition(servicesv1alpha1.ConditionTypeProvisioned, reason), true},
		"provisioned removed": {func(e *servicesv1alpha1.ServiceEntitlement) {
			apimeta.RemoveStatusCondition(&e.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned)
		}, true},
		"suspended status": {condition(ConditionTypeSuspended, flip), true},

		// Fields a pass never reads.
		"finalizer": {func(e *servicesv1alpha1.ServiceEntitlement) { e.Finalizers = []string{"x"} }, false},
		"labels":    {func(e *servicesv1alpha1.ServiceEntitlement) { e.Labels = map[string]string{"x": "y"} }, false},
		"provisioning evaluation": {func(e *servicesv1alpha1.ServiceEntitlement) {
			now := metav1.Now()
			e.Status.LastProvisioningEvaluation = &now
		}, false},
		"observed generation": {func(e *servicesv1alpha1.ServiceEntitlement) { e.Status.ObservedGeneration++ }, false},
		"condition message":   {condition(servicesv1alpha1.ConditionTypeReady, func(c *metav1.Condition) { c.Message = "changed" }), false},
		"suspended reason":    {condition(ConditionTypeSuspended, reason), false},
		"suspended False removed": {func(e *servicesv1alpha1.ServiceEntitlement) {
			apimeta.RemoveStatusCondition(&e.Status.Conditions, ConditionTypeSuspended)
		}, false},

		// The setup aggregate this reconciler writes.
		"own setup write": {func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status = *ownSetup.Status.DeepCopy()
		}, false},
		"setup changed by someone else": {func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{RequestedAt: metav1.NewTime(time.Unix(1, 0))}
		}, true},
		"setup removed by someone else": {func(e *servicesv1alpha1.ServiceEntitlement) { e.Status.Setup = nil }, true},
		"only setupComplete changed by someone else": {
			condition(servicesv1alpha1.ConditionTypeSetupComplete, flip), true,
		},
	} {
		if got := update(tc.mutate); got != tc.want {
			t.Errorf("%s: admitted = %v, want %v", name, got, tc.want)
		}
	}
	if !p.Create(event.CreateEvent{Object: base}) || !p.Delete(event.DeleteEvent{Object: base}) {
		t.Error("entitlement create and delete events must always pass")
	}
}

// TestProjectionInputsCoverEveryFieldAPassReads changes each field of a
// ServiceEntitlement on its own and runs a real pass before and after. Any
// change that alters what the pass produces must be admitted by the filter, so
// a new reader in Reconcile or aggregateSetup that projectionInputs does not
// know about fails here.
func TestProjectionInputsCoverEveryFieldAPassReads(t *testing.T) {
	base := func() []*servicesv1alpha1.ServiceEntitlement {
		compute := setupEntitlement("compute", "compute.miloapis.com", metav1.ConditionTrue, "DependenciesSatisfied")
		// The dependency is entitled by its canonical name, so compute's
		// lookup of "networking" resolves through status.serviceName.
		networking := setupEntitlement("networking", "networking", metav1.ConditionTrue, "DependenciesSatisfied")
		networking.Spec.ServiceRef.Name = "networking.miloapis.com"
		for _, e := range []*servicesv1alpha1.ServiceEntitlement{compute, networking} {
			e.UID = types.UID(e.Name + "-uid")
			e.Generation = 1
			e.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
			e.Finalizers = []string{serviceEntitlementFinalizer}
			setCondition(ConditionTypeSuspended, metav1.ConditionFalse, "ProjectActive")(e)
			for i := range e.Status.Conditions {
				e.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Unix(1100, 0))
			}
		}
		return []*servicesv1alpha1.ServiceEntitlement{compute, networking}
	}
	outcome := func(entitlements []*servicesv1alpha1.ServiceEntitlement) string {
		t.Helper()
		computeConfig := passHarnessConfig("compute")
		computeConfig.Spec.Locations = &servicesv1alpha1.ServiceLocationConfig{SupportedClasses: []servicesv1alpha1.LocationClassName{projectionClass}}
		availability := availableAt(projectionLocation, true)
		availability.Spec.ServiceRef.Name = "compute"
		root := projectionRootClient(
			setupService("compute", "compute.miloapis.com", "networking"),
			setupService("networking", "networking.miloapis.com"),
			computeConfig, passHarnessConfig("networking.miloapis.com"),
			platformLocation(projectionLocation, true, projectionClass),
			availability,
		)
		objects := make([]client.Object, 0, len(entitlements))
		for _, e := range entitlements {
			objects = append(objects, e.DeepCopy())
		}
		consumer := projectionConsumerClient(objects...)
		mgr := newTestManager()
		mgr.add(projectionProject, consumer)
		r := &LocationProjectionReconciler{
			rootClient: root, rootAPIReader: root, Manager: mgr, Scheme: projectionScheme(),
			projects: newProjectionProjectIndex(),
		}
		if _, err := r.Reconcile(context.Background(), mcreconcile.Request{
			Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: locationProjectionRequestName}},
			ClusterName: projectionProject,
		}); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Setup     map[string]string
			Locations []string
			Mirrors   []string
			Services  []string
		}
		result.Setup = map[string]string{}
		var list servicesv1alpha1.ServiceEntitlementList
		if err := consumer.List(context.Background(), &list); err != nil {
			t.Fatal(err)
		}
		for i := range list.Items {
			result.Setup[list.Items[i].Name] = normalizedSetup(&list.Items[i])
		}
		locations := &unstructured.UnstructuredList{}
		locations.SetGroupVersionKind(LocationGVK.GroupVersion().WithKind("LocationList"))
		if err := consumer.List(context.Background(), locations); err != nil {
			t.Fatal(err)
		}
		for _, l := range locations.Items {
			result.Locations = append(result.Locations, l.GetName())
		}
		var mirrors servicesv1alpha1.ServiceAvailabilityList
		if err := consumer.List(context.Background(), &mirrors); err != nil {
			t.Fatal(err)
		}
		for _, m := range mirrors.Items {
			result.Mirrors = append(result.Mirrors, m.Name)
		}
		for _, service := range []string{"compute", "networking", "networking.miloapis.com", "x1", "x2"} {
			if len(r.projects.ProjectsForService(service)) > 0 {
				result.Services = append(result.Services, service)
			}
		}
		raw, _ := json.Marshal(result)
		return string(raw)
	}

	baseline := outcome(base())
	if !strings.Contains(baseline, projectionLocation) || !strings.Contains(baseline, "AllStagesComplete") {
		t.Fatalf("baseline must project a Location and complete setup, or readers go untested: %s", baseline)
	}
	p := entitlementProjectionPredicate(newProjectionWriteLedger())
	checked, changed := 0, 0
	for _, m := range entitlementFieldMutations() {
		for target := range 2 {
			entitlements := base()
			before := entitlements[target].DeepCopy()
			m.apply(entitlements[target])
			if reflect.DeepEqual(before, entitlements[target]) {
				continue
			}
			checked++
			got := outcome(entitlements)
			if got == baseline {
				continue
			}
			changed++
			if !p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: entitlements[target]}) {
				t.Errorf("%s on %s changes the pass outcome but is filtered out\nbefore: %s\nafter:  %s",
					m.name, before.Name, baseline, got)
			}
		}
	}
	t.Logf("%d field changes checked, %d changed the outcome", checked, changed)
	if changed == 0 {
		t.Fatal("no mutation changed the outcome; the test is not exercising the pass")
	}
}

// normalizedSetup is an entitlement's setup aggregate without the timestamps
// a pass takes from the clock.
func normalizedSetup(e *servicesv1alpha1.ServiceEntitlement) string {
	copied := e.DeepCopy()
	if setup := copied.Status.Setup; setup != nil {
		for i := range setup.Stages {
			setup.Stages[i].LastTransitionTime = metav1.Time{}
		}
		if setup.FirstCompletedAt != nil {
			setup.FirstCompletedAt = &metav1.Time{}
		}
	}
	if c := apimeta.FindStatusCondition(copied.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete); c != nil {
		c.LastTransitionTime = metav1.Time{}
	}
	return setupFingerprint(copied)
}

type entitlementFieldMutation struct {
	name  string
	apply func(*servicesv1alpha1.ServiceEntitlement)
}

// entitlementFieldMutations sets and clears every metadata field a client can
// write, every top-level spec and status field, and every field of every
// condition. The setup aggregate is left out: it is this reconciler's own
// output, covered by the ledger cases in TestEntitlementProjectionPredicate.
func entitlementFieldMutations() []entitlementFieldMutation {
	var out []entitlementFieldMutation
	add := func(name string, apply func(*servicesv1alpha1.ServiceEntitlement)) {
		out = append(out, entitlementFieldMutation{name: name, apply: apply})
	}
	add("generation", func(e *servicesv1alpha1.ServiceEntitlement) { e.Generation++ })
	add("deletionTimestamp", func(e *servicesv1alpha1.ServiceEntitlement) {
		now := metav1.NewTime(time.Unix(2000, 0))
		e.DeletionTimestamp = &now
	})
	add("labels", func(e *servicesv1alpha1.ServiceEntitlement) { e.Labels = map[string]string{dependencyOfLabel: "x"} })
	add("annotations", func(e *servicesv1alpha1.ServiceEntitlement) { e.Annotations = map[string]string{"x": "y"} })
	add("finalizers", func(e *servicesv1alpha1.ServiceEntitlement) { e.Finalizers = append(e.Finalizers, "x") })

	for _, part := range []struct {
		name string
		get  func(*servicesv1alpha1.ServiceEntitlement) reflect.Value
	}{
		{"spec", func(e *servicesv1alpha1.ServiceEntitlement) reflect.Value { return reflect.ValueOf(&e.Spec).Elem() }},
		{"status", func(e *servicesv1alpha1.ServiceEntitlement) reflect.Value { return reflect.ValueOf(&e.Status).Elem() }},
	} {
		fields := part.get(&servicesv1alpha1.ServiceEntitlement{}).Type()
		for i := range fields.NumField() {
			field := fields.Field(i)
			if field.Name == "Conditions" || field.Name == "Setup" {
				continue
			}
			for seed := range 3 {
				add(fmt.Sprintf("%s.%s set (%d)", part.name, field.Name, seed), func(e *servicesv1alpha1.ServiceEntitlement) {
					fillValue(part.get(e).Field(i), seed+1)
				})
			}
			add(fmt.Sprintf("%s.%s cleared", part.name, field.Name), func(e *servicesv1alpha1.ServiceEntitlement) {
				v := part.get(e).Field(i)
				v.Set(reflect.Zero(v.Type()))
			})
		}
	}

	for _, conditionType := range []string{
		servicesv1alpha1.ConditionTypeReady,
		ConditionTypeSuspended,
		servicesv1alpha1.ConditionTypeDependenciesSatisfied,
		servicesv1alpha1.ConditionTypeProvisioned,
	} {
		with := func(mutate func(*metav1.Condition)) func(*servicesv1alpha1.ServiceEntitlement) {
			return func(e *servicesv1alpha1.ServiceEntitlement) {
				if c := apimeta.FindStatusCondition(e.Status.Conditions, conditionType); c != nil {
					mutate(c)
				}
			}
		}
		add(conditionType+" status", with(func(c *metav1.Condition) {
			if c.Status == metav1.ConditionTrue {
				c.Status = metav1.ConditionFalse
			} else {
				c.Status = metav1.ConditionTrue
			}
		}))
		add(conditionType+" unknown", with(func(c *metav1.Condition) { c.Status = metav1.ConditionUnknown }))
		add(conditionType+" reason", with(func(c *metav1.Condition) { c.Reason = "Changed" }))
		add(conditionType+" message", with(func(c *metav1.Condition) { c.Message = "changed" }))
		add(conditionType+" observedGeneration", with(func(c *metav1.Condition) { c.ObservedGeneration += 5 }))
		add(conditionType+" lastTransitionTime", with(func(c *metav1.Condition) {
			c.LastTransitionTime = metav1.NewTime(time.Unix(3000, 0))
		}))
		add(conditionType+" removed", func(e *servicesv1alpha1.ServiceEntitlement) {
			apimeta.RemoveStatusCondition(&e.Status.Conditions, conditionType)
		})
	}
	add("unrelated condition added", func(e *servicesv1alpha1.ServiceEntitlement) {
		apimeta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{Type: "Other", Status: metav1.ConditionTrue, Reason: "X"})
	})
	return out
}

// fillValue sets v to a non-zero value derived from seed, recursively.
func fillValue(v reflect.Value, seed int) {
	if v.Type() == reflect.TypeFor[metav1.Time]() {
		v.Set(reflect.ValueOf(metav1.NewTime(time.Unix(int64(5000+seed), 0))))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprintf("x%d", seed))
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + int64(seed))
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillValue(v.Elem(), seed)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillValue(v.Field(i), seed)
			}
		}
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillValue(elem, seed)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		fillValue(key, seed)
		value := reflect.New(v.Type().Elem()).Elem()
		fillValue(value, seed)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, value)
		v.Set(m)
	}
}

func TestPruneFailureDoesNotHideALaterForeignDelete(t *testing.T) {
	ctx := context.Background()
	availability := availableAt(projectionLocation, true)
	root := projectionRootClient(
		publishedProjectionConfig(projectionClass),
		platformLocation(projectionLocation, true, projectionClass),
		availability,
	)
	failDeletes := false
	consumer := interceptor.NewClient(projectionConsumerClient(activeProjectionEntitlement()), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID(types.UID(obj.GetObjectKind().GroupVersionKind().Kind + "/" + obj.GetName()))
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if failDeletes {
				return errors.New("delete unavailable")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	mgr := newTestManager()
	mgr.add(projectionProject, consumer)
	r := &LocationProjectionReconciler{
		rootClient: root, rootAPIReader: root, Manager: mgr, Scheme: projectionScheme(),
		writes: newProjectionWriteLedger(),
	}
	reconcile := func() error {
		_, err := r.Reconcile(ctx, mcreconcile.Request{
			Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: locationProjectionRequestName}},
			ClusterName: projectionProject,
		})
		return err
	}
	setAvailable := func(available bool) {
		var sa servicesv1alpha1.ServiceAvailability
		if err := root.Get(ctx, types.NamespacedName{Name: availability.Name}, &sa); err != nil {
			t.Fatal(err)
		}
		sa.Status = availableAt(projectionLocation, available).Status
		if err := root.Update(ctx, &sa); err != nil {
			t.Fatal(err)
		}
	}
	if err := reconcile(); err != nil {
		t.Fatal(err)
	}

	// The location stops being wanted, and its prune fails.
	setAvailable(false)
	failDeletes = true
	if err := reconcile(); err == nil {
		t.Fatal("expected the failed prune to fail the pass")
	}
	// It is wanted again before any retry, so the pass writes nothing to it.
	failDeletes = false
	setAvailable(true)
	if err := reconcile(); err != nil {
		t.Fatal(err)
	}

	location, ok := projectedLocation(t, consumer, projectionLocation)
	if !ok {
		t.Fatal("location is missing")
	}
	if !managedProjectionPredicate(r.writes).Delete(event.DeleteEvent{Object: location}) {
		t.Fatal("someone else's delete after a failed prune was taken for the reconciler's own")
	}
}

func TestManagedProjectionPredicateDropsOnlyOwnWrites(t *testing.T) {
	ctx := context.Background()
	root := projectionRootClient(
		publishedProjectionConfig(projectionClass),
		platformLocation(projectionLocation, true, projectionClass),
		availableAt(projectionLocation, true),
	)
	// The API server assigns UIDs; the fake client does not.
	consumer := interceptor.NewClient(projectionConsumerClient(activeProjectionEntitlement()), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID(types.UID(obj.GetObjectKind().GroupVersionKind().Kind + "/" + obj.GetName()))
			return c.Create(ctx, obj, opts...)
		},
	})
	mgr := newTestManager()
	mgr.add(projectionProject, consumer)
	r := &LocationProjectionReconciler{
		rootClient: root, rootAPIReader: root, Manager: mgr, Scheme: projectionScheme(),
		writes: newProjectionWriteLedger(),
	}
	if _, err := r.Reconcile(ctx, mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: locationProjectionRequestName}},
		ClusterName: projectionProject,
	}); err != nil {
		t.Fatal(err)
	}
	p := managedProjectionPredicate(r.writes)

	location, ok := projectedLocation(t, consumer, projectionLocation)
	if !ok {
		t.Fatal("location was not projected")
	}
	mirror, ok := mirroredAvailability(t, consumer, projectionService+"--"+projectionLocation)
	if !ok {
		t.Fatal("availability was not mirrored")
	}
	for _, obj := range []client.Object{location, mirror} {
		if p.Create(event.CreateEvent{Object: obj}) || p.Update(event.UpdateEvent{ObjectOld: obj, ObjectNew: obj}) {
			t.Errorf("%s %s: own write admitted", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
		}
	}

	// Someone else edits the projection: drift to repair.
	edited := location.DeepCopy()
	edited.SetLabels(map[string]string{labelManagedBy: labelManagedByValue, "edited": "true"})
	if err := consumer.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if !p.Update(event.UpdateEvent{ObjectOld: location, ObjectNew: edited}) {
		t.Error("a foreign edit to a projection was dropped")
	}
	// Someone else deletes it.
	if !p.Delete(event.DeleteEvent{Object: edited}) {
		t.Error("a foreign delete of a projection was dropped")
	}

	// The reconciler's own prune is not.
	r.writes.recordDelete(projectionProject, mirror)
	if p.Delete(event.DeleteEvent{Object: mirror}) {
		t.Error("own prune admitted")
	}
	if !p.Delete(event.DeleteEvent{Object: mirror}) {
		t.Error("a delete is only recognized once")
	}
}

func TestProjectionRootDispatcherTargetsServiceSpecEdits(t *testing.T) {
	index := newProjectionProjectIndex()
	index.Replace("by-name", []string{"networking"})
	index.Replace("by-canonical-name", []string{"networking.miloapis.com"})
	index.Replace("unrelated", []string{"storage"})
	events := make(chan event.TypedGenericEvent[mcreconcile.Request], 4)
	dispatcher := &projectionRootDispatcher{index: index, events: events}

	service := setupService("networking", "networking.miloapis.com")
	dispatcher.dispatchObjects(context.Background(), service)
	for _, want := range []string{"by-canonical-name", "by-name"} {
		if got := string((<-events).Object.ClusterName); got != want {
			t.Fatalf("Service dispatched to %q, want %q", got, want)
		}
	}
	if len(events) != 0 {
		t.Fatalf("Service dispatched to %d unrelated projects", len(events))
	}

	for name, tc := range map[string]struct {
		mutate func(*servicesv1alpha1.Service)
		want   bool
	}{
		"dependency added": {func(s *servicesv1alpha1.Service) {
			s.Spec.Dependencies = append(s.Spec.Dependencies, servicesv1alpha1.ServiceDependency{ServiceRef: servicesv1alpha1.ServiceRef{Name: "ipam"}})
		}, true},
		"canonical name": {func(s *servicesv1alpha1.Service) { s.Spec.ServiceName = "net.miloapis.com" }, true},
		"status only":    {func(s *servicesv1alpha1.Service) { s.Status.ObservedGeneration++ }, false},
		"labels only":    {func(s *servicesv1alpha1.Service) { s.Labels = map[string]string{"x": "y"} }, false},
	} {
		next := service.DeepCopy()
		tc.mutate(next)
		if got := projectionRelevantRootUpdate(service, next); got != tc.want {
			t.Errorf("%s: relevant = %v, want %v", name, got, tc.want)
		}
	}
	if !projectionRelevantRootUpdate(&servicesv1alpha1.ServiceAvailability{}, &servicesv1alpha1.ServiceAvailability{}) {
		t.Error("updates to other root kinds must always dispatch")
	}
}

// syncSignalingCache reports when the dispatcher has registered its handlers:
// Start waits for cache sync only after every AddEventHandler call.
type syncSignalingCache struct {
	*informertest.FakeInformers
	synced chan struct{}
}

func (c *syncSignalingCache) WaitForCacheSync(ctx context.Context) bool {
	close(c.synced)
	return c.FakeInformers.WaitForCacheSync(ctx)
}

// rootCacheManager serves only the cache, which is all Start reads.
type rootCacheManager struct {
	manager.Manager
	cache cache.Cache
}

func (m *rootCacheManager) GetCache() cache.Cache { return m.cache }

// Start must register the Service informer with the spec filter. Without the
// registration, Service edits never reach a pass; without the filter, every
// Service status write would.
func TestProjectionRootDispatcherStartWiresServiceUpdates(t *testing.T) {
	scheme := testScheme()
	location := &unstructured.Unstructured{}
	location.SetGroupVersionKind(LocationGVK)
	serviceInformer := controllertest.NewFakeInformer(controllertest.Synced)
	informers := &informertest.FakeInformers{
		Scheme: scheme,
		InformersByGVK: map[schema.GroupVersionKind]toolscache.SharedIndexInformer{
			servicesv1alpha1.GroupVersion.WithKind("Service"):              serviceInformer,
			servicesv1alpha1.GroupVersion.WithKind("ServiceAvailability"):  controllertest.NewFakeInformer(controllertest.Synced),
			servicesv1alpha1.GroupVersion.WithKind("ServiceConfiguration"): controllertest.NewFakeInformer(controllertest.Synced),
			LocationGVK: controllertest.NewFakeInformer(controllertest.Synced),
		},
	}
	rootCache := &syncSignalingCache{FakeInformers: informers, synced: make(chan struct{})}

	index := newProjectionProjectIndex()
	index.Replace("consumer", []string{"networking.miloapis.com"})
	events := make(chan event.TypedGenericEvent[mcreconcile.Request], 4)
	dispatcher := &projectionRootDispatcher{
		rootMgr: &rootCacheManager{cache: rootCache},
		gvk:     LocationGVK,
		index:   index,
		events:  events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start: %v", err)
		}
	}()
	select {
	case <-rootCache.synced:
	case err := <-done:
		t.Fatalf("Start returned before registering handlers: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not register handlers")
	}

	service := setupService("networking", "networking.miloapis.com")
	statusOnly := service.DeepCopy()
	statusOnly.Status.ObservedGeneration++
	serviceInformer.Update(service, statusOnly)
	if len(events) != 0 {
		t.Fatal("a Service status write dispatched a pass")
	}

	edited := service.DeepCopy()
	edited.Spec.Dependencies = append(edited.Spec.Dependencies, servicesv1alpha1.ServiceDependency{
		ServiceRef: servicesv1alpha1.ServiceRef{Name: "ipam"},
	})
	serviceInformer.Update(service, edited)
	select {
	case got := <-events:
		if project := string(got.Object.ClusterName); project != "consumer" {
			t.Errorf("Service edit dispatched to %q, want consumer", project)
		}
	default:
		t.Fatal("a Service dependency edit dispatched no pass")
	}
}
