// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
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
	base := setupEntitlement("compute", "compute.miloapis.com", metav1.ConditionTrue, "Complete")
	base.UID = "compute-uid"
	base.ResourceVersion = "1"

	update := func(mutate func(*servicesv1alpha1.ServiceEntitlement)) bool {
		next := base.DeepCopy()
		next.ResourceVersion = "2"
		mutate(next)
		return p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: next})
	}
	ownSetup := base.DeepCopy()
	setCondition(servicesv1alpha1.ConditionTypeSetupComplete, metav1.ConditionTrue, "AllStagesComplete")(ownSetup)
	ownSetup.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{RequestedAt: metav1.Now()}
	ledger.recordSetup(projectionProject, ownSetup)

	for name, tc := range map[string]struct {
		mutate func(*servicesv1alpha1.ServiceEntitlement)
		want   bool
	}{
		"finalizer": {func(e *servicesv1alpha1.ServiceEntitlement) { e.Finalizers = []string{"x"} }, false},
		"provisioning evaluation": {func(e *servicesv1alpha1.ServiceEntitlement) {
			now := metav1.Now()
			e.Status.LastProvisioningEvaluation = &now
		}, false},
		"condition message": {func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Conditions[0].Message = "changed"
		}, false},
		"phase": {func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Phase = servicesv1alpha1.EntitlementPhase("Revoked")
		}, true},
		"provisioned reason": {setCondition(servicesv1alpha1.ConditionTypeProvisioned, metav1.ConditionTrue, "Reinstalled"), true},
		"suspended":          {setCondition(ConditionTypeSuspended, metav1.ConditionTrue, "ProjectSuspended"), true},
		"deleting": {func(e *servicesv1alpha1.ServiceEntitlement) {
			now := metav1.Now()
			e.DeletionTimestamp = &now
		}, true},
		"own setup write": {func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status = *ownSetup.Status.DeepCopy()
		}, false},
		"setup changed by someone else": {func(e *servicesv1alpha1.ServiceEntitlement) {
			e.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{RequestedAt: metav1.NewTime(time.Unix(1, 0))}
		}, true},
	} {
		if got := update(tc.mutate); got != tc.want {
			t.Errorf("%s: admitted = %v, want %v", name, got, tc.want)
		}
	}
	if !p.Create(event.CreateEvent{Object: base}) || !p.Delete(event.DeleteEvent{Object: base}) {
		t.Error("entitlement create and delete events must always pass")
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
