// SPDX-License-Identifier: AGPL-3.0-only

package main

import "k8s.io/client-go/rest"

// Client-side rate limits for requests to Milo. controller-runtime builds one
// REST client, and so one limiter, per kind per project control plane, so each
// limit applies to one kind in one project rather than to the operator as a
// whole.
//
// Without these, a config loaded from kubeconfigPath falls back to client-go's
// 5 requests per second with a burst of 10. One activation writes status to
// its ServiceEntitlements dozens of times in under a second, and at that limit
// those writes alone held setup for about three seconds. The defaults leave a
// single activation, and several at once, inside the burst, while still
// stopping a controller that loops on one project from flooding Milo.
const (
	defaultKubeAPIQPS   float32 = 50
	defaultKubeAPIBurst int     = 100
)

// setClientRateLimits applies the --kube-api-qps and --kube-api-burst limits
// to cfg and to every config copied from it. A negative QPS turns client-side
// throttling off and leaves the API server's priority and fairness to protect
// it.
func setClientRateLimits(cfg *rest.Config, qps float32, burst int) {
	cfg.QPS = qps
	cfg.Burst = burst
	cfg.RateLimiter = nil
}
