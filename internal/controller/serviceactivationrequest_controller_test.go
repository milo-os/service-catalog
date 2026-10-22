// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func newActivationRequest(name, _ string, consumerProject string, authorized bool) *servicesv1alpha1.ServiceActivationRequest {
	annotations := map[string]string{}
	if authorized {
		annotations[servicesv1alpha1.ServiceActivationAuthorizedAnnotation] = servicesv1alpha1.ServiceActivationAuthorizedValueTrue
	}
	return &servicesv1alpha1.ServiceActivationRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			UID:               types.UID("request-uid"),
			Annotations:       annotations,
			CreationTimestamp: metav1.NewTime(time.Unix(1700000000, 0)),
		},
		Spec: servicesv1alpha1.ServiceActivationRequestSpec{
			ServiceRef:         servicesv1alpha1.CanonicalServiceReference{Name: testServiceName},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: consumerProject},
			RequestMessage:     "managed onboarding",
			RequestedBy: servicesv1alpha1.ActorReference{
				Username: "provider@example.com",
				UID:      types.UID("actor-uid"),
			},
		},
	}
}

func TestServiceActivationRequestReconcilerDoesNotResolveMetadataName(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	activation := newActivationRequest("enable-compute", testProviderProject, testConsumerProject, true)
	activation.Spec.ServiceRef.Name = svc.Name
	r := &ServiceActivationRequestReconciler{rootClient: newServiceRootClient(svc)}

	_, err := r.resolveService(context.Background(), activation.Spec.ServiceRef.Name)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("resolveService(metadata.name) error = %v, want NotFound", err)
	}
}

func activationRequest(cluster, name string) mcreconcile.Request {
	return mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: name}},
		ClusterName: multicluster.ClusterName(cluster),
	}
}

func TestServiceActivationRequestReconcilerCreatesEntitlementWithProvenance(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	activation := newActivationRequest("enable-compute", testProviderProject, testConsumerProject, true)
	rootClient := newServiceRootClient(svc)
	providerClient := newFakeClient(activation)
	consumerClient := newFakeClient()
	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)
	r := &ServiceActivationRequestReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}

	res, err := r.Reconcile(context.Background(), activationRequest(testProviderProject, activation.Name))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want event-driven reconciliation", res.RequeueAfter)
	}

	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), client.ObjectKey{Name: testServiceSlug}, &entitlement); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if entitlement.Spec.ProviderActivation == nil {
		t.Fatal("providerActivation provenance was not set")
	}
	if got := entitlement.Spec.ServiceRef.Name; got != svc.Name {
		t.Errorf("entitlement service ref = %q, want resolved Service resource name %q", got, svc.Name)
	}
	if got := entitlement.Spec.ProviderActivation.RequestRef.Name; got != activation.Name {
		t.Errorf("request ref name = %q, want %q", got, activation.Name)
	}
	if got := entitlement.Spec.ProviderActivation.ProviderProjectRef.Name; got != testProviderProject {
		t.Errorf("provider project = %q, want %q", got, testProviderProject)
	}
	if got := entitlement.Spec.ProviderActivation.Actor.Username; got != activation.Spec.RequestedBy.Username {
		t.Errorf("actor = %q, want %q", got, activation.Spec.RequestedBy.Username)
	}
	if entitlement.Spec.RequestMessage != activation.Spec.RequestMessage {
		t.Errorf("request message = %q, want %q", entitlement.Spec.RequestMessage, activation.Spec.RequestMessage)
	}

	var got servicesv1alpha1.ServiceActivationRequest
	if err := providerClient.Get(context.Background(), client.ObjectKey{Name: activation.Name}, &got); err != nil {
		t.Fatalf("get activation request: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.ServiceActivationRequestPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	if got.Status.EntitlementRef == nil || got.Status.EntitlementRef.Name != entitlement.Name {
		t.Fatalf("entitlementRef = %#v, want name %q", got.Status.EntitlementRef, entitlement.Name)
	}
}

