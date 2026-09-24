// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestDecisionEventReason(t *testing.T) {
	const (
		pending = servicesv1alpha1.EntitlementPhasePendingApproval
		active  = servicesv1alpha1.EntitlementPhaseActive
		reject  = servicesv1alpha1.EntitlementPhaseRejected
	)
	tests := []struct {
		name         string
		announced    servicesv1alpha1.EntitlementPhase
		hasAnnounced bool
		next         servicesv1alpha1.EntitlementPhase
		want         string
	}{
		{name: "approval", announced: pending, hasAnnounced: true, next: active, want: EventReasonEntitlementApproved},
		{name: "denial", announced: pending, hasAnnounced: true, next: reject, want: EventReasonEntitlementRejected},
		{name: "reversal to approved", announced: reject, hasAnnounced: true, next: active, want: EventReasonEntitlementApproved},
		{name: "revocation", announced: active, hasAnnounced: true, next: reject, want: EventReasonEntitlementRejected},
		{name: "already announced active", announced: active, hasAnnounced: true, next: active},
		{name: "already announced rejected", announced: reject, hasAnnounced: true, next: reject},
		{name: "back to pending", announced: active, hasAnnounced: true, next: pending},
		{name: "new request has no baseline", next: pending},
		{name: "decided before events existed", next: active},
		{name: "rejected before events existed", next: reject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decisionEventReason(string(tt.announced), tt.hasAnnounced, tt.next)
			if got != tt.want || ok != (tt.want != "") {
				t.Errorf("decisionEventReason(%q, %v, %q) = %q, %v; want %q, %v",
					tt.announced, tt.hasAnnounced, tt.next, got, ok, tt.want, tt.want != "")
			}
		})
	}
}

// decisionFixture drives a gated entitlement to PendingApproval so a test can
// record a provider decision and observe the events that follow.
type decisionFixture struct {
	r              *ServiceEntitlementReconciler
	consumerClient client.Client
	providerClient client.Client
}

func newDecisionFixture(t *testing.T, mode servicesv1alpha1.EnablementMode, consumerClient client.Client) *decisionFixture {
	t.Helper()
	svc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, mode)
	svc.Spec.DisplayName = "Compute"
	providerClient := newFakeClient()

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, providerClient)

	f := &decisionFixture{
		r: &ServiceEntitlementReconciler{
			rootClient: newFakeClient(svc),
			Manager:    mgr,
			Scheme:     testScheme(),
		},
		consumerClient: consumerClient,
		providerClient: providerClient,
	}
	f.reconcile(t)
	return f
}

func (f *decisionFixture) reconcile(t *testing.T) {
	t.Helper()
	reconcileUntilStable(t, f.r, entitlementRequest(testConsumerProject, testServiceSlug), 5)
}

func (f *decisionFixture) decide(t *testing.T, decision servicesv1alpha1.ApprovalDecision, message string) {
	t.Helper()
	ctx := context.Background()
	var sc servicesv1alpha1.ServiceConsumer
	key := types.NamespacedName{Name: serviceConsumerName(testServiceName, testConsumerProject)}
	if err := f.providerClient.Get(ctx, key, &sc); err != nil {
		t.Fatalf("get serviceconsumer: %v", err)
	}
	sc.Spec.Approval = &servicesv1alpha1.ProviderApproval{Decision: decision, Message: message}
	if err := f.providerClient.Update(ctx, &sc); err != nil {
		t.Fatalf("record decision: %v", err)
	}
}

// propagate runs the ServiceConsumer reconciler, the path that normally
// carries a provider's decision onto the entitlement.
func (f *decisionFixture) propagate(t *testing.T) {
	t.Helper()
	r := &ServiceConsumerReconciler{Manager: f.r.Manager, Scheme: testScheme()}
	req := entitlementRequest(testProviderProject, serviceConsumerName(testServiceName, testConsumerProject))
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("ServiceConsumer reconcile: %v", err)
	}
}

