package activation

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	versioned "go.miloapis.com/service-catalog/pkg/generated/clientset/versioned"
)

// EntitlementClient is the narrow surface the flow needs against a project's
// virtual control plane. Keeping it minimal lets tests inject the generated
// fake clientset and simulate catalog-absent, already-exists, and watch-event
// sequences without a real API server. ServiceEntitlements are cluster-scoped,
// so no namespace is carried.
type EntitlementClient interface {
	// List returns every ServiceEntitlement visible in the control plane.
	List(ctx context.Context) (*servicesv1alpha1.ServiceEntitlementList, error)
	// Get fetches a single entitlement by name.
	Get(ctx context.Context, name string) (*servicesv1alpha1.ServiceEntitlement, error)
	// Create submits a new entitlement, writing the server's response back into e.
	Create(ctx context.Context, e *servicesv1alpha1.ServiceEntitlement) error
	// Delete removes an entitlement.
	Delete(ctx context.Context, e *servicesv1alpha1.ServiceEntitlement) error
	// Watch returns a watcher scoped to a single entitlement by name, seeded at
	// resourceVersion (empty means "from now").
	Watch(ctx context.Context, name, resourceVersion string) (watch.Interface, error)
}

// CatalogClient is the narrow, list-only surface against the platform-wide
// root API server. The CLI never mutates Service — that stays the producer's
// own publishing flow — so this is a single method.
type CatalogClient interface {
	// ListServices returns every Service visible at this scope.
	ListServices(ctx context.Context) (*servicesv1alpha1.ServiceList, error)
}

// clientsetAdapter maps the generated services clientset onto both
// EntitlementClient and CatalogClient. A single adapter (and a single fake
// clientset in tests) backs both scopes because the generated clientset
// already serves both Services() and ServiceEntitlements().
type clientsetAdapter struct {
	cs versioned.Interface
}

// NewClient wraps a generated services clientset (real or fake) as an
// EntitlementClient. Tests pass the generated fake; production passes a
// clientset built from a REST config.
func NewClient(cs versioned.Interface) EntitlementClient {
	return &clientsetAdapter{cs: cs}
}

// NewRESTClient builds an EntitlementClient from a Kubernetes REST config. This
// is the auth seam: callers supply a config already carrying the control-plane
// host and a bearer token.
func NewRESTClient(cfg *rest.Config) (EntitlementClient, error) {
	cs, err := versioned.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building services clientset: %w", err)
	}
	return NewClient(cs), nil
}

// NewCatalogClient wraps a generated services clientset (real or fake) as a
// CatalogClient.
func NewCatalogClient(cs versioned.Interface) CatalogClient {
	return &clientsetAdapter{cs: cs}
}

// NewCatalogRESTClient builds a CatalogClient from a Kubernetes REST config.
// Service lives at the platform-wide root API server, so callers typically
// supply a different *rest.Config than the one passed to NewRESTClient.
func NewCatalogRESTClient(cfg *rest.Config) (CatalogClient, error) {
	cs, err := versioned.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building services clientset: %w", err)
	}
	return NewCatalogClient(cs), nil
}

// entitlements is the subset of the generated ServiceEntitlementInterface the
// adapter calls. Naming it keeps the methods readable and documents the exact
// surface the flow depends on.
func (a *clientsetAdapter) entitlements() clientEntitlements {
	return a.cs.ServicesV1alpha1().ServiceEntitlements()
}

func (a *clientsetAdapter) List(ctx context.Context) (*servicesv1alpha1.ServiceEntitlementList, error) {
	return a.entitlements().List(ctx, metav1.ListOptions{})
}

func (a *clientsetAdapter) Get(ctx context.Context, name string) (*servicesv1alpha1.ServiceEntitlement, error) {
	return a.entitlements().Get(ctx, name, metav1.GetOptions{})
}

func (a *clientsetAdapter) Create(ctx context.Context, e *servicesv1alpha1.ServiceEntitlement) error {
	created, err := a.entitlements().Create(ctx, e, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	// The clientset returns a fresh object; copy it back so callers see the
	// server-assigned resourceVersion (used to seed the post-create watch).
	*e = *created
	return nil
}

func (a *clientsetAdapter) Delete(ctx context.Context, e *servicesv1alpha1.ServiceEntitlement) error {
	return a.entitlements().Delete(ctx, e.Name, metav1.DeleteOptions{})
}

func (a *clientsetAdapter) Watch(ctx context.Context, name, resourceVersion string) (watch.Interface, error) {
	return a.entitlements().Watch(ctx, metav1.ListOptions{
		// Scope the watch to the single named entitlement and resume from the
		// create response's resourceVersion so the first status write is not
		// missed between the create and the watch establishing.
		FieldSelector:   "metadata.name=" + name,
		ResourceVersion: resourceVersion,
	})
}

// ListServices lists every Service visible at this client's scope.
func (a *clientsetAdapter) ListServices(ctx context.Context) (*servicesv1alpha1.ServiceList, error) {
	return a.cs.ServicesV1alpha1().Services().List(ctx, metav1.ListOptions{})
}

// clientEntitlements is the subset of the generated ServiceEntitlementInterface
// the adapter uses. The generated interface is a superset, so it satisfies this.
type clientEntitlements interface {
	List(ctx context.Context, opts metav1.ListOptions) (*servicesv1alpha1.ServiceEntitlementList, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*servicesv1alpha1.ServiceEntitlement, error)
	Create(ctx context.Context, e *servicesv1alpha1.ServiceEntitlement, opts metav1.CreateOptions) (*servicesv1alpha1.ServiceEntitlement, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}
