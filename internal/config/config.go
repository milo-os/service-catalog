// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// ServicesOperator is the configuration for the services operator.
type ServicesOperator struct {
	metav1.TypeMeta

	MetricsServer MetricsServerConfig `json:"metricsServer"`

	// WebhookServer configures the admission webhook server. When unset, the
	// manager runs without an admission webhook server and no serving cert
	// is required.
	WebhookServer *WebhookServerConfig `json:"webhookServer,omitempty"`

	// KubeconfigPath is the path to the kubeconfig file pointing at the Milo
	// API server where services resources (Service, MeterDefinition,
	// MonitoredResourceType) are stored. When empty the controller falls back
	// to in-cluster config / $KUBECONFIG via ctrl.GetConfig(), which is
	// useful for local development where the controller and API server share
	// a cluster.
	KubeconfigPath string `json:"kubeconfigPath,omitempty"`

	// LocationSource names the API group Locations are read from when
	// evaluating whether a service is available at a location and which
	// locations an entitled project can see. Both groups serve the same kind
	// under different shapes, and during the move from one to the other both
	// may be installed at once — so the source is chosen here rather than
	// inferred from which CRDs happen to be present, which would decide
	// silently and could resolve some locations from one group and some from
	// the other.
	//
	// Defaults to the network-services group, which is what control planes
	// serve today. Switching to the locations service is a deliberate act:
	// once set, that group is the only one read, and a control plane that does
	// not serve it reports the misconfiguration on every ServiceAvailability
	// rather than quietly falling back.
	//
	// +optional
	LocationSource LocationSource `json:"locationSource,omitempty"`

	// ConsumerScopedProjection, when set, switches projection of
	// consumer-facing resources (today: LocationBinding) off the all-projects
	// Milo manager and onto a consumer-scoped multicluster manager driven by
	// the consumer provider: the operator engages a consumer project only while
	// it has an active ServiceConsumer for one of ServiceNames, and tears down
	// the resources it projected there when the last consumer goes away. When
	// nil the operator keeps today's behavior, byte-for-byte — LocationBinding
	// projection runs on the all-projects manager. This mirrors the
	// WebhookServer pointer-gate above: nil = feature off.
	ConsumerScopedProjection *ConsumerScopedProjectionConfig `json:"consumerScopedProjection,omitempty"`
}

// LocationSource names the API group Locations are read from.
type LocationSource string

const (
	// LocationSourceNetworkServices reads Locations from the network-services
	// operator's group, where a location carries its class as the flat
	// spec.locationClassName. This is the default and what production serves.
	LocationSourceNetworkServices LocationSource = "networking.datumapis.com/v1alpha"

	// LocationSourceLocationsService reads Locations from milo-os/locations,
	// where a location names a LocationClass through spec.locationClassRef.
	LocationSourceLocationsService LocationSource = "locations.miloapis.com/v1alpha1"
)

// locationSourceGVKs maps each source onto the GroupVersionKind read for it.
var locationSourceGVKs = map[LocationSource]schema.GroupVersionKind{
	LocationSourceNetworkServices: {
		Group:   "networking.datumapis.com",
		Version: "v1alpha",
		Kind:    "Location",
	},
	LocationSourceLocationsService: {
		Group:   "locations.miloapis.com",
		Version: "v1alpha1",
		Kind:    "Location",
	},
}

// GVK resolves the configured location source, rejecting a value that names no
// known group so the manager fails at startup rather than on first reconcile.
func (s LocationSource) GVK() (schema.GroupVersionKind, error) {
	gvk, ok := locationSourceGVKs[s]
	if !ok {
		return schema.GroupVersionKind{}, fmt.Errorf(
			"unknown locationSource %q, expected one of %q or %q",
			s, LocationSourceNetworkServices, LocationSourceLocationsService)
	}
	return gvk, nil
}

// RestConfig returns the *rest.Config used to connect to the Milo API server.
// When KubeconfigPath is empty it falls back to the standard
// controller-runtime config resolution (in-cluster / $KUBECONFIG).
func (c *ServicesOperator) RestConfig() (*rest.Config, error) {
	if c.KubeconfigPath == "" {
		return ctrl.GetConfig()
	}
	return clientcmd.BuildConfigFromFlags("", c.KubeconfigPath)
}

// +k8s:deepcopy-gen=true

// WebhookServerConfig configures the admission webhook server.
type WebhookServerConfig struct {
	// Host is the address that the server will listen on.
	// Defaults to "" - all addresses.
	Host string `json:"host"`

	// Port is the port number that the server will serve.
	// It will be defaulted to 9443 if unspecified.
	Port int `json:"port"`

	// TLS is the TLS configuration for the webhook server.
	TLS TLSConfig `json:"tls"`

	// ClientCAName is the CA certificate name which server used to verify remote(client)'s certificate.
	ClientCAName string `json:"clientCAName"`
}

func SetDefaults_WebhookServerConfig(obj *WebhookServerConfig) {
	if obj.TLS.CertDir == "" {
		obj.TLS.CertDir = filepath.Join(os.TempDir(), "k8s-webhook-server", "serving-certs")
	}
}

