// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"net"
	"time"

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

// electionOptions returns the leader-election fields every elected manager in
// this process shares: the flags, a lease client of its own, and release on
// shutdown. The lease client keeps renewals off the connection the rest of the
// API traffic uses, so a stalled connection cannot cost the lease. Releasing
// is safe because runUntilManagerStops ends the process only after every
// manager has stopped.
func electionOptions(cfg *rest.Config, enabled bool, namespace string, lease, renew, retry *time.Duration) ctrl.Options {
	return ctrl.Options{
		LeaderElection:                enabled,
		LeaderElectionNamespace:       namespace,
		LeaseDuration:                 lease,
		RenewDeadline:                 renew,
		RetryPeriod:                   retry,
		LeaderElectionConfig:          leaderElectionRestConfig(cfg),
		LeaderElectionReleaseOnCancel: true,
	}
}

// withElection returns opts with election's leader-election fields; opts keeps
// its own lease ID.
func withElection(opts ctrl.Options, election ctrl.Options) ctrl.Options {
	opts.LeaderElection = election.LeaderElection
	opts.LeaderElectionNamespace = election.LeaderElectionNamespace
	opts.LeaseDuration = election.LeaseDuration
	opts.RenewDeadline = election.RenewDeadline
	opts.RetryPeriod = election.RetryPeriod
	opts.LeaderElectionConfig = election.LeaderElectionConfig
	opts.LeaderElectionReleaseOnCancel = election.LeaderElectionReleaseOnCancel
	return opts
}

const (
	leaderElectionQPS   = 5
	leaderElectionBurst = 10
)

func leaderElectionRestConfig(base *rest.Config) *rest.Config {
	cfg := rest.CopyConfig(base)
	cfg.RateLimiter = nil
	cfg.QPS = leaderElectionQPS
	cfg.Burst = leaderElectionBurst
	cfg.Dial = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	return cfg
}
