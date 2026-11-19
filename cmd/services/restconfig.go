// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"k8s.io/client-go/rest"

	"go.miloapis.com/service-catalog/internal/clientmetrics"
	"go.miloapis.com/service-catalog/internal/config"
)

// miloRestConfig loads the rest config every Milo client is built from,
// applies the client rate limits, and reports them. Every manager, the Milo
// providers, and each project control plane's config derive from the result,
// so the limits must be set before anything copies it, and reported only after
// they are set; otherwise the dashboard shows a limit the clients do not run
// with. TestMiloRestConfigReportsTheLimitClientsUse pins that order.
func miloRestConfig(serverConfig *config.ServicesOperator, qps float32, burst int) (*rest.Config, error) {
	cfg, err := serverConfig.RestConfig()
	if err != nil {
		return nil, err
	}
	setClientRateLimits(cfg, qps, burst)
	clientmetrics.ReportRateLimit(cfg)
	return cfg, nil
}
