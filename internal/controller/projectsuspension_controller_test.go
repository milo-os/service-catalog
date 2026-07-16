// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestProjectSuspensionPropagationReconciler(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	tests := []struct {
		name          string
		isSuspended   bool
		initialStatus bool
		wantSuspended bool
	}{
		{
			name:          "propagates suspension true",
			isSuspended:   true,
			initialStatus: false,
			wantSuspended: true,
		},
		{
			name:          "propagates suspension false",
			isSuspended:   false,
			initialStatus: true,
			wantSuspended: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Root cluster resources
			conditions := []metav1.Condition{}
			if tt.isSuspended {
				conditions = append(conditions, metav1.Condition{
					Type:               resourcemanagerv1alpha1.ProjectSuspended,
					Status:             metav1.ConditionTrue,
					Reason:             resourcemanagerv1alpha1.ProjectSuspendedReason,
					ObservedGeneration: 1,
				})
			} else {
				conditions = append(conditions, metav1.Condition{
					Type:               resourcemanagerv1alpha1.ProjectSuspended,
					Status:             metav1.ConditionFalse,
					Reason:             resourcemanagerv1alpha1.ProjectSuspendedReason,
					ObservedGeneration: 1,
				})
			}

			project := &resourcemanagerv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{
					Name: consumerProject,
				},
				Status: resourcemanagerv1alpha1.ProjectStatus{
					Conditions: conditions,
				},
			}

			service := &servicesv1alpha1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "compute",
				},
				Spec: servicesv1alpha1.ServiceSpec{
					ServiceName: serviceName,
					Phase:       servicesv1alpha1.PhasePublished,
					Owner: servicesv1alpha1.ServiceOwner{
						ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{
							Name: providerProject,
						},
					},
				},
			}

			// Consumer cluster resources
			entitlement := &servicesv1alpha1.ServiceEntitlement{
				ObjectMeta: metav1.ObjectMeta{
					Name: "compute-entitlement",
				},
				Spec: servicesv1alpha1.ServiceEntitlementSpec{
					ServiceRef: servicesv1alpha1.ServiceRef{
						Name: "compute",
					},
				},
				Status: servicesv1alpha1.ServiceEntitlementStatus{
					Phase: servicesv1alpha1.EntitlementPhaseActive,
				},
			}

			// Provider cluster resources
			consumerName := serviceConsumerName(serviceName, consumerProject)
			consumer := &servicesv1alpha1.ServiceConsumer{
				ObjectMeta: metav1.ObjectMeta{
					Name: consumerName,
				},
				Spec: servicesv1alpha1.ServiceConsumerSpec{
					ServiceRef:         servicesv1alpha1.ServiceRef{Name: "compute"},
					ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: consumerProject},
					Suspended:          tt.initialStatus,
				},
			}

			rootClient := newFakeClient(project, service)
			consumerClient := newFakeClient(entitlement)
			providerClient := newFakeClient(consumer)

			mgr := newTestManager()
			mgr.add(consumerProject, consumerClient)
			mgr.add(providerProject, providerClient)

			r := &ProjectSuspensionPropagationReconciler{
				client:  rootClient,
				Scheme:  testScheme(),
				Manager: mgr,
			}

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: consumerProject},
			})
			if err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}

			var gotConsumer servicesv1alpha1.ServiceConsumer
			err = providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &gotConsumer)
			if err != nil {
				t.Fatalf("Failed to fetch ServiceConsumer: %v", err)
			}

			if gotConsumer.Spec.Suspended != tt.wantSuspended {
				t.Errorf("got Suspended = %v, want %v", gotConsumer.Spec.Suspended, tt.wantSuspended)
			}
		})
	}
}