func (f *decisionFixture) entitlement(t *testing.T) *servicesv1alpha1.ServiceEntitlement {
	t.Helper()
	var got servicesv1alpha1.ServiceEntitlement
	if err := f.consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	return &got
}

func (f *decisionFixture) events(t *testing.T) []eventsv1.Event {
	t.Helper()
	var list eventsv1.EventList
	if err := f.consumerClient.List(context.Background(), &list); err != nil {
		t.Fatalf("list events: %v", err)
	}
	return list.Items
}

func TestServiceEntitlementReconciler_EmitsApprovedEventOnce(t *testing.T) {
	f := newDecisionFixture(t, servicesv1alpha1.EnablementModeGatedByProvider,
		newFakeClient(newEntitlement(testServiceSlug, testServiceSlug)))
	if got := f.events(t); len(got) != 0 {
		t.Fatalf("pending request emitted %d events, want 0", len(got))
	}

	f.decide(t, servicesv1alpha1.ApprovalDecisionApproved, "")
	f.reconcile(t)
	f.reconcile(t)

	got := f.events(t)
	if len(got) != 1 {
		t.Fatalf("approval emitted %d events, want exactly 1", len(got))
	}
	evt := got[0]
	if evt.Reason != EventReasonEntitlementApproved || evt.Type != corev1.EventTypeNormal {
		t.Errorf("event reason/type = %q/%q, want %q/Normal", evt.Reason, evt.Type, EventReasonEntitlementApproved)
	}
	if evt.Namespace != entitlementEventNamespace {
		t.Errorf("event namespace = %q, want %q", evt.Namespace, entitlementEventNamespace)
	}
	if evt.Regarding.Kind != "ServiceEntitlement" || evt.Regarding.Name != testServiceSlug ||
		evt.Regarding.APIVersion != servicesv1alpha1.GroupVersion.String() {
		t.Errorf("event regarding = %+v, want the %q ServiceEntitlement", evt.Regarding, testServiceSlug)
	}
	if evt.Annotations[eventAnnotationServiceDisplayName] != "Compute" ||
		evt.Annotations[eventAnnotationServiceName] != testServiceName {
		t.Errorf("event annotations = %v, want display name and canonical service name", evt.Annotations)
	}
	if _, ok := evt.Annotations[eventAnnotationDecisionMessage]; ok {
		t.Errorf("event carries a decision message annotation though the provider gave none")
	}
	if evt.ReportingController == "" || evt.ReportingInstance == "" {
		t.Errorf("event reportingController/Instance must be set for events.k8s.io/v1")
	}
}

func TestServiceEntitlementReconciler_EmitsRejectedEventWithMessage(t *testing.T) {
	f := newDecisionFixture(t, servicesv1alpha1.EnablementModeGatedByProvider,
		newFakeClient(newEntitlement(testServiceSlug, testServiceSlug)))

	f.decide(t, servicesv1alpha1.ApprovalDecisionDenied, "Compute is limited to verified organizations.")
	f.reconcile(t)
	f.reconcile(t)

	got := f.events(t)
	if len(got) != 1 {
		t.Fatalf("denial emitted %d events, want exactly 1", len(got))
	}
	evt := got[0]
	if evt.Reason != EventReasonEntitlementRejected || evt.Type != corev1.EventTypeWarning {
		t.Errorf("event reason/type = %q/%q, want %q/Warning", evt.Reason, evt.Type, EventReasonEntitlementRejected)
	}
	if msg := evt.Annotations[eventAnnotationDecisionMessage]; msg != "Compute is limited to verified organizations." {
		t.Errorf("decision message annotation = %q", msg)
	}
	if !strings.Contains(evt.Note, "Compute is limited to verified organizations.") {
		t.Errorf("event note %q omits the provider's message", evt.Note)
	}
}

