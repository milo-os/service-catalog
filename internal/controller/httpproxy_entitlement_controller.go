// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// HTTPProxy is namespaced in the consumer project control plane. Listing it
// is how we know the project is using ALB even when nobody created a
// ServiceEntitlement for networking.
var httpProxyGVK = schema.GroupVersionKind{
	Group:   "networking.datumapis.com",
	Version: "v1alpha",
	Kind:    "HTTPProxy",
}

const (
	// networkingServiceObjectName is Service.metadata.name for networking.
	// Admission resolves spec.serviceRef.name against that field, not the
	// canonical serviceName.
	networkingServiceObjectName = "networking-datumapis-com"

	// networkingServiceCanonicalName is Service.spec.serviceName.
	networkingServiceCanonicalName = "networking.datumapis.com"

	// httpProxyEnrolledLabel marks entitlements this reconciler created so a
	// later pass can tell them from a consumer admin's own Direct write.
	httpProxyEnrolledLabel = "services.miloapis.com/httpproxy-enrolled"
)

// HTTPProxyEntitlementReconciler enrolls a project in networking when it
// already has an HTTPProxy. ALB creation never required a ServiceEntitlement,
// so those projects never got Locations projected. Watching HTTPProxy fills
// the gap for new proxies and for every proxy already in a project cache.
//
// The entitlement is left in place if the last HTTPProxy is deleted. Tearing
// it down would drop IP classes and billing enrollment the project may still
// be using.
type HTTPProxyEntitlementReconciler struct {
	Manager mcmanager.Manager
	Scheme  *runtime.Scheme
}

// RegisterHTTPProxyScheme adds the foreign HTTPProxy GVK as unstructured so
// the project-cluster cache can watch it. Call this on the manager scheme
// before SetupWithManager. LocationBinding projections still set GVK on the
// object and do not go through scheme.New, so they are not confused with this
// mapping.
func RegisterHTTPProxyScheme(s *runtime.Scheme) {
	s.AddKnownTypeWithName(httpProxyGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(httpProxyGVK.GroupVersion().WithKind("HTTPProxyList"), &unstructured.UnstructuredList{})
	metav1.AddToGroupVersion(s, httpProxyGVK.GroupVersion())
}

// +kubebuilder:rbac:groups=networking.datumapis.com,resources=httpproxies,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch;create

func (r *HTTPProxyEntitlementReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	cl, err := r.Manager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get cluster %q: %w", req.ClusterName, err)
	}
	consumerClient := cl.GetClient()

	proxy := &unstructured.Unstructured{}
	proxy.SetGroupVersionKind(httpProxyGVK)
	if err := consumerClient.Get(ctx, req.NamespacedName, proxy); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get HTTPProxy: %w", err)
	}

	if !proxy.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	if err := ensureNetworkingEntitlement(ctx, consumerClient); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func ensureNetworkingEntitlement(ctx context.Context, consumerClient client.Client) error {
	var existing servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(ctx, &existing); err != nil {
		return fmt.Errorf("failed to list ServiceEntitlements: %w", err)
	}
	for i := range existing.Items {
		if isNetworkingEntitlement(&existing.Items[i]) {
			return nil
		}
	}

	entitlement := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Name: networkingServiceObjectName,
			Labels: map[string]string{
				httpProxyEnrolledLabel: "true",
			},
		},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: networkingServiceObjectName},
		},
	}
	if err := consumerClient.Create(ctx, entitlement); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create ServiceEntitlement %q: %w", networkingServiceObjectName, err)
	}
	log.FromContext(ctx).Info("enrolled project in networking from HTTPProxy")
	return nil
}

func isNetworkingEntitlement(se *servicesv1alpha1.ServiceEntitlement) bool {
	switch se.Spec.ServiceRef.Name {
	case networkingServiceObjectName, networkingServiceCanonicalName:
		return true
	}
	return se.Status.ServiceName == networkingServiceCanonicalName
}

func (r *HTTPProxyEntitlementReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.Manager = mgr
	proxy := &unstructured.Unstructured{}
	proxy.SetGroupVersionKind(httpProxyGVK)
	// For HTTPProxy, not ServiceEntitlement: the informer list on engage
	// backfills every existing ALB, including projects that never created a
	// networking entitlement. Networking APIs are aggregated into every
	// project control plane (ALB creation never required entitlement), so
	// the watch can sync. e2e installs a stub CRD for the same reason.
	return mcbuilder.ControllerManagedBy(mgr).
		Named("httpproxy-entitlement").
		For(proxy, mcbuilder.WithEngageWithProviderClusters(true)).
		Complete(r)
}
