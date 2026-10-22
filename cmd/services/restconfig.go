// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"k8s.io/client-go/rest"

	"go.miloapis.com/service-catalog/internal/clientmetrics"
	"go.miloapis.com/service-catalog/internal/config"
)

// miloRestConfig loads the rest config every Milo client is built from and
// reports its client-side rate limit. Anything that changes the config's rate
// limit must happen before the report; otherwise the dashboard shows a limit
// the clients do not run with. TestMiloRestConfigReportsTheLimitClientsUse
// pins that order.
func miloRestConfig(serverConfig *config.ServicesOperator) (*rest.Config, error) {
	cfg, err := serverConfig.RestConfig()
	if err != nil {
		return nil, err
	}
	clientmetrics.ReportRateLimit(cfg)
	return cfg, nil
}
