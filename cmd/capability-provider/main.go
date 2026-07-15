// SPDX-License-Identifier: AGPL-3.0-only

// capability-provider is the playground's catalog-side adapter: it lists the
// AgentBindings the services operator projects into the demo consumer project
// and serves them over HTTP in the assistant's capability-document shape,
// rewriting every provider MCP endpoint onto the Envoy AI Gateway MCPRoute so
// tool traffic is metered and project-attributed.
//
// It is the concrete "capability provider API" the Go-port contract promised;
// the assistant's HTTPCapabilitySource is its client. The wire schema is owned
// by the assistant (internal/capability) — this binary conforms to it.
//
// Config (flags, each with an env fallback so the Deployment can set them):
//
//	--listen-addr             LISTEN_ADDR                 (:8080)
//	--consumer-project        CONSUMER_PROJECT            (demo-project)
//	--mcp-gateway-base-url     MCP_GATEWAY_BASE_URL        ("" = pass through)
//	--mcp-gateway-append-name  MCP_GATEWAY_APPEND_SERVER_NAME (true)
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	listenAddr := flagOrEnv("listen-addr", "LISTEN_ADDR", ":8080", "address the HTTP server binds to")
	consumerProject := flagOrEnv("consumer-project", "CONSUMER_PROJECT", "demo-project",
		"the backing consumer project name this control plane models")
	gatewayBaseURL := flagOrEnv("mcp-gateway-base-url", "MCP_GATEWAY_BASE_URL", "",
		"Envoy AI Gateway MCPRoute base URL to rewrite provider MCP endpoints onto (empty = pass through)")
	appendServerName := flagBoolOrEnv("mcp-gateway-append-name", "MCP_GATEWAY_APPEND_SERVER_NAME", true,
		"append /<serverName> to the gateway base URL per MCP server")
	flag.Parse()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(servicesv1alpha1.AddToScheme(scheme))

	cfg, err := ctrl.GetConfig()
	if err != nil {
		logger.Error("capability.config.load_failed", "error", err.Error())
		os.Exit(1)
	}

	// A direct (uncached) client is deliberate: the assistant fetches
	// per-conversation, so QPS is low and every request must reflect the
	// live projection (proof P3 flips an AgentBinding and expects the very
	// next fetch to change). No informer, no cache, no stale reads.
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		logger.Error("capability.client.build_failed", "error", err.Error())
		os.Exit(1)
	}

	rw := rewriteConfig{GatewayBaseURL: *gatewayBaseURL, AppendServerName: *appendServerName}
	h := newHandler(&clientLister{client: c}, *consumerProject, rw, logger)

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           h.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("capability.starting",
		"listenAddr", *listenAddr,
		"consumerProject", *consumerProject,
		"gatewayBaseURL", *gatewayBaseURL,
		"appendServerName", *appendServerName,
	)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("capability.server.failed", "error", err.Error())
			os.Exit(1)
		}
	}()

	ctx := ctrl.SetupSignalHandler()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("capability.server.shutdown_error", "error", err.Error())
	}
	logger.Info("capability.stopped")
}

// clientLister is the production bindingLister: a cluster-scoped list of every
// AgentBinding the operator has projected.
type clientLister struct {
	client client.Client
}

func (l *clientLister) ListBindings(ctx context.Context) ([]servicesv1alpha1.AgentBinding, error) {
	var list servicesv1alpha1.AgentBindingList
	if err := l.client.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func flagOrEnv(name, envKey, def, usage string) *string {
	if v, ok := os.LookupEnv(envKey); ok {
		def = v
	}
	return flag.String(name, def, usage)
}

func flagBoolOrEnv(name, envKey string, def bool, usage string) *bool {
	if v, ok := os.LookupEnv(envKey); ok {
		if parsed, err := strconv.ParseBool(v); err == nil {
			def = parsed
		}
	}
	return flag.Bool(name, def, usage)
}
