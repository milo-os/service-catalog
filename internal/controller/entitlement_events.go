// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// Event reasons written to a consumer project when a provider decides a
// GatedByProvider request. The project's activity feed matches on these, so
// they are part of the ActivityPolicy contract and must not be renamed.
const (
	EventReasonEntitlementApproved = "EntitlementApproved"
	EventReasonEntitlementRejected = "EntitlementRejected"
)

// Annotations carried on entitlement decision events so the ActivityPolicy can
// render a summary without looking anything up.
const (
	eventAnnotationServiceName        = "services.miloapis.com/service-name"
	eventAnnotationServiceDisplayName = "services.miloapis.com/service-display-name"
	eventAnnotationDecisionMessage    = "services.miloapis.com/decision-message"
)

// entitlementEventNamespace is where decision events are written.
// ServiceEntitlement is cluster-scoped but events.k8s.io Events are
// namespaced, and every Milo project has a "default" namespace.
const entitlementEventNamespace = "default"

const entitlementEventReportingController = "services.miloapis.com/services-controller"

// decisionAnnouncedAnnotation records the last phase of a GatedByProvider
// entitlement that its decision events account for. Two controllers write the
// phase: this reconciler, and the ServiceConsumer reconciler that propagates a
// provider's decision. Comparing against a durable record, instead of the
// phase the reconcile happened to read, finds a decision whichever of them
// wrote it, and emits it once.
const decisionAnnouncedAnnotation = "services.miloapis.com/announced-decision"

// decisionEventReason reports which decision event, if any, moving from the
// announced phase to the next one warrants. With no announced phase there is
// no baseline, so nothing is emitted: that is a new request, or an entitlement
// decided before this controller emitted events, whose decision was never news
// to this feed.
func decisionEventReason(announced string, hasAnnounced bool, next servicesv1alpha1.EntitlementPhase) (string, bool) {
	if !hasAnnounced || announced == string(next) {
		return "", false
	}
	switch next {
	case servicesv1alpha1.EntitlementPhaseActive:
		return EventReasonEntitlementApproved, true
	case servicesv1alpha1.EntitlementPhaseRejected:
		return EventReasonEntitlementRejected, true
	}
	return "", false
}

// announceDecision emits a decision event when a GatedByProvider entitlement's
// phase has moved past the one last announced, and records the new phase.
//
// The record is written before the event, so a failure between the two drops
// the event rather than emitting it twice.
func announceDecision(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	svc *servicesv1alpha1.Service,
	approval *servicesv1alpha1.ProviderApproval,
) error {
	phase := entitlement.Status.Phase
	if phase == "" {
		return nil
	}
	announced, hasAnnounced := entitlement.Annotations[decisionAnnouncedAnnotation]
	if hasAnnounced && announced == string(phase) {
		return nil
	}

	before := entitlement.DeepCopy()
	if entitlement.Annotations == nil {
		entitlement.Annotations = map[string]string{}
	}
	entitlement.Annotations[decisionAnnouncedAnnotation] = string(phase)
	if err := consumerClient.Patch(ctx, entitlement, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("failed to record announced decision on ServiceEntitlement: %w", err)
	}

	if reason, ok := decisionEventReason(announced, hasAnnounced, phase); ok {
		emitDecisionEvent(ctx, consumerClient, entitlement, svc, approval, reason)
	}
	return nil
}

// emitDecisionEvent records a provider's decision on the entitlement in the
// consumer project, where the activity system picks it up. Emission is best
// effort: the announced phase is already recorded, so a failure is logged
// rather than retried.
func emitDecisionEvent(
	ctx context.Context,
	consumerClient client.Client,
	entitlement *servicesv1alpha1.ServiceEntitlement,
	svc *servicesv1alpha1.Service,
	approval *servicesv1alpha1.ProviderApproval,
	reason string,
) {
	displayName := svc.Spec.DisplayName
	if displayName == "" {
		displayName = entitlement.Name
	}
	annotations := map[string]string{
		eventAnnotationServiceName:        svc.Spec.ServiceName,
		eventAnnotationServiceDisplayName: displayName,
	}

	note := fmt.Sprintf("The service provider approved access to %s.", displayName)
	eventType := corev1.EventTypeNormal
	if reason == EventReasonEntitlementRejected {
		note = fmt.Sprintf("The service provider rejected access to %s.", displayName)
		eventType = corev1.EventTypeWarning
	}
	if approval != nil && approval.Message != "" {
		annotations[eventAnnotationDecisionMessage] = approval.Message
		note = fmt.Sprintf("%s %s", note, approval.Message)
	}

	now := time.Now()
	evt := &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s.%x", entitlement.Name, now.UnixNano()),
			Namespace:   entitlementEventNamespace,
			Annotations: annotations,
		},
		EventTime:           metav1.NewMicroTime(now),
		Action:              reason,
		Reason:              reason,
		Note:                strings.ToValidUTF8(truncateMessage(note, 1024), ""),
		Type:                eventType,
		ReportingController: entitlementEventReportingController,
		ReportingInstance:   reportingInstance(),
		Regarding: corev1.ObjectReference{
			APIVersion:      servicesv1alpha1.GroupVersion.String(),
			Kind:            "ServiceEntitlement",
			Name:            entitlement.Name,
			UID:             entitlement.UID,
			ResourceVersion: entitlement.ResourceVersion,
		},
	}
	if err := consumerClient.Create(ctx, evt); err != nil {
		log.FromContext(ctx).Error(err, "failed to emit entitlement decision event",
			"reason", reason, "entitlement", entitlement.Name)
	}
}

// reportingInstance identifies the reporting pod. events.k8s.io/v1 requires
// a value of 1 to 128 characters.
func reportingInstance() string {
	name := os.Getenv("POD_NAME")
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = "services-controller"
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}
