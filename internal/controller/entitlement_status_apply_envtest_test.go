// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"sync"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs run against a real API server, whose server-side apply merge
// and ownership rules are what keep concurrent status writers from erasing
// each other.
var _ = Describe("ServiceEntitlement status ownership", func() {
	newEntitlement := func(name string) *servicesv1alpha1.ServiceEntitlement {
		e := &servicesv1alpha1.ServiceEntitlement{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}},
		}
		Expect(k8sClient.Create(ctx, e)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, e) })
		return e
	}
	get := func(name string) *servicesv1alpha1.ServiceEntitlement {
		var e servicesv1alpha1.ServiceEntitlement
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &e)).To(Succeed())
		return &e
	}

	It("keeps both conditions when two controllers write from the same stale copy", func() {
		newEntitlement("envtest-ssa-concurrent")
		stale := get("envtest-ssa-concurrent")

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			e := stale.DeepCopy()
			errs[0] = patchEntitlementSuspendedCondition(ctx, k8sClient, e, projectSuspension{Suspended: true})
		}()
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			e := stale.DeepCopy()
			errs[1] = applyEntitlementReadyStatus(ctx, k8sClient, e,
				servicesv1alpha1.EntitlementPhaseActive, "Enabled", "Enabled.", "compute.miloapis.com")
		}()
		wg.Wait()
		Expect(errs).To(HaveEach(BeNil()))

		got := get("envtest-ssa-concurrent")
		Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeSuspended)).To(BeTrue())
		Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeReady)).To(BeTrue())
		Expect(got.Status.Phase).To(Equal(servicesv1alpha1.EntitlementPhaseActive))
	})

	It("takes over status last written by Update and can still retire a condition", func() {
		newEntitlement("envtest-ssa-migrate")

		// Before server-side apply, every writer used Update or a merge patch.
		legacy := get("envtest-ssa-migrate")
		legacy.Status.Phase = servicesv1alpha1.EntitlementPhaseActive
		for _, typ := range []string{ConditionTypeReady, ConditionTypeSuspended, servicesv1alpha1.ConditionTypeDependenciesSatisfied, "SomethingElse"} {
			apimeta.SetStatusCondition(&legacy.Status.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: "Legacy"})
		}
		Expect(k8sClient.Status().Update(ctx, legacy)).To(Succeed())

		// The dependencies writer retires its condition.
		cached := get("envtest-ssa-migrate")
		e := cached.DeepCopy()
		apimeta.RemoveStatusCondition(&e.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied)
		Expect(applyEntitlementStatus(ctx, k8sClient, dependenciesConditionOwner, cached, e)).To(Succeed())

		got := get("envtest-ssa-migrate")
		Expect(apimeta.FindStatusCondition(got.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied)).To(BeNil())
		for _, typ := range []string{ConditionTypeReady, ConditionTypeSuspended, "SomethingElse"} {
			Expect(apimeta.FindStatusCondition(got.Status.Conditions, typ)).NotTo(BeNil(), typ)
		}
		Expect(got.Status.Phase).To(Equal(servicesv1alpha1.EntitlementPhaseActive))

		// Each moved field now belongs to its apply manager, and the Update
		// manager keeps only what nothing here owns.
		legacyEntries := 0
		for _, entry := range got.ManagedFields {
			if entry.Operation != metav1.ManagedFieldsOperationUpdate || entry.Subresource != "status" {
				continue
			}
			legacyEntries++
			Expect(string(entry.FieldsV1.Raw)).NotTo(ContainSubstring(`\"type\":\"Ready\"`))
			Expect(string(entry.FieldsV1.Raw)).NotTo(ContainSubstring(`\"type\":\"Suspended\"`))
			Expect(string(entry.FieldsV1.Raw)).NotTo(ContainSubstring(`f:phase`))
			Expect(string(entry.FieldsV1.Raw)).To(ContainSubstring(`\"type\":\"SomethingElse\"`))
		}
		Expect(legacyEntries).To(Equal(1))

		// Suspension can now lift its condition too, without touching Ready.
		cached = got
		e = cached.DeepCopy()
		apimeta.RemoveStatusCondition(&e.Status.Conditions, ConditionTypeSuspended)
		Expect(applyEntitlementStatus(ctx, k8sClient, suspendedConditionOwner, cached, e)).To(Succeed())
		got = get("envtest-ssa-migrate")
		Expect(apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeSuspended)).To(BeNil())
		Expect(apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)).NotTo(BeNil())
	})

	It("keeps a condition's transition time when nothing about it changed", func() {
		newEntitlement("envtest-ssa-transition")
		e := get("envtest-ssa-transition")
		Expect(patchEntitlementSuspendedCondition(ctx, k8sClient, e, projectSuspension{Suspended: true})).To(Succeed())
		first := apimeta.FindStatusCondition(get("envtest-ssa-transition").Status.Conditions, ConditionTypeSuspended).LastTransitionTime

		e = get("envtest-ssa-transition")
		rv := e.ResourceVersion
		Expect(patchEntitlementSuspendedCondition(ctx, k8sClient, e, projectSuspension{Suspended: true})).To(Succeed())
		got := get("envtest-ssa-transition")
		Expect(got.ResourceVersion).To(Equal(rv), "an unchanged condition should not be written")
		Expect(apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeSuspended).LastTransitionTime).To(Equal(first))
	})
})