func TestServiceEntitlementProjectSuspension(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
		serviceSlug     = "compute"
	)

	tests := []struct {
		name             string
		projectSuspended bool
		wantSuspended    bool
	}{
		{
			name:             "creates consumer as suspended when project is suspended",
			projectSuspended: true,
			wantSuspended:    true,
		},
		{
			name:             "creates consumer as active when project is active",
			projectSuspended: false,
			wantSuspended:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. Setup Project with or without Suspended condition on root cluster
			conditions := []metav1.Condition{}
			conditions = append(conditions, metav1.Condition{
				Type: resourcemanagerv1alpha1.ProjectSuspended,
				Status: metav1.ConditionStatus(func() string {
					if tt.projectSuspended {
						return "True"
					}
					return "False"
				}()),
				Reason:             resourcemanagerv1alpha1.ProjectSuspendedReason,
				ObservedGeneration: 1,
			})

			project := &resourcemanagerv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{
					Name: consumerProject,
				},
				Status: resourcemanagerv1alpha1.ProjectStatus{
					Conditions: conditions,
				},
			}

			svc := newPublishedService(serviceSlug, serviceName, providerProject, "")
			ent := newEntitlement(serviceSlug, serviceSlug)

			rootClient := newFakeClient(svc, project)
			consumerClient := newFakeClient(ent)
			providerClient := newFakeClient()

			mgr := newTestManager()
			mgr.add(consumerProject, consumerClient)
			mgr.add(providerProject, providerClient)

			r := &ServiceEntitlementReconciler{
				rootClient: rootClient,
				Manager:    mgr,
				Scheme:     testScheme(),
			}

			// Reconcile the entitlement
			reconcileUntilStable(t, r, entitlementRequest(consumerProject, serviceSlug), 5)

			// Get the created ServiceConsumer
			consumerName := serviceConsumerName(serviceName, consumerProject)
			var consumer servicesv1alpha1.ServiceConsumer
			if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &consumer); err != nil {
				t.Fatalf("failed to get ServiceConsumer: %v", err)
			}

			if consumer.Spec.Suspended != tt.wantSuspended {
				t.Errorf("got Suspended = %v, want %v", consumer.Spec.Suspended, tt.wantSuspended)
			}
		})
	}
}

func TestProjectSuspensionPropagationAndEntitlementLifecycle(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
		serviceSlug     = "compute"
	)

	// 1. Initial State: Project is ACTIVE (unsuspended)
	conditions := []metav1.Condition{
		{
			Type:               resourcemanagerv1alpha1.ProjectSuspended,
			Status:             metav1.ConditionFalse,
			Reason:             resourcemanagerv1alpha1.ProjectSuspendedReason,
			ObservedGeneration: 1,
		},
	}
	project := &resourcemanagerv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: consumerProject},
		Status:     resourcemanagerv1alpha1.ProjectStatus{Conditions: conditions},
	}

	svc := newPublishedService(serviceSlug, serviceName, providerProject, "")
	ent := newEntitlement(serviceSlug, serviceSlug)

	rootClient := newFakeClient(svc, project)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)
	mgr.add(providerProject, providerClient)

	// Initialize controllers
	entReconciler := &ServiceEntitlementReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}
	propReconciler := &ProjectSuspensionPropagationReconciler{
		client:  rootClient,
		Manager: mgr,
		Scheme:  testScheme(),
	}

	// Step A: Reconcile entitlement -> should create ServiceConsumer with Suspended=false
	reconcileUntilStable(t, entReconciler, entitlementRequest(consumerProject, serviceSlug), 5)

	consumerName := serviceConsumerName(serviceName, consumerProject)
	var consumer servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &consumer); err != nil {
		t.Fatalf("failed to get ServiceConsumer: %v", err)
	}
	if consumer.Spec.Suspended {
		t.Fatalf("expected ServiceConsumer to be unsuspended initially")
	}

	// Step B: Project gets SUSPENDED
	project.Status.Conditions[0].Status = metav1.ConditionTrue
	if err := rootClient.Status().Update(context.Background(), project); err != nil {
		t.Fatalf("failed to update project status: %v", err)
	}

	// Run propagation reconciler
	_, err := propReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: consumerProject},
	})
	if err != nil {
		t.Fatalf("propagation reconcile failed: %v", err)
	}

	// Assert: ServiceConsumer is now Suspended=true
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &consumer); err != nil {
		t.Fatalf("failed to get ServiceConsumer: %v", err)
	}
	if !consumer.Spec.Suspended {
		t.Fatalf("expected ServiceConsumer to be suspended after project suspension")
	}

	// Step C: Project gets REINSTATED (active)
	project.Status.Conditions[0].Status = metav1.ConditionFalse
	if err := rootClient.Status().Update(context.Background(), project); err != nil {
		t.Fatalf("failed to update project status: %v", err)
	}

	// Run propagation reconciler again
	_, err = propReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: consumerProject},
	})
	if err != nil {
		t.Fatalf("propagation reconcile failed: %v", err)
	}

	// Assert: ServiceConsumer is now Suspended=false
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &consumer); err != nil {
		t.Fatalf("failed to get ServiceConsumer: %v", err)
	}
	if consumer.Spec.Suspended {
		t.Fatalf("expected ServiceConsumer to be unsuspended after project reinstatement")
	}
}

