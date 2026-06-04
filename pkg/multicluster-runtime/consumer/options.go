// SPDX-License-Identifier: AGPL-3.0-only

package consumer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

// DefaultResyncInterval is the periodic full-resync cadence applied when
// Options.ResyncInterval is unset. It aligns with the catalog's existing
// location-binding resync, but the library owns the default so adopters need not
// depend on a catalog constant.
const DefaultResyncInterval = 5 * time.Minute

// Options configures a Provider.
type Options struct {
	// ServiceNames is the set of CANONICAL service names (Service.spec.serviceName,
	// e.g. "compute.miloapis.com") this operator owns. Only ServiceConsumers whose
	// spec.serviceRef.name resolves to one of these canonical names are watched,
	// counted, and (in later phases) torn down. Required, non-empty. A provider
	// project may host ServiceConsumers for other services; those are ignored.
	ServiceNames []string

	// ClusterOptions is applied to every engaged consumer cluster.Cluster.
	// Callers MUST set the scheme here (e.g.
	// func(o *cluster.Options) { o.Scheme = scheme }) or consumer-side caches fall
	// back to the client-go global scheme and every consumer-side watch fails with
	// "kind must be registered to the Scheme".
	ClusterOptions []cluster.Option

	// ManagedResources lists the resource types this operator CREATES AND OWNS in
	// a consumer project — the set deleted when the project deactivates. On
	// deactivation the provider deletes every object of these types carrying the
	// services.miloapis.com/service-name label for one of ServiceNames. Deletes are
	// label-scoped and idempotent.
	//
	// Wired in Phase 2 (deactivation cleanup); declared here so the API is stable.
	ManagedResources []schema.GroupVersionKind

	// Teardowns is the escape hatch for cleanup a label-scoped delete cannot
	// express (ordering between types, finalizer coordination, external systems).
	// Each runs, in order, AFTER ManagedResources deletion and AFTER the per-cluster
	// context is cancelled, against a non-cached direct client. Each must be
	// idempotent.
	//
	// Wired in Phase 2 (deactivation cleanup); declared here so the API is stable.
	Teardowns []Teardown

	// ResyncInterval is the periodic full-resync cadence. Defaults to
	// DefaultResyncInterval when zero.
	ResyncInterval time.Duration

	// newCluster is an injection seam for tests; defaults to cluster.New.
	newCluster func(*rest.Config, ...cluster.Option) (cluster.Cluster, error)

	// newClient is an injection seam for tests; defaults to client.New. It builds
	// the NON-CACHED direct client used for deactivation teardown — the engaged
	// cluster's cache is being torn down at that point, so teardown must talk to
	// the API server directly.
	newClient func(*rest.Config, client.Options) (client.Client, error)
}

// Teardown removes the resources a single operator created in a consumer project
// when that project loses its last active ServiceConsumer for the caller's
// services.
type Teardown interface {
	// TeardownConsumer deletes the resources the caller created in the consumer
	// project. It MUST be idempotent and MUST scope deletes to the caller's
	// service (by the services.miloapis.com/service-name label and/or owner-ref) —
	// NEVER the coarse app.kubernetes.io/managed-by label, which a second operator
	// in the same project would share. It MUST NOT delete provider-side
	// ServiceConsumers. A non-nil error aborts disengage and is retried with
	// backoff (alert-only; never force-cancel, never auto-leak).
	TeardownConsumer(ctx context.Context, consumerProject string,
		consumerClient client.Client, serviceNames []string) error
}
