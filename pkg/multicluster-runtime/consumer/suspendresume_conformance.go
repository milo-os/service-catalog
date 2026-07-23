// SPDX-License-Identifier: AGPL-3.0-only

package consumer

import (
	"context"
	"testing"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ConformanceSuspendResume exercises the standardized suspend/resume contract
// documented on Suspend and Resume against a caller-supplied hook pair: it
// drives consumerName's ServiceConsumer through the platform's Suspended
// signal (True, then False) and asserts that
//
//  1. SuspendConsumer runs when the signal flips True, and the provider
//     reports back via the Paused condition (Status=True) WITHOUT altering
//     the platform's own Suspended condition;
//  2. ResumeConsumer runs when the signal flips False, and Paused reports
//     back False;
//  3. every object in retain — fetched fresh from consumerClient before and
//     after each transition — is unchanged, proving suspend/resume neither
//     mutates nor deletes the resources the operator projected into the
//     consumer project.
//
// providerClient must already contain consumerName's ServiceConsumer
// (Status.Phase = Active, no Suspended condition set); consumerClient must
// already contain every object in retain. Managed services implementing
// Suspend/Resume should call this from their own tests, seeded with their
// real hook implementations and real projected resources, instead of
// hand-rolling these assertions.
func ConformanceSuspendResume(
	t *testing.T,
	providerClient client.Client,
	consumerClient client.Client,
	consumerName, consumerProject string,
	serviceNames []string,
	suspend Suspend,
	resume Resume,
	retain ...client.Object,
) {
	t.Helper()
	ctx := context.Background()

	p := &Provider{
		opts: Options{
			ServiceNames: serviceNames,
			Suspends:     []Suspend{suspend},
			Resumes:      []Resume{resume},
		},
		log:                log.Log.WithName("conformance-suspend-resume"),
		providerClient:     providerClient,
		providerRestConfig: &rest.Config{Host: "https://conformance.invalid"},
		newClient: func(*rest.Config, client.Options) (client.Client, error) {
			return consumerClient, nil
		},
	}

	getConsumer := func() *servicesv1alpha1.ServiceConsumer {
		var sc servicesv1alpha1.ServiceConsumer
		if err := providerClient.Get(ctx, client.ObjectKey{Name: consumerName}, &sc); err != nil {
			t.Fatalf("conformance: get ServiceConsumer %q: %v", consumerName, err)
		}
		return &sc
	}

	setSignal := func(status metav1.ConditionStatus, reason string) {
		sc := getConsumer()
		before := sc.DeepCopy()
		apimeta.SetStatusCondition(&sc.Status.Conditions, metav1.Condition{
			Type:    servicesv1alpha1.ConditionTypeSuspended,
			Status:  status,
			Reason:  reason,
			Message: "conformance harness signal",
		})
		if err := providerClient.Status().Patch(ctx, sc, client.MergeFrom(before)); err != nil {
			t.Fatalf("conformance: set Suspended signal to %s/%s: %v", status, reason, err)
		}
	}

	snapshotRetained := func() []client.Object {
		snap := make([]client.Object, len(retain))
		for i, obj := range retain {
			fresh, ok := obj.DeepCopyObject().(client.Object)
			if !ok {
				t.Fatalf("conformance: retain[%d] does not implement client.Object", i)
			}
			if err := consumerClient.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
				t.Fatalf("conformance: get retained object %s/%s: %v", fresh.GetNamespace(), fresh.GetName(), err)
			}
			snap[i] = fresh
		}
		return snap
	}
	assertRetained := func(step string, before []client.Object) {
		after := snapshotRetained()
		for i := range retain {
			if !apiequality.Semantic.DeepEqual(before[i], after[i]) {
				t.Errorf("conformance: %s changed retained object %s/%s across the transition:\nbefore=%+v\nafter=%+v",
					step, before[i].GetNamespace(), before[i].GetName(), before[i], after[i])
			}
		}
	}

	runTransition := func(step string, signalStatus metav1.ConditionStatus, signalReason string) *servicesv1alpha1.ServiceConsumer {
		setSignal(signalStatus, signalReason)
		before := snapshotRetained()
		if err := p.reconcileSuspension(ctx, consumerProject, []servicesv1alpha1.ServiceConsumer{*getConsumer()}); err != nil {
			t.Fatalf("conformance: %s: reconcileSuspension: %v", step, err)
		}
		assertRetained(step, before)
		return getConsumer()
	}

	// 1. Suspend: signal flips True -> SuspendConsumer must run, Paused must
	// report True, and the platform's own signal must survive untouched.
	sc := runTransition("suspend", metav1.ConditionTrue, "ConformanceSuspended")
	if signal := apimeta.FindStatusCondition(sc.Status.Conditions, servicesv1alpha1.ConditionTypeSuspended); signal == nil ||
		signal.Status != metav1.ConditionTrue || signal.Reason != "ConformanceSuspended" {
		t.Errorf("conformance: suspend: platform's Suspended signal must be left untouched, got %+v", signal)
	}
	if !apimeta.IsStatusConditionTrue(sc.Status.Conditions, servicesv1alpha1.ConditionTypePaused) {
		t.Errorf("conformance: suspend: expected Paused=True after SuspendConsumer ran")
	}

	// 2. Reinstate: signal flips False -> ResumeConsumer must run, Paused
	// must report False, and the platform's own signal must again survive
	// untouched.
	sc = runTransition("reinstate", metav1.ConditionFalse, "ConformanceActive")
	if signal := apimeta.FindStatusCondition(sc.Status.Conditions, servicesv1alpha1.ConditionTypeSuspended); signal == nil ||
		signal.Status != metav1.ConditionFalse || signal.Reason != "ConformanceActive" {
		t.Errorf("conformance: reinstate: platform's Suspended signal must be left untouched, got %+v", signal)
	}
	if apimeta.IsStatusConditionTrue(sc.Status.Conditions, servicesv1alpha1.ConditionTypePaused) {
		t.Errorf("conformance: reinstate: expected Paused=False after ResumeConsumer ran")
	}
}
