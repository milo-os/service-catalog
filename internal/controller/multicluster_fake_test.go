// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
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

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

// testScheme returns a scheme with the services API types registered.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
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

func (m *testManager) GetCluster(_ context.Context, name string) (cluster.Cluster, error) {
	c, ok := m.clusters[name]
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
func (m *testManager) GetManager(context.Context, string) (manager.Manager, error) {
	return nil, nil
}
func (m *testManager) GetLocalManager() manager.Manager                      { return nil }
func (m *testManager) GetProvider() multicluster.Provider                    { return nil }
func (m *testManager) GetFieldIndexer() client.FieldIndexer                  { return nil }
func (m *testManager) Engage(context.Context, string, cluster.Cluster) error { return nil }

// newFakeClient builds a fake client with the services scheme and full
// status-subresource support for our types.
func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(
			&servicesv1alpha1.ServiceEntitlement{},
			&servicesv1alpha1.ServiceConsumer{},
			&servicesv1alpha1.Service{},
		).
		Build()
}