func TestServiceActivationRequestReconcilerDoesNotFailOnProjectCacheMiss(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	activation := newActivationRequest("enable-compute", testProviderProject, testConsumerProject, true)
	cacheClient := newFakeClient(svc) // Deliberately missing the live Project.
	apiReader := newServiceRootClient()
	providerClient := newFakeClient(activation)
	consumerClient := newFakeClient()
	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)
	r := &ServiceActivationRequestReconciler{
		rootClient: cacheClient, rootAPIReader: apiReader, Manager: mgr, Scheme: testScheme(),
	}

	if _, err := r.Reconcile(context.Background(), activationRequest(testProviderProject, activation.Name)); err != nil {
		t.Fatal(err)
	}
	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), client.ObjectKey{Name: testServiceSlug}, &entitlement); err != nil {
		t.Fatalf("cache miss dropped activation instead of creating entitlement: %v", err)
	}
	var got servicesv1alpha1.ServiceActivationRequest
	if err := providerClient.Get(context.Background(), client.ObjectKey{Name: activation.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == servicesv1alpha1.ServiceActivationRequestPhaseFailed ||
		got.Status.Phase == servicesv1alpha1.ServiceActivationRequestPhaseDisabled {
		t.Fatalf("cache miss produced terminal activation status: %#v", got.Status)
	}
}

func TestServiceActivationRequestReconcilerRejectsUnavailableConsumerProject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		root   func(*servicesv1alpha1.Service) client.Client
	}{
		{
			name:   "missing",
			reason: "ConsumerProjectNotFound",
			root:   func(svc *servicesv1alpha1.Service) client.Client { return newFakeClient(svc) },
		},
		{
			name:   "terminating",
			reason: "ConsumerProjectTerminating",
			root: func(svc *servicesv1alpha1.Service) client.Client {
				now := metav1.Now()
				return newServiceRootClient(svc, &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{
					Name: testConsumerProject, DeletionTimestamp: &now, Finalizers: []string{"test"},
				}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
			activation := newActivationRequest("enable-compute", testProviderProject, testConsumerProject, true)
			providerClient := newFakeClient(activation)
			consumerClient := newFakeClient()
			mgr := newTestManager()
			mgr.add(testProviderProject, providerClient)
			mgr.add(testConsumerProject, consumerClient)
			r := &ServiceActivationRequestReconciler{rootClient: tc.root(svc), Manager: mgr, Scheme: testScheme()}

			result, err := r.Reconcile(context.Background(), activationRequest(testProviderProject, activation.Name))
			if err != nil || result != (ctrl.Result{}) {
				t.Fatalf("reconcile result=%+v err=%v", result, err)
			}
			var entitlement servicesv1alpha1.ServiceEntitlement
			if err := consumerClient.Get(context.Background(), client.ObjectKey{Name: testServiceSlug}, &entitlement); !apierrors.IsNotFound(err) {
				t.Fatalf("unavailable project received entitlement: err=%v object=%#v", err, entitlement)
			}
			var got servicesv1alpha1.ServiceActivationRequest
			if err := providerClient.Get(context.Background(), client.ObjectKey{Name: activation.Name}, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != servicesv1alpha1.ServiceActivationRequestPhaseFailed || got.Status.EntitlementRef != nil {
				t.Fatalf("status = %#v, want Failed with no entitlement reference", got.Status)
			}
			for _, conditionType := range []string{conditionTypeEntitlementCreated, conditionTypeActivationReady} {
				condition := apimeta.FindStatusCondition(got.Status.Conditions, conditionType)
				if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != tc.reason {
					t.Errorf("condition %s = %#v, want False/%s", conditionType, condition, tc.reason)
				}
			}
		})
	}
}

func TestServiceActivationRequestReconcilerDisablesExistingRequestForUnavailableProject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		root   func(*servicesv1alpha1.Service) client.Client
	}{
		{
			name:   "missing",
			reason: "ConsumerProjectNotFound",
			root:   func(svc *servicesv1alpha1.Service) client.Client { return newFakeClient(svc) },
		},
		{
			name:   "terminating",
			reason: "ConsumerProjectTerminating",
			root: func(svc *servicesv1alpha1.Service) client.Client {
				now := metav1.Now()
				return newServiceRootClient(svc, &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{
					Name: testConsumerProject, DeletionTimestamp: &now, Finalizers: []string{"test"},
				}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
			activation := newActivationRequest("enable-compute", testProviderProject, testConsumerProject, true)
			activation.Status.Phase = servicesv1alpha1.ServiceActivationRequestPhaseActive
			activation.Status.EntitlementRef = &servicesv1alpha1.ServiceEntitlementReference{
				Name: testServiceSlug,
				UID:  types.UID("entitlement-uid"),
			}
			setActivationCondition(activation, conditionTypeEntitlementCreated, metav1.ConditionTrue,
				"EntitlementCreated", "The consumer-side service entitlement exists.")
			setActivationCondition(activation, conditionTypeActivationReady, metav1.ConditionTrue,
				"EntitlementActive", "The service is active in the consumer project.")

			providerClient := newFakeClient(activation)
			mgr := newTestManager()
			mgr.add(testProviderProject, providerClient)
			wakeups := newActivationRequestWakeups()
			req := activationRequest(testProviderProject, activation.Name)
			consumerClusterName := multicluster.ClusterName(testConsumerProject)
			wakeups.waitFor(consumerClusterName, req)
			r := &ServiceActivationRequestReconciler{
				rootClient: tc.root(svc),
				Manager:    mgr,
				Scheme:     testScheme(),
				wakeups:    wakeups,
			}

			result, err := r.Reconcile(context.Background(), req)
			if err != nil || result != (ctrl.Result{}) {
				t.Fatalf("reconcile result=%+v err=%v", result, err)
			}
			var got servicesv1alpha1.ServiceActivationRequest
			if err := providerClient.Get(context.Background(), client.ObjectKey{Name: activation.Name}, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != servicesv1alpha1.ServiceActivationRequestPhaseDisabled {
				t.Fatalf("phase = %q, want Disabled", got.Status.Phase)
			}
			if got.Status.EntitlementRef == nil || got.Status.EntitlementRef.Name != testServiceSlug ||
				got.Status.EntitlementRef.UID != types.UID("entitlement-uid") {
				t.Fatalf("entitlementRef = %#v, want original reference retained", got.Status.EntitlementRef)
			}
			ready := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeActivationReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != tc.reason {
				t.Fatalf("Ready = %#v, want False/%s", ready, tc.reason)
			}

			wakeups.mu.Lock()
			_, stillWaiting := wakeups.waiting[consumerClusterName]
			wakeups.mu.Unlock()
			if stillWaiting {
				t.Fatal("terminal activation request remained registered for consumer engagement")
			}
		})
	}
}

func TestServiceActivationRequestReconcilerWaitsForConsumerEngagement(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	activation := newActivationRequest("enable-compute", testProviderProject, testConsumerProject, true)
	providerClient := newFakeClient(activation)
	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	wakeups := newActivationRequestWakeups()
	r := &ServiceActivationRequestReconciler{
		rootClient: newServiceRootClient(svc),
		Manager:    mgr,
		Scheme:     testScheme(),
		wakeups:    wakeups,
	}
	req := activationRequest(testProviderProject, activation.Name)

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want engagement-driven reconciliation", res.RequeueAfter)
	}

	mgr.add(testConsumerProject, newFakeClient())
	if err := wakeups.Engage(context.Background(), multicluster.ClusterName(testConsumerProject), nil); err != nil {
		t.Fatalf("Engage: %v", err)
	}
	select {
	case event := <-wakeups.events:
		if event.Object != req {
			t.Fatalf("wake-up request = %#v, want %#v", event.Object, req)
		}
	case <-time.After(time.Second):
		t.Fatal("consumer engagement did not wake the activation request")
	}
}

