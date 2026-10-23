package activation

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func requesterWith(ec EntitlementClient, io IOStreams) Requester {
	return Requester{Service: testConfig, Client: ec, IO: io, Project: "datum-cloud"}
}

func TestRequestSubmitPendingExitsZero(t *testing.T) {
	pending := entitlement("compute", servicesv1alpha1.EntitlementPhasePendingApproval, servicesv1alpha1.ReasonEntitlementPendingApproval, "Waiting for the service provider to approve this request.", nil)
	ec, cs := newFake(withWatch(modifiedEvent(pending)))
	io, _, errb := testIO("", false)

	// A submitted-but-pending request via the explicit verb is success (exit 0).
	if err := requesterWith(ec, io).Request(context.Background(), RequestOptions{}); err != nil {
		t.Fatalf("Request() = %v, want nil (exit 0)", err)
	}
	if got := countVerb(cs, "create"); got != 1 {
		t.Fatalf("expected 1 create, got %d", got)
	}
	if !strings.Contains(errb.String(), "has been submitted") {
		t.Fatalf("missing submitted copy:\n%s", errb.String())
	}
}

func TestRequestDeniedWithoutRenewIsTerminal(t *testing.T) {
	denied := entitlement("compute", servicesv1alpha1.EntitlementPhaseRejected, servicesv1alpha1.ReasonEntitlementRejected, "The service provider denied this request.", nil)
	ec, cs := newFake(withObjects(denied))
	io, _, _ := testIO("", false)

	err := requesterWith(ec, io).Request(context.Background(), RequestOptions{})
	wantExit(t, err, ExitDeniedOrRevoked, StateDenied)
	if got := countVerb(cs, "delete"); got != 0 {
		t.Fatalf("denied without --renew must not delete")
	}
}

func TestRequestRenewDeletesAndRecreates(t *testing.T) {
	denied := entitlement("compute", servicesv1alpha1.EntitlementPhaseRejected, servicesv1alpha1.ReasonEntitlementRejected, "The service provider denied this request.", nil)
	pending := entitlement("compute", servicesv1alpha1.EntitlementPhasePendingApproval, servicesv1alpha1.ReasonEntitlementPendingApproval, "Waiting for the service provider to approve this request.", nil)
	ec, cs := newFake(withObjects(denied), withWatch(modifiedEvent(pending)))
	io, _, _ := testIO("", false) // non-interactive: no renew confirmation prompt

	if err := requesterWith(ec, io).Request(context.Background(), RequestOptions{Renew: true}); err != nil {
		t.Fatalf("Request(--renew) = %v, want nil (exit 0)", err)
	}
	if got := countVerb(cs, "delete"); got != 1 {
		t.Fatalf("renew should delete the rejected entitlement once, got %d", got)
	}
	if got := countVerb(cs, "create"); got != 1 {
		t.Fatalf("renew should recreate once, got %d", got)
	}
}

func TestRequestWaitTimesOutWhilePending(t *testing.T) {
	pending := entitlement("compute", servicesv1alpha1.EntitlementPhasePendingApproval, servicesv1alpha1.ReasonEntitlementPendingApproval, "Waiting for the service provider to approve this request.", nil)
	// The entitlement stays pending and the watch never delivers a resolving
	// event, so a short --timeout expires while pending → exit 12.
	ec, _ := newFake(withObjects(pending))
	io, _, errb := testIO("", false)

	err := requesterWith(ec, io).Request(context.Background(), RequestOptions{Wait: true, Timeout: 20 * time.Millisecond})
	wantExit(t, err, ExitPending, StatePendingApproval)
	if !strings.Contains(errb.String(), "Setup is still waiting on authorization") {
		t.Fatalf("missing timeout copy:\n%s", errb.String())
	}
}

