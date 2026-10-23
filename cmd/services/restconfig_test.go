// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"testing"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"go.miloapis.com/service-catalog/internal/clientmetrics"
	"go.miloapis.com/service-catalog/internal/config"
)

// The reported limit must be the one client-go builds from the returned
// config. If the config's limits change after the report, the dashboard
// shows a limit the clients do not use.
func TestMiloRestConfigReportsTheLimitClientsUse(t *testing.T) {
	clientmetrics.Register()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	kubeconfig := `apiVersion: v1
kind: Config
current-context: milo
clusters:
- name: milo
  cluster:
    server: https://milo.example.com
users:
- name: operator
  user:
    token: test
contexts:
- name: milo
  context: {cluster: milo, user: operator}
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	cfg, err := miloRestConfig(&config.ServicesOperator{KubeconfigPath: path}, defaultKubeAPIQPS, defaultKubeAPIBurst)
	if err != nil {
		t.Fatalf("miloRestConfig: %v", err)
	}

	clientCfg := rest.CopyConfig(cfg)
	clientCfg.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	restClient, err := rest.UnversionedRESTClientFor(clientCfg)
	if err != nil {
		t.Fatalf("build REST client: %v", err)
	}
	var want float64
	if limiter := restClient.GetRateLimiter(); limiter != nil {
		want = float64(limiter.QPS())
	}

	if want != float64(defaultKubeAPIQPS) {
		t.Fatalf("client QPS = %v, want the --kube-api-qps default %v", want, defaultKubeAPIQPS)
	}
	if got := reportedQPS(t); got != want {
		t.Fatalf("reported client QPS = %v, want %v (the limit client-go builds from the returned config)", got, want)
	}
}

func reportedQPS(t *testing.T) float64 {
	t.Helper()
	families, err := crmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "service_catalog_client_rate_limit" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "limit" && label.GetValue() == "qps" {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatal("service_catalog_client_rate_limit{limit=\"qps\"} is not registered")
	return 0
}