func (c *WebhookServerConfig) Options(ctx context.Context, secretsClient client.Client) webhook.Options {
	opts := webhook.Options{
		Host:     c.Host,
		Port:     c.Port,
		CertDir:  c.TLS.CertDir,
		CertName: c.TLS.CertName,
		KeyName:  c.TLS.KeyName,
	}

	if secretRef := c.TLS.SecretRef; secretRef != nil {
		opts.TLSOpts = c.TLS.Options(ctx, secretsClient)
	}

	return opts
}

// +k8s:deepcopy-gen=true

// ConsumerScopedProjectionConfig configures consumer-scoped projection. When
// present on ServicesOperator, the operator engages a consumer project only
// while it has an active ServiceConsumer for one of ServiceNames and projects
// its consumer-facing resources (LocationBinding) there, tearing them down when
// the last consumer goes away.
type ConsumerScopedProjectionConfig struct {
	// ProviderProject is the name of the provider Project whose virtual control
	// plane hosts this operator's ServiceConsumer objects. It is the single
	// source of truth for routing — the manager builds providerMgr against this
	// project's control plane and gates startup on its readiness. Required when
	// this block is set; the manager fails fast at startup if it is empty.
	ProviderProject string `json:"providerProject"`

	// ServiceNames is the set of CANONICAL service names
	// (Service.spec.serviceName, e.g. "compute.miloapis.com") this operator
	// owns. Only ServiceConsumers resolving to one of these canonical names are
	// engaged and projected into; a provider project may host ServiceConsumers
	// for other services, which are ignored. Required, non-empty.
	ServiceNames []string `json:"serviceNames"`

	// ResyncInterval optionally overrides the consumer provider's periodic
	// full-resync cadence. When unset the provider uses its own default (5m).
	ResyncInterval *metav1.Duration `json:"resyncInterval,omitempty"`
}

// +k8s:deepcopy-gen=true

// MetricsServerConfig configures the metrics server.
type MetricsServerConfig struct {
	// SecureServing enables serving metrics via https.
	SecureServing *bool `json:"secureServing,omitempty"`

	// BindAddress is the bind address for the metrics server.
	BindAddress string `json:"bindAddress"`

	// TLS is the TLS configuration for the metrics server.
	TLS TLSConfig `json:"tls"`
}

func SetDefaults_MetricsServerConfig(obj *MetricsServerConfig) {
	if obj.SecureServing == nil {
		obj.SecureServing = ptr.To(true)
	}

	if obj.BindAddress == "" {
		obj.BindAddress = "0"
	}

	if len(obj.TLS.CertDir) == 0 {
		obj.TLS.CertDir = filepath.Join(os.TempDir(), "k8s-metrics-server", "serving-certs")
	}
}

func (c *MetricsServerConfig) Options(ctx context.Context, secretsClient client.Client) metricsserver.Options {
	secureServing := c.SecureServing != nil && *c.SecureServing

	opts := metricsserver.Options{
		SecureServing: secureServing,
		BindAddress:   c.BindAddress,
		CertDir:       c.TLS.CertDir,
		CertName:      c.TLS.CertName,
		KeyName:       c.TLS.KeyName,
	}

	if secureServing {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if secretRef := c.TLS.SecretRef; secretRef != nil {
		opts.TLSOpts = c.TLS.Options(ctx, secretsClient)
	}

	return opts
}

// +k8s:deepcopy-gen=true

// TLSConfig configures TLS certificate management.
type TLSConfig struct {
	// SecretRef is a reference to a secret that contains the server key and certificate.
	SecretRef *corev1.ObjectReference `json:"secretRef,omitempty"`

	// CertDir is the directory that contains the server key and certificate.
	CertDir string `json:"certDir"`

	// CertName is the server certificate name. Defaults to tls.crt.
	CertName string `json:"certName"`

	// KeyName is the server key name. Defaults to tls.key.
	KeyName string `json:"keyName"`
}

func (c *TLSConfig) Options(ctx context.Context, secretsClient client.Client) []func(*tls.Config) {
	var tlsOpts []func(*tls.Config)

	if secretRef := c.SecretRef; secretRef != nil {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			logger := ctrl.Log.WithName("tls-client")
			c.GetCertificate = func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				logger.Info("getting certificate")

				var secret corev1.Secret
				secretObjectKey := types.NamespacedName{
					Name:      secretRef.Name,
					Namespace: secretRef.Namespace,
				}
				if err := secretsClient.Get(ctx, secretObjectKey, &secret); err != nil {
					return nil, fmt.Errorf("failed to get secret: %w", err)
				}

				cert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
				if err != nil {
					return nil, fmt.Errorf("failed to parse certificate: %w", err)
				}

				return &cert, nil
			}
		})
	}

	return tlsOpts
}

func SetDefaults_TLSConfig(obj *TLSConfig) {
	if len(obj.CertName) == 0 {
		obj.CertName = "tls.crt"
	}

	if len(obj.KeyName) == 0 {
		obj.KeyName = "tls.key"
	}
}

// SetDefaults_ServicesOperator sets defaults for ServicesOperator.
// The generated SetObjectDefaults_ServicesOperator handles calling nested
// defaults (MetricsServerConfig, WebhookServerConfig, TLSConfig), so this
// function only sets top-level defaults.
func SetDefaults_ServicesOperator(obj *ServicesOperator) {
	// Locations are read from the group control planes serve today unless an
	// operator deliberately moves them.
	if obj.LocationSource == "" {
		obj.LocationSource = LocationSourceNetworkServices
	}
}

func init() {
	SchemeBuilder.Register(&ServicesOperator{})
}