// --- Hardened edge-case coverage for ProjectSuspensionPropagationReconciler ---

func newSuspensionCondition(status metav1.ConditionStatus) []metav1.Condition {
	return []metav1.Condition{{
		Type:               resourcemanagerv1alpha1.ProjectSuspended,
		Status:             status,
		Reason:             resourcemanagerv1alpha1.ProjectSuspendedReason,
		ObservedGeneration: 1,
	}}
}

func newProjectFixture(name string, conditions []metav1.Condition) *resourcemanagerv1alpha1.Project {
	return &resourcemanagerv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     resourcemanagerv1alpha1.ProjectStatus{Conditions: conditions},
	}
}

func newServiceConsumerFixture(name, serviceRef, consumerProject string, suspended bool) *servicesv1alpha1.ServiceConsumer {
	return &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: serviceRef},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: consumerProject},
			Suspended:          suspended,
		},
	}
}

func newEntitlementWithPhase(name, serviceRef string, phase servicesv1alpha1.EntitlementPhase) *servicesv1alpha1.ServiceEntitlement {
	ent := newEntitlement(name, serviceRef)
	ent.Status.Phase = phase
	return ent
}

func projectSuspensionRequest(project string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: project}}
}

// TestProjectSuspensionPropagation_ProjectNotFound covers deletion races: the
// Project watch can fire after the object is already gone (e.g. deleted
// between event and reconcile). The reconciler must treat this as a no-op,
// not an error.
func TestProjectSuspensionPropagation_ProjectNotFound(t *testing.T) {
	rootClient := newFakeClient()
	mgr := newTestManager()

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	res, err := r.Reconcile(context.Background(), projectSuspensionRequest("does-not-exist"))
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected empty result, got %+v", res)
	}
}

// TestProjectSuspensionPropagation_ConsumerClusterNotEngaged covers a project
// that exists but whose virtual control plane hasn't been engaged yet by the
// multicluster manager (e.g. still starting up). Must not error.
func TestProjectSuspensionPropagation_ConsumerClusterNotEngaged(t *testing.T) {
	const consumerProject = "consumer-proj"

	project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
	rootClient := newFakeClient(project)
	mgr := newTestManager() // no clusters engaged

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
}

// TestProjectSuspensionPropagation_SkipsEntitlementsWithoutStatusPhase ensures
// an entitlement that hasn't been reconciled yet (status.phase is still the
// zero value, so no ServiceConsumer has ever been projected for it) is
// skipped rather than causing a spurious Service/consumer lookup.
func TestProjectSuspensionPropagation_SkipsEntitlementsWithoutStatusPhase(t *testing.T) {
	const consumerProject = "consumer-proj"

	project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
	// Deliberately no backing Service — if the reconciler tried to resolve it
	// for this entitlement, it would surface as an unexpected error.
	unreconciled := newEntitlement("compute", "compute")

	rootClient := newFakeClient(project)
	consumerClient := newFakeClient(unreconciled)
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var consumers servicesv1alpha1.ServiceConsumerList
	if err := providerClient.List(context.Background(), &consumers); err != nil {
		t.Fatalf("failed to list ServiceConsumers: %v", err)
	}
	if len(consumers.Items) != 0 {
		t.Errorf("expected no ServiceConsumer activity for an unreconciled entitlement, got %d", len(consumers.Items))
	}
}

