// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs run against a real API server, whose server-side apply merges
// metadata.finalizers as a set and creates or updates in one call.
var _ = Describe("Server-side applied setup writes", func() {
	It("adds the entitlement finalizer from a stale copy without touching anything else", func() {
		e := &servicesv1alpha1.ServiceEntitlement{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-apply-finalizer", Finalizers: []string{"example.com/other"}},
			Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}},
		}
		Expect(k8sClient.Create(ctx, e)).To(Succeed())
		DeferCleanup(func() {
			var got servicesv1alpha1.ServiceEntitlement
			if k8sClient.Get(ctx, client.ObjectKeyFromObject(e), &got) == nil {
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, &got)
				_ = k8sClient.Delete(ctx, &got)
			}
		})
		stale := e.DeepCopy()

		// Another controller writes status first.
		Expect(applyEntitlementReadyStatus(ctx, k8sClient, e.DeepCopy(),
			servicesv1alpha1.EntitlementPhaseActive, "Enabled", "Enabled.", "compute.miloapis.com")).To(Succeed())

		Expect(applyFinalizer(ctx, k8sClient, stale, serviceEntitlementGVK, serviceEntitlementFinalizer)).To(Succeed())
		Expect(stale.Status.Phase).To(Equal(servicesv1alpha1.EntitlementPhaseActive), "obj should hold the server's latest copy")

		var got servicesv1alpha1.ServiceEntitlement
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(e), &got)).To(Succeed())
		Expect(got.Finalizers).To(ConsistOf("example.com/other", serviceEntitlementFinalizer))
		Expect(got.Spec.ServiceRef.Name).To(Equal("compute"))
		Expect(got.Status.Phase).To(Equal(servicesv1alpha1.EntitlementPhaseActive))
	})

	It("does not recreate an entitlement deleted since it was read", func() {
		gone := &servicesv1alpha1.ServiceEntitlement{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-apply-finalizer-gone", UID: types.UID("4c2a8f0e-0000-4000-8000-000000000000")},
		}
		Expect(applyFinalizer(ctx, k8sClient, gone, serviceEntitlementGVK, serviceEntitlementFinalizer)).NotTo(Succeed())
		var got servicesv1alpha1.ServiceEntitlement
		err := k8sClient.Get(ctx, client.ObjectKey{Name: gone.Name}, &got)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "apply recreated the entitlement: %v", err)
	})

	It("lets two passes that both missed the ServiceConsumer apply it at once", func() {
		const name = "envtest-apply-consumer"
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})
		entitlement := &servicesv1alpha1.ServiceEntitlement{
			Spec: servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}, RequestMessage: "please"},
		}
		r := &ServiceEntitlementReconciler{}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				_, errs[i] = r.applyServiceConsumer(ctx, k8sClient, name, "consumer-project", entitlement, false)
			}()
		}
		wg.Wait()
		Expect(errs).To(HaveEach(BeNil()))

		var got servicesv1alpha1.ServiceConsumer
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
		Expect(got.Spec.ServiceRef.Name).To(Equal("compute"))
		Expect(got.Spec.ConsumerProjectRef.Name).To(Equal("consumer-project"))
		Expect(got.Spec.RequestMessage).To(Equal("please"))
	})

	It("takes over a ServiceConsumer last written by Update", func() {
		const name = "envtest-apply-consumer-legacy"
		legacy := &servicesv1alpha1.ServiceConsumer{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: servicesv1alpha1.ServiceConsumerSpec{
				ServiceRef:         servicesv1alpha1.ServiceRef{Name: "compute"},
				ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: "consumer-project"},
				RequestMessage:     "old",
			},
		}
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })

		entitlement := &servicesv1alpha1.ServiceEntitlement{
			Spec: servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}, RequestMessage: "new"},
		}
		consumer, err := (&ServiceEntitlementReconciler{}).applyServiceConsumer(ctx, k8sClient, name, "consumer-project", entitlement, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(consumer.Spec.RequestMessage).To(Equal("new"))
	})

	It("records a provider-initiated approval once and never overwrites the provider's decision", func() {
		const name = "envtest-apply-consumer-approval"
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &servicesv1alpha1.ServiceConsumer{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})
		entitlement := &servicesv1alpha1.ServiceEntitlement{
			Spec: servicesv1alpha1.ServiceEntitlementSpec{
				ServiceRef:         servicesv1alpha1.ServiceRef{Name: "compute"},
				ProviderActivation: &servicesv1alpha1.ProviderActivation{},
			},
		}
		r := &ServiceEntitlementReconciler{}

		consumer, err := r.applyServiceConsumer(ctx, k8sClient, name, "consumer-project", entitlement, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(consumer.Spec.Approval).NotTo(BeNil())
		Expect(consumer.Spec.Approval.Decision).To(Equal(servicesv1alpha1.ApprovalDecisionApproved))

		// The provider later revokes. Later request applies keep its decision.
		consumer.Spec.Approval = &servicesv1alpha1.ProviderApproval{Decision: servicesv1alpha1.ApprovalDecisionDenied, Message: "No."}
		Expect(k8sClient.Update(ctx, consumer)).To(Succeed())
		consumer, err = r.applyServiceConsumer(ctx, k8sClient, name, "consumer-project", entitlement, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(consumer.Spec.Approval.Decision).To(Equal(servicesv1alpha1.ApprovalDecisionDenied))

		// A pass working from a copy that predates the decision can't overwrite it.
		approval := map[string]any{"approval": map[string]any{
			"decision": string(servicesv1alpha1.ApprovalDecisionApproved),
			"message":  "Approved because the service provider initiated activation.",
		}}
		err = applySpec(ctx, k8sClient, serviceConsumerGVK, name, approval, providerActivationApprovalManager, false, &servicesv1alpha1.ServiceConsumer{})
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "stale approval apply: %v", err)
		var got servicesv1alpha1.ServiceConsumer
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
		Expect(got.Spec.Approval.Decision).To(Equal(servicesv1alpha1.ApprovalDecisionDenied))
		Expect(controllerutil.ContainsFinalizer(&got, serviceEntitlementFinalizer)).To(BeFalse())
	})
})