func TestMapServiceEntitlementToActivationRequest(t *testing.T) {
	entitlement := newEntitlement(testServiceSlug, testServiceSlug)
	entitlement.Spec.ProviderActivation = &servicesv1alpha1.ProviderActivation{
		RequestRef:         servicesv1alpha1.ServiceActivationRequestReference{Name: "enable-compute"},
		ProviderProjectRef: servicesv1alpha1.ProducerProjectReference{Name: testProviderProject},
	}

	got := mapServiceEntitlementToActivationRequest(context.Background(), entitlement)
	want := activationRequest(testProviderProject, "enable-compute")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("mapped requests = %#v, want %#v", got, []mcreconcile.Request{want})
	}

	entitlement.Spec.ProviderActivation = nil
	if got := mapServiceEntitlementToActivationRequest(context.Background(), entitlement); len(got) != 0 {
		t.Fatalf("unlinked entitlement mapped to %#v, want no requests", got)
	}
}

func TestServiceActivationRequestReconcilerDeniedWithoutAuthorization(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	activation := newActivationRequest("enable-compute", testProviderProject, testConsumerProject, false)
	rootClient := newServiceRootClient(svc)
	providerClient := newFakeClient(activation)
	consumerClient := newFakeClient()
	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)
	r := &ServiceActivationRequestReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}

	if _, err := r.Reconcile(context.Background(), activationRequest(testProviderProject, activation.Name)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got servicesv1alpha1.ServiceActivationRequest
	if err := providerClient.Get(context.Background(), client.ObjectKey{Name: activation.Name}, &got); err != nil {
		t.Fatalf("get activation request: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.ServiceActivationRequestPhaseDenied {
		t.Errorf("phase = %q, want Denied", got.Status.Phase)
	}
	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), client.ObjectKey{Name: testServiceSlug}, &entitlement); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no entitlement, got err=%v object=%#v", err, entitlement)
	}
}

