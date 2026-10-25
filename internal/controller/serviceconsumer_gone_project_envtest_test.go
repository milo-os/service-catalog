// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs run against a real API server, whose deletion and precondition
// rules decide what a delete of a ServiceConsumer leaves behind.
var _ = Describe("ServiceConsumer of a deleted Project", func() {
	const teardownFinalizer = "services.miloapis.com/provider-teardown"

	create := func(project string, finalizers ...string) *servicesv1alpha1.ServiceConsumer {
		sc := consumerOfProject(project)
		sc.Status = servicesv1alpha1.ServiceConsumerStatus{}
		sc.Finalizers = finalizers
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
		DeferCleanup(func() {
			var cur servicesv1alpha1.ServiceConsumer
			if k8sClient.Get(ctx, client.ObjectKeyFromObject(sc), &cur) == nil {
				cur.Finalizers = nil
				_ = k8sClient.Update(ctx, &cur)
				_ = k8sClient.Delete(ctx, &cur)
			}
		})
		return sc
	}
	reconcilerOn := func(c client.Client) *ServiceConsumerReconciler {
		mgr := newTestManager()
		mgr.add(testProviderProject, c)
		root := newRootClient()
		return &ServiceConsumerReconciler{Manager: mgr, Scheme: testScheme(), rootClient: root, rootAPIReader: root}
	}

	It("marks the consumer for deletion and keeps the provider-teardown hold", func() {
		sc := create("envtest-gone-hold", teardownFinalizer)

		_, err := reconcilerOn(k8sClient).Reconcile(ctx, entitlementRequest(testProviderProject, sc.Name))
		Expect(err).NotTo(HaveOccurred())

		var got servicesv1alpha1.ServiceConsumer
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sc), &got)).To(Succeed())
		Expect(got.DeletionTimestamp.IsZero()).To(BeFalse(), "the consumer of a deleted Project is not marked for deletion")
		Expect(controllerutil.ContainsFinalizer(&got, teardownFinalizer)).To(BeTrue(), "the provider-teardown hold was removed")
	})

	It("does not delete a consumer recreated under the same name", func() {
		sc := create("envtest-gone-recreated")
		var recreated *servicesv1alpha1.ServiceConsumer
		direct, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		// Recreate the consumer after the reconciler has read it and before its
		// delete reaches the API server.
		racing := interceptor.NewClient(direct, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
				recreated = consumerOfProject("envtest-gone-recreated")
				recreated.Status = servicesv1alpha1.ServiceConsumerStatus{}
				Expect(k8sClient.Create(ctx, recreated)).To(Succeed())
				return c.Delete(ctx, obj, opts...)
			},
		})

		_, err = reconcilerOn(racing).Reconcile(ctx, entitlementRequest(testProviderProject, sc.Name))
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "want the UID precondition to refuse the delete, got %v", err)

		var got servicesv1alpha1.ServiceConsumer
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sc), &got)).To(Succeed())
		Expect(got.UID).To(Equal(recreated.UID))
		Expect(got.DeletionTimestamp.IsZero()).To(BeTrue(), "the recreated consumer was deleted")
	})
})