// TestProjectSuspensionPropagation_PropagatesAcrossEntitlementPhases is a
// regression test: a ServiceConsumer is upserted for every entitlement phase
// (Active, PendingApproval, Rejected), so suspension must propagate to all of
// them, not just Active ones.
func TestProjectSuspensionPropagation_PropagatesAcrossEntitlementPhases(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	phases := []servicesv1alpha1.EntitlementPhase{
		servicesv1alpha1.EntitlementPhaseActive,
		servicesv1alpha1.EntitlementPhasePendingApproval,
		servicesv1alpha1.EntitlementPhaseRejected,
	}

	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
			svc := newPublishedService("compute", serviceName, providerProject, "")
			ent := newEntitlementWithPhase("compute", "compute", phase)
			consumerName := serviceConsumerName(serviceName, consumerProject)
			consumer := newServiceConsumerFixture(consumerName, "compute", consumerProject, false)

			rootClient := newFakeClient(project, svc)
			consumerClient := newFakeClient(ent)
			providerClient := newFakeClient(consumer)

			mgr := newTestManager()
			mgr.add(consumerProject, consumerClient)
			mgr.add(providerProject, providerClient)

			r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

			if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}

			var got servicesv1alpha1.ServiceConsumer
			if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got); err != nil {
				t.Fatalf("failed to get ServiceConsumer: %v", err)
			}
			if !got.Spec.Suspended {
				t.Errorf("phase %q: expected ServiceConsumer to be suspended, got Suspended=false", phase)
			}
		})
	}
}

// TestProjectSuspensionPropagation_ResolvesServiceByCanonicalName is a
// regression test: entitlements may reference a Service by its canonical
// spec.serviceName rather than its Kubernetes object name. The propagation
// reconciler must resolve that the same way the entitlement controller does,
// not with a raw Get-by-object-name that would 404.
func TestProjectSuspensionPropagation_ResolvesServiceByCanonicalName(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
	// Object name deliberately differs from the canonical serviceName.
	svc := newPublishedService("compute-object", serviceName, providerProject, "")
	// The entitlement references the service by its canonical name.
	ent := newEntitlementWithPhase("compute-entitlement", serviceName, servicesv1alpha1.EntitlementPhaseActive)
	consumerName := serviceConsumerName(serviceName, consumerProject)
	consumer := newServiceConsumerFixture(consumerName, serviceName, consumerProject, false)

	rootClient := newFakeClient(project, svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient(consumer)

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)
	mgr.add(providerProject, providerClient)

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got); err != nil {
		t.Fatalf("failed to get ServiceConsumer: %v", err)
	}
	if !got.Spec.Suspended {
		t.Errorf("expected ServiceConsumer resolved via canonical service name to be suspended")
	}
}

// TestProjectSuspensionPropagation_ServiceNotFoundSkipsGracefully ensures one
// entitlement referencing a deleted/nonexistent Service doesn't abort the
// whole reconcile — sibling entitlements in the same project must still be
// processed.
func TestProjectSuspensionPropagation_ServiceNotFoundSkipsGracefully(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
	svc := newPublishedService("compute", serviceName, providerProject, "")

	orphaned := newEntitlementWithPhase("orphaned", "storage-deleted", servicesv1alpha1.EntitlementPhaseActive)
	valid := newEntitlementWithPhase("compute-entitlement", "compute", servicesv1alpha1.EntitlementPhaseActive)

	consumerName := serviceConsumerName(serviceName, consumerProject)
	consumer := newServiceConsumerFixture(consumerName, "compute", consumerProject, false)

	rootClient := newFakeClient(project, svc)
	consumerClient := newFakeClient(orphaned, valid)
	providerClient := newFakeClient(consumer)

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)
	mgr.add(providerProject, providerClient)

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got); err != nil {
		t.Fatalf("failed to get ServiceConsumer: %v", err)
	}
	if !got.Spec.Suspended {
		t.Errorf("expected the valid entitlement's ServiceConsumer to still be suspended despite a sibling orphaned entitlement")
	}
}