// The ServiceConsumer reconciler usually writes the decided phase before the
// entitlement reconciler runs, so that reconcile reads a phase that already
// changed. The event must still be emitted, once.
func TestServiceEntitlementReconciler_EmitsEventWhenConsumerControllerPropagates(t *testing.T) {
	f := newDecisionFixture(t, servicesv1alpha1.EnablementModeGatedByProvider,
		newFakeClient(newEntitlement(testServiceSlug, testServiceSlug)))

	f.decide(t, servicesv1alpha1.ApprovalDecisionApproved, "")
	f.propagate(t)
	if got := f.entitlement(t).Status.Phase; got != servicesv1alpha1.EntitlementPhaseActive {
		t.Fatalf("after propagation entitlement phase = %q, want Active", got)
	}
	if got := f.events(t); len(got) != 0 {
		t.Fatalf("the ServiceConsumer reconciler emitted %d events, want 0 (emission belongs to the entitlement reconciler)", len(got))
	}

	f.reconcile(t)
	f.reconcile(t)

	got := f.events(t)
	if len(got) != 1 || got[0].Reason != EventReasonEntitlementApproved {
		t.Fatalf("events after propagation = %d (%v), want one %q", len(got), got, EventReasonEntitlementApproved)
	}
}

// Entitlements decided before this controller emitted events carry no
// announced phase. Their first reconcile after rollout records it without
// emitting, so a rollout doesn't flood every project's feed with old news.
func TestServiceEntitlementReconciler_BaselinesDecisionsMadeBeforeRollout(t *testing.T) {
	f := newDecisionFixture(t, servicesv1alpha1.EnablementModeGatedByProvider,
		newFakeClient(newEntitlement(testServiceSlug, testServiceSlug)))
	f.decide(t, servicesv1alpha1.ApprovalDecisionApproved, "")
	f.reconcile(t)

	// Simulate the pre-rollout state: decided, with no announced phase and no
	// event on record.
	ctx := context.Background()
	ent := f.entitlement(t)
	delete(ent.Annotations, decisionAnnouncedAnnotation)
	if err := f.consumerClient.Update(ctx, ent); err != nil {
		t.Fatalf("strip announced annotation: %v", err)
	}
	for _, evt := range f.events(t) {
		if err := f.consumerClient.Delete(ctx, &evt); err != nil {
			t.Fatalf("delete event: %v", err)
		}
	}

	f.reconcile(t)

	if got := f.events(t); len(got) != 0 {
		t.Errorf("baseline reconcile emitted %d events, want 0", len(got))
	}
	if got := f.entitlement(t).Annotations[decisionAnnouncedAnnotation]; got != string(servicesv1alpha1.EntitlementPhaseActive) {
		t.Errorf("announced annotation = %q, want Active recorded as the baseline", got)
	}
}

func TestServiceEntitlementReconciler_SelfServiceEmitsNoDecisionEvent(t *testing.T) {
	f := newDecisionFixture(t, "", newFakeClient(newEntitlement(testServiceSlug, testServiceSlug)))
	if got := f.events(t); len(got) != 0 {
		t.Errorf("self-service activation emitted %d events, want 0", len(got))
	}
	if _, ok := f.entitlement(t).Annotations[decisionAnnouncedAnnotation]; ok {
		t.Errorf("self-service entitlement carries %s, want none", decisionAnnouncedAnnotation)
	}
}

func TestServiceEntitlementReconciler_EventFailureDoesNotFailReconcile(t *testing.T) {
	consumerClient := &failingEventsClient{Client: newFakeClient(newEntitlement(testServiceSlug, testServiceSlug))}
	f := newDecisionFixture(t, servicesv1alpha1.EnablementModeGatedByProvider, consumerClient)

	f.decide(t, servicesv1alpha1.ApprovalDecisionApproved, "")
	f.reconcile(t)

	var got servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &got); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	if got.Status.Phase != servicesv1alpha1.EntitlementPhaseActive {
		t.Errorf("entitlement phase = %q, want Active despite the event failure", got.Status.Phase)
	}
}

// failingEventsClient rejects every Event create and passes everything else
// through.
type failingEventsClient struct {
	client.Client
}

func (c *failingEventsClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*eventsv1.Event); ok {
		return errors.New("events unavailable")
	}
	return c.Client.Create(ctx, obj, opts...)
}
