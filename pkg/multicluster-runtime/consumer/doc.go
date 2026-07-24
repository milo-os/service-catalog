// SPDX-License-Identifier: AGPL-3.0-only

// Package consumer implements a reusable sigs.k8s.io/multicluster-runtime
// Provider that engages a consumer project as a cluster.Cluster only while that
// project has an active ServiceConsumer for at least one of the operator's owned
// services, and tears down the resources the operator created there once none
// remain.
//
// It is the consumer analogue of Milo's platform provider: Milo watches Project
// and engages on Ready; this provider watches ServiceConsumer in the PROVIDER
// project and engages a CONSUMER project on active membership. The service
// catalog is the first adopter (it projects LocationBindings into consumer
// projects), but the library is operator-agnostic.
//
// # Two-manager topology
//
// A caller runs two managers. providerMgr is a plain controller-runtime manager
// pointed at the PROVIDER project — where the ServiceConsumer records live and
// the single source of truth for routing; the Provider registers itself as a
// reconciler on it. consumerMcMgr is a multicluster manager whose membership is
// driven by this Provider; its engaged clusters are exactly the consumer
// projects currently using one of the operator's services. The operator's own
// reconcilers move from providerMgr/the all-projects manager onto consumerMcMgr
// unchanged — they still key off req.ClusterName (the consumer project) and
// Manager.GetCluster for the consumer-scoped client.
//
// # List-based recompute (restart-safe)
//
// Membership is recomputed from a List on every reconcile, never from an
// in-memory watch map. Two facts force this:
//
//   - ServiceConsumer is hash-named (sc-<sha256(service+project)[:8]>). A delete
//     event delivers only the hash key, so the consumer project needed to
//     disengage is unrecoverable from a delete event alone.
//   - A map built from watch replay is not restart-safe: a consumer removed or
//     revoked while the operator was down produces no replay event on restart.
//
// Listing on every reconcile, plus a periodic full resync (see ResyncInterval),
// makes the active set self-healing regardless of missed events. Each reconcile
// resolves every relevant ServiceConsumer to its canonical service name, buckets
// by consumer project, counts active-and-not-deleting consumers per project, and
// engages or disengages on the delta. The disengage candidate set is every
// project whose ServiceConsumer for the operator's services is Denied or being
// deleted (revoked), unioned with the currently-engaged projects, minus the
// active set — so a project whose consumer was revoked to Denied (or left
// mid-deletion) while the operator was DOWN is still torn down on the next
// reconcile after a restart, even though it was never engaged in this process. A
// consumer that never became active (e.g. PendingApproval) projected no
// resources, so it is correctly not a teardown candidate.
//
// Restart-safety residual: this self-healing covers only consumers whose
// ServiceConsumer OBJECT is still present (active, Denied, or deleting). A
// consumer that was FULLY DELETED while the operator was down would leave no
// ServiceConsumer to list, so the provider could not discover that project and
// would not sweep it — which is exactly why the provider gates the
// ServiceConsumer's deletion (see "Deprovisioning finalizer" below): the object
// cannot disappear until the operator has confirmed teardown, so a
// fully-deleted-while-down consumer is not a reachable state. Consumer-side
// owner-reference garbage collection remains a backstop for resources inside the
// consumer's own control plane, but it cannot reach cross-cluster or federated
// resources; the finalizer is what guarantees those are reclaimed.
//
// # Deprovisioning finalizer
//
// An operator that projects resources cannot rely on merely OBSERVING a
// deletion in time — the consumer's control plane can be torn down before the
// poll fires, stranding whatever the operator created outside it. To make
// teardown a GATE rather than a best-effort reaction, the provider stamps the
// services.miloapis.com/provider-teardown finalizer on each of its
// ServiceConsumers while they are active, and removes it only after teardown of
// that project succeeds. It never DELETES a provider-side ServiceConsumer (the
// consumer-side ServiceEntitlement cascade owns deletion); the finalizer only
// holds the deletion open until teardown confirms. The consumer-side
// ServiceEntitlement finalizer in turn holds the entitlement — and thus the
// project — until the ServiceConsumer is gone, chaining the guarantee all the
// way up to project deletion. The finalizer is stamped only when the operator
// declared ManagedResources or Teardowns, so an operator that projects nothing
// never gates a deletion.
//
// # Canonical service-name keying
//
// Options.ServiceNames are CANONICAL Service.spec.serviceName values (e.g.
// "compute.miloapis.com"), NOT Service object names (e.g.
// "compute-miloapis-com"). A ServiceConsumer's spec.serviceRef.name holds the
// OBJECT name, so the provider resolves serviceRef.name → Service.spec.serviceName
// (reading the Service from the provider project) before matching against
// ServiceNames. This keeps the engagement match-key identical to the cleanup
// label value, so teardown deletes exactly what engagement counted.
//
// # Adopter contract
//
// For every resource the operator CREATES in a consumer project, the adopter
// must:
//
//  1. Label it services.miloapis.com/service-name: <canonical service name> —
//     drives the project-level teardown sweep.
//  2. Owner-ref it to its consumer-side ServiceEntitlement — drives
//     per-entitlement garbage collection when a single entitlement is removed
//     while the service stays active.
//  3. Declare its GroupVersionKind in Options.ManagedResources — the sweep
//     deletes the declared types, not "whatever happens to exist." A type
//     created in consumer projects but absent from ManagedResources will NOT be
//     torn down.
//
// Deletes are scoped by the services.miloapis.com/service-name label for the
// operator's ServiceNames and are idempotent. The provider NEVER deletes by the
// coarse app.kubernetes.io/managed-by label (a second operator in the same
// project would share it) and NEVER deletes provider-side ServiceConsumers.
//
// # Deactivation and cancel-before-teardown
//
// When a consumer project's last active ServiceConsumer for the operator's
// services goes away (consumer deleted, or provider-revoked to Denied), the
// provider disengages it: it cancels the per-cluster context FIRST (stopping the
// operator's reconcilers for that cluster, so nothing re-creates resources
// mid-teardown), then deletes every object of the declared ManagedResources
// types carrying the service-name label via a NON-CACHED direct client (the
// cache is being torn down), then runs any Options.Teardowns in order. A non-nil
// error from deletion or any teardown ABORTS disengage and requeues with
// backoff; the cluster stays tracked and teardown is retried — never
// force-cancelled past a failure, never silently leaked.
//
// # The Get sentinel expectation
//
// Get returns multicluster.ErrClusterNotFound wrapped with %w when a project is
// not engaged (or is mid-teardown), so the framework's ClusterNotFoundWrapper
// recognizes the sentinel and silently drops stale requeues instead of logging
// errors. Callers inspecting the error MUST use errors.Is(err,
// multicluster.ErrClusterNotFound) rather than string-matching. A project that
// is engaged but tearing down reports not-engaged via the same sentinel.
//
// # Readiness gate
//
// WaitProviderProjectReady blocks until the provider Project reports
// status.conditions[Ready]=True before providerMgr.Start, so a manager pointed
// at a not-yet-ready project does not crash-loop. Call it in the providerMgr
// start goroutine.
//
// # Metrics
//
// The provider registers two metrics on controller-runtime's metrics registry
// (served on the manager's existing /metrics endpoint, no adopter wiring):
//
//   - consumer_provider_engaged_clusters (gauge): the current number of engaged
//     consumer projects.
//   - consumer_provider_teardown_failures_total{consumer_project} (counter):
//     per-project teardown failures; alert-only, bounded cardinality (only
//     failing projects get a series). A persistently-incrementing series
//     indicates a consumer project whose teardown keeps failing while the
//     cluster stays engaged for retry.
//
// # Usage
//
//	// 1. Manager pointed at the PROVIDER project (single source of truth).
//	providerCfg, _ := consumer.ProjectRestConfig(baseCfg, providerProject)
//	providerMgr, _ := ctrl.NewManager(providerCfg, ctrl.Options{Scheme: scheme})
//
//	// 2. The provider: services owned + resource types projected.
//	provider, _ := consumer.New(providerMgr, consumer.Options{
//		ServiceNames:   []string{"compute.miloapis.com"}, // canonical names
//		ClusterOptions: []cluster.Option{func(o *cluster.Options) { o.Scheme = scheme }},
//		ManagedResources: []schema.GroupVersionKind{{
//			Group: "networking.datumapis.com", Version: "v1alpha", Kind: "LocationBinding",
//		}},
//	})
//
//	// 3. Multicluster manager driven by the provider; engaged clusters are the
//	//    consumer projects using the service.
//	consumerMcMgr, _ := mcmanager.New(baseCfg, provider, mcmanager.Options{Scheme: scheme})
//	_ = (&MyReconciler{}).SetupWithManager(consumerMcMgr) // unchanged reconciler
//
//	// 4. Gate on provider-project readiness, then start all three concurrently.
//	go func() { _ = consumer.WaitProviderProjectReady(ctx, baseCfg, providerProject); _ = providerMgr.Start(ctx) }()
//	go func() { _ = provider.Run(ctx, consumerMcMgr) }()
//	go func() { _ = consumerMcMgr.Start(ctx) }()
//
// ClusterOptions MUST set the scheme or consumer-side caches fall back to the
// client-go global scheme and every consumer-side watch fails with "kind must be
// registered to the Scheme". See the enhancement doc
// docs/enhancements/consumer-project-engagement.md for the normative design.
package consumer