// TestProjectSuspensionPropagation_DraftServiceSkipped ensures suspension is
// not propagated for a Service that's still in Draft — mirrors the
// entitlement controller, which never activates a consumer for an unpublished
// service.
func TestProjectSuspensionPropagation_DraftServiceSkipped(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
	svc := newPublishedService("compute", serviceName, providerProject, "")
	svc.Spec.Phase = servicesv1alpha1.PhaseDraft

	ent := newEntitlementWithPhase("compute-entitlement", "compute", servicesv1alpha1.EntitlementPhaseRejected)
	consumerName := serviceConsumerName(serviceName, consumerProject)
	consumer := newServiceConsumerFixture(consumerName, "compute", consumerProject, false)

	rootClient := newFakeClient(project, svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient(consumer)

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)
	mgr.add(providerProject, providerClient)

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got); err != nil {
		t.Fatalf("failed to get ServiceConsumer: %v", err)
	}
	if got.Spec.Suspended {
		t.Errorf("expected ServiceConsumer for a Draft service to be left untouched (Suspended=false)")
	}
}

// TestProjectSuspensionPropagation_ProviderClusterNotEngagedSkipsGracefully
// ensures a provider project that hasn't been engaged yet doesn't abort
// processing of sibling entitlements whose provider IS engaged.
func TestProjectSuspensionPropagation_ProviderClusterNotEngagedSkipsGracefully(t *testing.T) {
	const (
		consumerProject      = "consumer-proj"
		engagedProvider      = "provider-engaged"
		notEngagedProvider   = "provider-not-engaged"
		engagedServiceName   = "compute.miloapis.com"
		unengagedServiceName = "storage.miloapis.com"
	)

	project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
	engagedSvc := newPublishedService("compute", engagedServiceName, engagedProvider, "")
	unengagedSvc := newPublishedService("storage", unengagedServiceName, notEngagedProvider, "")

	entEngaged := newEntitlementWithPhase("compute-entitlement", "compute", servicesv1alpha1.EntitlementPhaseActive)
	entUnengaged := newEntitlementWithPhase("storage-entitlement", "storage", servicesv1alpha1.EntitlementPhaseActive)

	engagedConsumerName := serviceConsumerName(engagedServiceName, consumerProject)
	engagedConsumer := newServiceConsumerFixture(engagedConsumerName, "compute", consumerProject, false)

	rootClient := newFakeClient(project, engagedSvc, unengagedSvc)
	consumerClient := newFakeClient(entEngaged, entUnengaged)
	providerClient := newFakeClient(engagedConsumer)

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)
	mgr.add(engagedProvider, providerClient)
	// notEngagedProvider is intentionally never added to mgr.

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: engagedConsumerName}, &got); err != nil {
		t.Fatalf("failed to get ServiceConsumer: %v", err)
	}
	if !got.Spec.Suspended {
		t.Errorf("expected the engaged provider's ServiceConsumer to be suspended despite a sibling unengaged provider")
	}
}

// TestProjectSuspensionPropagation_ConsumerNotFoundSkipsGracefully covers a
// ServiceConsumer that no longer exists in the provider project (e.g. deleted
// out-of-band) — must be skipped, not treated as an error.
func TestProjectSuspensionPropagation_ConsumerNotFoundSkipsGracefully(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
	svc := newPublishedService("compute", serviceName, providerProject, "")
	ent := newEntitlementWithPhase("compute-entitlement", "compute", servicesv1alpha1.EntitlementPhaseActive)

	rootClient := newFakeClient(project, svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient() // no ServiceConsumer present

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)
	mgr.add(providerProject, providerClient)

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
}

// TestProjectSuspensionPropagation_NoOpWhenAlreadyMatching ensures the
// reconciler doesn't issue a needless Patch (and resourceVersion churn) when
// the ServiceConsumer's Suspended flag already reflects the Project's state.
func TestProjectSuspensionPropagation_NoOpWhenAlreadyMatching(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	project := newProjectFixture(consumerProject, newSuspensionCondition(metav1.ConditionTrue))
	svc := newPublishedService("compute", serviceName, providerProject, "")
	ent := newEntitlementWithPhase("compute-entitlement", "compute", servicesv1alpha1.EntitlementPhaseActive)
	consumerName := serviceConsumerName(serviceName, consumerProject)
	consumer := newServiceConsumerFixture(consumerName, "compute", consumerProject, true) // already suspended

	rootClient := newFakeClient(project, svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient(consumer)

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)
	mgr.add(providerProject, providerClient)

	var before servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &before); err != nil {
		t.Fatalf("failed to get ServiceConsumer before reconcile: %v", err)
	}

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var after servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &after); err != nil {
		t.Fatalf("failed to get ServiceConsumer after reconcile: %v", err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("expected no write when Suspended already matched: resourceVersion changed from %q to %q",
			before.ResourceVersion, after.ResourceVersion)
	}
}

