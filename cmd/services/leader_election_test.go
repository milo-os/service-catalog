// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	ctrl "sigs.k8s.io/controller-runtime"
)

type remoteAddrRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (r *remoteAddrRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.addrs = append(r.addrs, req.RemoteAddr)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (r *remoteAddrRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addrs[len(r.addrs)-1]
}

func restClientFor(t *testing.T, cfg *rest.Config) *rest.RESTClient {
	t.Helper()
	cfg = rest.CopyConfig(cfg)
	cfg.GroupVersion = &schema.GroupVersion{Group: "coordination.k8s.io", Version: "v1"}
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(scheme).WithoutConversion()
	c, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatalf("building REST client: %v", err)
	}
	return c
}

func getLease(ctx context.Context, c *rest.RESTClient) error {
	return c.Get().Namespace("default").Resource("leases").Name("services.miloapis.com").Do(ctx).Error()
}

func TestLeaderElectionRestConfigDoesNotMutateBase(t *testing.T) {
	limiter := flowcontrol.NewTokenBucketRateLimiter(1, 1)
	base := &rest.Config{Host: "https://example.invalid", QPS: 50, Burst: 100, RateLimiter: limiter}

	cfg := leaderElectionRestConfig(base)

	if cfg.RateLimiter != nil {
		t.Errorf("leader election config reuses the base rate limiter")
	}
	if cfg.QPS != leaderElectionQPS || cfg.Burst != leaderElectionBurst {
		t.Errorf("got QPS %v Burst %d, want %v and %d", cfg.QPS, cfg.Burst, leaderElectionQPS, leaderElectionBurst)
	}
	if cfg.Dial == nil {
		t.Errorf("leader election config has no dialer of its own")
	}
	if base.RateLimiter != limiter || base.QPS != 50 || base.Burst != 100 || base.Dial != nil {
		t.Errorf("base config was mutated: %+v", base)
	}
}

func TestLeaderElectionIgnoresSaturatedMainLimiter(t *testing.T) {
	srv := httptest.NewServer(&remoteAddrRecorder{})
	defer srv.Close()

	base := &rest.Config{
		Host:        srv.URL,
		RateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.001, 1),
	}
	mainClient := restClientFor(t, base)

	if err := getLease(context.Background(), mainClient); err != nil {
		t.Fatalf("first request on main client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := getLease(ctx, mainClient); err == nil {
		t.Fatalf("expected the main client to be throttled")
	}

	leaderClient := restClientFor(t, leaderElectionRestConfig(base))
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := getLease(ctx, leaderClient); err != nil {
		t.Fatalf("leader election request was throttled by the main limiter: %v", err)
	}
}

func TestLeaderElectionUsesItsOwnConnection(t *testing.T) {
	rec := &remoteAddrRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	base := &rest.Config{Host: srv.URL, QPS: -1}
	mainClient := restClientFor(t, base)
	ctx := context.Background()

	if err := getLease(ctx, mainClient); err != nil {
		t.Fatalf("main client request: %v", err)
	}
	mainAddr := rec.last()

	if err := getLease(ctx, restClientFor(t, base)); err != nil {
		t.Fatalf("second main client request: %v", err)
	}
	if rec.last() != mainAddr {
		t.Fatalf("clients built from the same config did not share a connection, so this test cannot tell connections apart")
	}

	if err := getLease(ctx, restClientFor(t, leaderElectionRestConfig(base))); err != nil {
		t.Fatalf("leader election request: %v", err)
	}
	if rec.last() == mainAddr {
		t.Errorf("leader election reused the main client's connection %s", mainAddr)
	}
}

func TestElectionOptionsReleaseOnCancelWithAClientOfTheirOwn(t *testing.T) {
	lease, renew, retry := 60*time.Second, 40*time.Second, 5*time.Second
	opts := electionOptions(&rest.Config{Host: "https://example.invalid"}, true, "milo-system", &lease, &renew, &retry)

	if !opts.LeaderElectionReleaseOnCancel {
		t.Errorf("an elected manager keeps its lease on shutdown")
	}
	if opts.LeaderElectionConfig == nil || opts.LeaderElectionConfig.Dial == nil {
		t.Errorf("an elected manager renews its lease without a client of its own")
	}
	if !opts.LeaderElection || opts.LeaderElectionNamespace != "milo-system" ||
		*opts.LeaseDuration != lease || *opts.RenewDeadline != renew || *opts.RetryPeriod != retry {
		t.Errorf("the flags did not reach the options: %+v", opts)
	}
}

func TestWithElectionCopiesEveryLeaderElectionField(t *testing.T) {
	lease, renew, retry := 60*time.Second, 40*time.Second, 5*time.Second
	election := electionOptions(&rest.Config{Host: "https://example.invalid"}, true, "milo-system", &lease, &renew, &retry)

	opts := withElection(ctrl.Options{LeaderElectionID: "services.miloapis.com", Scheme: scheme}, election)

	if opts.LeaderElectionID != "services.miloapis.com" || opts.Scheme != scheme {
		t.Errorf("withElection replaced the manager's own fields: %+v", opts)
	}
	if !opts.LeaderElection || opts.LeaderElectionNamespace != "milo-system" || opts.LeaseDuration != &lease ||
		opts.RenewDeadline != &renew || opts.RetryPeriod != &retry {
		t.Errorf("withElection dropped a flag: %+v", opts)
	}
	if !opts.LeaderElectionReleaseOnCancel || opts.LeaderElectionConfig != election.LeaderElectionConfig {
		t.Errorf("withElection dropped the release or the lease client: %+v", opts)
	}
}