func TestServiceActivationRequestReconcilerDoesNotRecreateConsumerDeletedEntitlement(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "")
	activation := newActivationRequest("enable-compute", testProviderProject, testConsumerProject, true)
	rootClient := newServiceRootClient(svc)
	providerClient := newFakeClient(activation)
	consumerClient := newFakeClient()
	mgr := newTestManager()
	mgr.add(testProviderProject, providerClient)
	mgr.add(testConsumerProject, consumerClient)
	r := &ServiceActivationRequestReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}
	req := activationRequest(testProviderProject, activation.Name)

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("initial Reconcile: %v", err)
	}
	var entitlement servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), client.ObjectKey{Name: testServiceSlug}, &entitlement); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if err := consumerClient.Delete(context.Background(), &entitlement); err != nil {
		t.Fatalf("delete entitlement: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile after deletion: %v", err)
	}

	var got servicesv1alpha1.ServiceActivationRequest
	if err := providerClient.Get(context.Background(), client.ObjectKey{Name: activation.Name}, &got); err != nil {
		t.Fatalf("get activation request: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.ServiceActivationRequestPhaseDisabled {
		t.Errorf("phase = %q, want Disabled", got.Status.Phase)
	}
	if err := consumerClient.Get(context.Background(), client.ObjectKey{Name: testServiceSlug}, &entitlement); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted entitlement was recreated: err=%v", err)
	}
}

func TestServiceEntitlementReconcilerProviderActivationImplicitlyApprovesGatedService(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, servicesv1alpha1.EnablementModeGatedByProvider)
	entitlement := newEntitlement(testServiceSlug, testServiceSlug)
	entitlement.Spec.ProviderActivation = &servicesv1alpha1.ProviderActivation{
		RequestRef:         servicesv1alpha1.ServiceActivationRequestReference{Name: "enable-compute", UID: types.UID("request-uid")},
		ProviderProjectRef: servicesv1alpha1.ProducerProjectReference{Name: testProviderProject},
		Actor:              servicesv1alpha1.ActorReference{Username: "provider@example.com"},
		RequestedAt:        metav1.Now(),
	}
	rootClient := newServiceRootClient(svc)
	consumerClient := newFakeClient(entitlement)
	providerClient := newFakeClient()
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)
	r := &ServiceEntitlementReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, entitlement.Name), 5)
	var serviceConsumer servicesv1alpha1.ServiceConsumer
	name := serviceConsumerName(testServiceName, testConsumerProject)
	if err := providerClient.Get(context.Background(), client.ObjectKey{Name: name}, &serviceConsumer); err != nil {
		t.Fatalf("get ServiceConsumer: %v", err)
	}
	if serviceConsumer.Spec.Approval == nil || serviceConsumer.Spec.Approval.Decision != servicesv1alpha1.ApprovalDecisionApproved {
		t.Fatalf("approval = %#v, want Approved", serviceConsumer.Spec.Approval)
	}
	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), client.ObjectKey{Name: entitlement.Name}, &got); err != nil {
		t.Fatalf("get ServiceEntitlement: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("phase = %q, want Active", got.Status.Phase)
	}
}

func TestServiceEntitlementReconcilerProviderActivationDoesNotOverwriteDecision(t *testing.T) {
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, servicesv1alpha1.EnablementModeGatedByProvider)
	entitlement := newEntitlement(testServiceSlug, testServiceSlug)
	entitlement.Spec.ProviderActivation = &servicesv1alpha1.ProviderActivation{
		RequestRef:         servicesv1alpha1.ServiceActivationRequestReference{Name: "enable-compute", UID: types.UID("request-uid")},
		ProviderProjectRef: servicesv1alpha1.ProducerProjectReference{Name: testProviderProject},
		Actor:              servicesv1alpha1.ActorReference{Username: "provider@example.com"},
		RequestedAt:        metav1.Now(),
	}
	consumerName := serviceConsumerName(testServiceName, testConsumerProject)
	existingConsumer := &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: consumerName},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         entitlement.Spec.ServiceRef,
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: testConsumerProject},
			Approval: &servicesv1alpha1.ProviderApproval{
				Decision: servicesv1alpha1.ApprovalDecisionDenied,
				Message:  "capacity unavailable",
			},
		},
	}
	rootClient := newServiceRootClient(svc)
	consumerClient := newFakeClient(entitlement)
	providerClient := newFakeClient(existingConsumer)
	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)
	r := &ServiceEntitlementReconciler{rootClient: rootClient, Manager: mgr, Scheme: testScheme()}

	reconcileUntilStable(t, r, entitlementRequest(testConsumerProject, entitlement.Name), 5)
	var got servicesv1alpha1.ServiceConsumer
	if err := providerClient.Get(context.Background(), client.ObjectKey{Name: consumerName}, &got); err != nil {
		t.Fatalf("get ServiceConsumer: %v", err)
	}
	if got.Spec.Approval == nil || got.Spec.Approval.Decision != servicesv1alpha1.ApprovalDecisionDenied || got.Spec.Approval.Message != "capacity unavailable" {
		t.Fatalf("existing provider decision was overwritten: %#v", got.Spec.Approval)
	}
}
