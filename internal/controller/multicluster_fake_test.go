// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// testScheme returns a scheme with the services, billing, quota, and
// resourcemanager API types registered. Resourcemanager is needed by
// OrganizationDefaultsReconciler tests; billing types are needed by
// BillingEntitlement quota-gating tests.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = billingv1alpha1.AddToScheme(s)
	_ = quotav1alpha1.AddToScheme(s)
	_ = resourcemanagerv1alpha1.AddToScheme(s)
	return s
}

// testCluster wraps a fake client to satisfy cluster.Cluster. Only
// GetClient is exercised by the reconcilers under test; every other
// method returns a zero value.
type testCluster struct {
	client client.Client
}

func (c *testCluster) GetClient() client.Client                        { return c.client }
func (c *testCluster) GetScheme() *runtime.Scheme                      { return nil }
func (c *testCluster) GetHTTPClient() *http.Client                     { return nil }
func (c *testCluster) GetConfig() *rest.Config                         { return nil }
func (c *testCluster) GetCache() cache.Cache                           { return nil }
func (c *testCluster) GetFieldIndexer() client.FieldIndexer            { return nil }
func (c *testCluster) GetEventRecorderFor(string) record.EventRecorder { return nil }
func (c *testCluster) GetEventRecorder(string) events.EventRecorder    { return nil }
func (c *testCluster) GetRESTMapper() meta.RESTMapper                  { return nil }
func (c *testCluster) GetAPIReader() client.Reader                     { return nil }
func (c *testCluster) Start(context.Context) error                     { return nil }

// testManager implements mcmanager.Manager. GetCluster looks up a named
// cluster from the clusters map; unknown names return an error so the
// reconciler exercises its "provider not engaged yet" branch.
type testManager struct {
	clusters map[string]*testCluster
}

func newTestManager() *testManager {
	return &testManager{clusters: map[string]*testCluster{}}
}

func (m *testManager) add(name string, c client.Client) {
	m.clusters[name] = &testCluster{client: c}
}

func (m *testManager) GetCluster(_ context.Context, name multicluster.ClusterName) (cluster.Cluster, error) {
	c, ok := m.clusters[string(name)]
	if !ok {
		return nil, fmt.Errorf("cluster %q not engaged", name)
	}
	return c, nil
}

func (m *testManager) Add(mcmanager.Runnable) error                            { return nil }
func (m *testManager) Elected() <-chan struct{}                                { return nil }
func (m *testManager) AddMetricsServerExtraHandler(string, http.Handler) error { return nil }
func (m *testManager) AddHealthzCheck(string, healthz.Checker) error           { return nil }
func (m *testManager) AddReadyzCheck(string, healthz.Checker) error            { return nil }
func (m *testManager) Start(context.Context) error                             { return nil }
func (m *testManager) GetWebhookServer() webhook.Server                        { return nil }
func (m *testManager) GetLogger() logr.Logger                                  { return logr.Discard() }
func (m *testManager) GetControllerOptions() config.Controller                 { return config.Controller{} }
func (m *testManager) ClusterFromContext(context.Context) (cluster.Cluster, error) {
	return nil, nil
}
func (m *testManager) GetManager(context.Context, multicluster.ClusterName) (manager.Manager, error) {
	return nil, nil
}
func (m *testManager) GetLocalManager() manager.Manager     { return nil }
func (m *testManager) GetProvider() multicluster.Provider   { return nil }
func (m *testManager) GetFieldIndexer() client.FieldIndexer { return nil }
func (m *testManager) Engage(context.Context, multicluster.ClusterName, cluster.Cluster) error {
	return nil
}

// newFakeClient builds a fake client with the services scheme and full
// status-subresource support for our types. SSA Apply patches are handled
// via the ssaClient shim defined in entitlement_quota_grants_test.go.
func newFakeClient(objs ...client.Object) client.Client {
	base := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(
			&servicesv1alpha1.ServiceEntitlement{},
			&servicesv1alpha1.ServiceConsumer{},
			&servicesv1alpha1.Service{},
			&resourcemanagerv1alpha1.Project{},
		).
		WithIndex(&servicesv1alpha1.ServiceEntitlement{}, entitlementServiceNameIndex, entitlementServiceNameIndexer).
		WithIndex(&servicesv1alpha1.Service{}, "spec.serviceName", func(obj client.Object) []string {
			svc := obj.(*servicesv1alpha1.Service)
			if svc.Spec.ServiceName == "" {
				return nil
			}
			return []string{svc.Spec.ServiceName}
		}).
		WithIndex(&servicesv1alpha1.ServiceConfiguration{}, "spec.serviceRef.name", func(obj client.Object) []string {
			sc := obj.(*servicesv1alpha1.ServiceConfiguration)
			if sc.Spec.ServiceRef.Name == "" {
				return nil
			}
			return []string{sc.Spec.ServiceRef.Name}
		}).
		WithIndex(&billingv1alpha1.BillingAccountBinding{}, bindingProjectRefIndex, func(obj client.Object) []string {
			b := obj.(*billingv1alpha1.BillingAccountBinding)
			if b.Spec.ProjectRef.Name == "" {
				return nil
			}
			return []string{b.Spec.ProjectRef.Name}
		}).
		WithIndex(&billingv1alpha1.BillingAccountBinding{}, bindingBillingAccountRefIndex, func(obj client.Object) []string {
			b := obj.(*billingv1alpha1.BillingAccountBinding)
			if b.Spec.BillingAccountRef.Name == "" {
				return nil
			}
			return []string{b.Spec.BillingAccountRef.Name}
		}).
		WithIndex(&billingv1alpha1.BillingEntitlement{}, billingEntitlementOfferIndex, func(obj client.Object) []string {
			be := obj.(*billingv1alpha1.BillingEntitlement)
			if be.Spec.OfferRef.Name == "" {
				return nil
			}
			return []string{be.Spec.OfferRef.Name}
		}).
		Build()
	return &ssaClient{Client: base}
}

// newAdmissionFakeClient wraps newFakeClient with the one admission rule that
// governs ServiceEntitlement creates: the validating webhook resolves
// spec.serviceRef.name by metadata.name only, so a create naming a Service's
// canonical name is rejected. The plain fake client has no admission at all,
// which is how the controller writing an inadmissible serviceRef went
// unnoticed by the dependency tests.
func newAdmissionFakeClient(rootClient client.Client, objs ...client.Object) client.Client {
	return &admissionClient{Client: newFakeClient(objs...), root: rootClient}
}

type admissionClient struct {
	client.Client
	root client.Client
}

func (c *admissionClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	se, ok := obj.(*servicesv1alpha1.ServiceEntitlement)
	if !ok {
		return c.Client.Create(ctx, obj, opts...)
	}
	var svc servicesv1alpha1.Service
	if err := c.root.Get(ctx, client.ObjectKey{Name: se.Spec.ServiceRef.Name}, &svc); err != nil {
		return apierrors.NewInvalid(
			servicesv1alpha1.GroupVersion.WithKind("ServiceEntitlement").GroupKind(),
			se.Name,
			field.ErrorList{field.Invalid(
				field.NewPath("spec", "serviceRef", "name"), se.Spec.ServiceRef.Name,
				fmt.Sprintf("the service %q does not exist", se.Spec.ServiceRef.Name),
			)},
		)
	}
	return c.Client.Create(ctx, obj, opts...)
}