func TestRequestWaitReachesActive(t *testing.T) {
	pending := entitlement("compute", servicesv1alpha1.EntitlementPhasePendingApproval, servicesv1alpha1.ReasonEntitlementPendingApproval, "Waiting for the service provider to approve this request.", nil)
	active := entitlement("compute", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "This service is enabled and ready to use.", ptrNow())
	// Starts pending; the watch then delivers Active.
	ec, _ := newFake(withObjects(pending), withWatch(modifiedEvent(active)))
	io, _, errb := testIO("", false)

	if err := requesterWith(ec, io).Request(context.Background(), RequestOptions{Wait: true, Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("Request(--wait) = %v, want nil once Active", err)
	}
	if !strings.Contains(errb.String(), "setup is complete") {
		t.Fatalf("missing activation copy:\n%s", errb.String())
	}
}

func TestRequestWaitDoesNotStopAtAuthorization(t *testing.T) {
	pending := entitlement("compute", servicesv1alpha1.EntitlementPhasePendingApproval, servicesv1alpha1.ReasonEntitlementPendingApproval, "Waiting for approval.", nil)
	active := entitlement("compute", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "Access granted.", ptrNow())
	for i := range active.Status.Conditions {
		if active.Status.Conditions[i].Type == servicesv1alpha1.ConditionTypeSetupComplete {
			active.Status.Conditions[i].Status = metav1.ConditionFalse
			active.Status.Conditions[i].Reason = "ProjectionPending"
		}
	}
	active.Status.Setup = &servicesv1alpha1.ServiceSetupStatus{
		RequestedAt: metav1.Now(),
		Stages: []metav1.Condition{
			{Type: "Authorization", Status: metav1.ConditionTrue},
			{Type: "Dependencies", Status: metav1.ConditionTrue},
			{Type: "Provisioning", Status: metav1.ConditionTrue},
			{Type: "Projection", Status: metav1.ConditionFalse},
		},
	}
	ec, _ := newFake(withObjects(pending), withWatch(modifiedEvent(active)))
	io, _, errb := testIO("", false)

	err := requesterWith(ec, io).Request(context.Background(), RequestOptions{Wait: true, Timeout: 20 * time.Millisecond})
	wantExit(t, err, ExitPending, StateActive)
	if !strings.Contains(errb.String(), "waiting on Projection") {
		t.Fatalf("timeout did not report setup blocker:\n%s", errb.String())
	}
}

func TestRequestAlreadyActiveStands(t *testing.T) {
	active := entitlement("compute", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "This service is enabled and ready to use.", ptrNow())
	ec, cs := newFake(withObjects(active))
	io, _, errb := testIO("", false)

	if err := requesterWith(ec, io).Request(context.Background(), RequestOptions{}); err != nil {
		t.Fatalf("Request() on an active service = %v, want nil", err)
	}
	if got := countVerb(cs, "create"); got != 0 {
		t.Fatalf("request on an active service must not create")
	}
	if !strings.Contains(errb.String(), "Active") {
		t.Fatalf("missing active status:\n%s", errb.String())
	}
}

// TestRequestWaitRereadsWhenTheServerClosesTheWatch covers API servers that
// end a watch after its first event: --wait must re-read and finish promptly
// rather than sit out the minute-long re-read interval.
func TestRequestWaitRereadsWhenTheServerClosesTheWatch(t *testing.T) {
	pending := entitlement("compute", servicesv1alpha1.EntitlementPhasePendingApproval, servicesv1alpha1.ReasonEntitlementPendingApproval, "Waiting for approval.", nil)
	active := entitlement("compute", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "Access granted.", ptrNow())
	cs := buildFakeClientset(withObjects(pending))
	watches := 0
	cs.PrependWatchReactor(entitlementResource, func(k8stesting.Action) (bool, watch.Interface, error) {
		watches++
		fw := watch.NewFakeWithChanSize(1, false)
		fw.Modify(pending.DeepCopy())
		fw.Stop()
		if watches == 1 {
			// The entitlement completes while no watch is open.
			if _, err := cs.Tracker().Get(entitlementGVR, "", "compute"); err == nil {
				_ = cs.Tracker().Update(entitlementGVR, active.DeepCopy(), "")
			}
		}
		return true, fw, nil
	})
	io, _, _ := testIO("", false)

	start := time.Now()
	if err := requesterWith(NewClient(cs), io).Request(context.Background(), RequestOptions{Wait: true, Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("Request(--wait) = %v, want nil once Active", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("--wait took %s after the watch closed, want a prompt re-read", elapsed)
	}
}