// TestProjectSuspensionPropagation_NoConditionDefaultsToUnsuspended covers a
// Project whose ProjectSuspended condition has been removed entirely (as
// opposed to explicitly False) — the reconciler must still treat that as "not
// suspended" and reinstate any previously-suspended consumer.
func TestProjectSuspensionPropagation_NoConditionDefaultsToUnsuspended(t *testing.T) {
	const (
		consumerProject = "consumer-proj"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	project := newProjectFixture(consumerProject, nil) // no conditions at all
	svc := newPublishedService("compute", serviceName, providerProject, "")
	ent := newEntitlementWithPhase("compute-entitlement", "compute", servicesv1alpha1.EntitlementPhaseActive)
	consumerName := serviceConsumerName(serviceName, consumerProject)
	consumer := newServiceConsumerFixture(consumerName, "compute", consumerProject, true) // previously suspended

	rootClient := newFakeClient(project, svc)
	consumerClient := newFakeClient(ent)
	providerClient := newFakeClient(consumer)

	mgr := newTestManager()
	mgr.add(consumerProject, consumerClient)
	mgr.add(providerProject, providerClient)

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(consumerProject)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var got servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerName}, &got); err != nil {
		t.Fatalf("failed to get ServiceConsumer: %v", err)
	}
	if got.Spec.Suspended {
		t.Errorf("expected a missing ProjectSuspended condition to reinstate the consumer, got Suspended=true")
	}
}

// TestProjectSuspensionPropagation_IsolatesPerProject ensures reconciling one
// Project never touches ServiceConsumers belonging to a different project's
// entitlements.
func TestProjectSuspensionPropagation_IsolatesPerProject(t *testing.T) {
	const (
		projectA        = "consumer-proj-a"
		projectB        = "consumer-proj-b"
		providerProject = "provider-proj"
		serviceName     = "compute.miloapis.com"
	)

	suspendedProjectA := newProjectFixture(projectA, newSuspensionCondition(metav1.ConditionTrue))
	unsuspendedProjectB := newProjectFixture(projectB, newSuspensionCondition(metav1.ConditionFalse))
	svc := newPublishedService("compute", serviceName, providerProject, "")

	entA := newEntitlementWithPhase("compute-entitlement", "compute", servicesv1alpha1.EntitlementPhaseActive)
	entB := newEntitlementWithPhase("compute-entitlement", "compute", servicesv1alpha1.EntitlementPhaseActive)

	consumerNameA := serviceConsumerName(serviceName, projectA)
	consumerNameB := serviceConsumerName(serviceName, projectB)
	consumerA := newServiceConsumerFixture(consumerNameA, "compute", projectA, false)
	consumerB := newServiceConsumerFixture(consumerNameB, "compute", projectB, false)

	rootClient := newFakeClient(suspendedProjectA, unsuspendedProjectB, svc)
	consumerClientA := newFakeClient(entA)
	consumerClientB := newFakeClient(entB)
	providerClient := newFakeClient(consumerA, consumerB)

	mgr := newTestManager()
	mgr.add(projectA, consumerClientA)
	mgr.add(projectB, consumerClientB)
	mgr.add(providerProject, providerClient)

	r := &ProjectSuspensionPropagationReconciler{client: rootClient, Manager: mgr}

	// Only reconcile project A.
	if _, err := r.Reconcile(context.Background(), projectSuspensionRequest(projectA)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	var gotA, gotB servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerNameA}, &gotA); err != nil {
		t.Fatalf("failed to get ServiceConsumer A: %v", err)
	}
	if err := providerClient.Get(context.Background(), types.NamespacedName{Name: consumerNameB}, &gotB); err != nil {
		t.Fatalf("failed to get ServiceConsumer B: %v", err)
	}
	if !gotA.Spec.Suspended {
		t.Errorf("expected project A's ServiceConsumer to be suspended")
	}
	if gotB.Spec.Suspended {
		t.Errorf("expected project B's ServiceConsumer to be untouched by project A's reconcile, got Suspended=true")
	}
}
