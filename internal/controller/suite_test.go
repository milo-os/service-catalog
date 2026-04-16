// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestControllers(t *testing.T) {
	// envtest relies on the `kube-apiserver` and `etcd` binaries installed
	// via the envtest assets flow. When those assets aren't present (most
	// notably in pure unit-test runs like `go test ./internal/...`), skip
	// the envtest-backed specs rather than fail the build.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping controller envtest suite")
	}

	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "base", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = servicesv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	if cancel != nil {
		cancel()
	}
	if testEnv != nil {
		By("tearing down the test environment")
		Expect(testEnv.Stop()).To(Succeed())
	}
})

// Reconciler-level envtest specs are deliberately deferred. The
// validation package carries the unit-level coverage; the envtest
// harness is scaffolded here so that service_controller and
// serviceconfiguration_controller specs can be filled in without
// re-plumbing the test environment.
var _ = Describe("ServiceReconciler", func() {})
var _ = Describe("ServiceConfigurationReconciler", func() {})
