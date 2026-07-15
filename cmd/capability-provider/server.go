// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// bindingLister returns the AgentBindings visible in the backing project
// control plane. On the shared kind cluster there is exactly one such control
// plane (the engaged consumer project), so the real implementation lists all
// cluster-scoped AgentBindings; tests inject a fake.
type bindingLister interface {
	ListBindings(ctx context.Context) ([]servicesv1alpha1.AgentBinding, error)
}

// handler serves the assistant's capability-provider contract.
//
// GET /projects/{project}/capability-documents returns the JSON array of
// capability documents entitling {project}. Because every catalog kind is
// cluster-scoped, one kind cluster models one project's control plane
// (consumerProject). A request for any other project name returns an empty
// array: that project has no entitlement here, which is exactly the isolation
// the real multi-control-plane platform enforces (proof P5).
type handler struct {
	lister          bindingLister
	consumerProject string
	rewrite         rewriteConfig
	logger          *slog.Logger
}

func newHandler(lister bindingLister, consumerProject string, rw rewriteConfig, logger *slog.Logger) *handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &handler{lister: lister, consumerProject: consumerProject, rewrite: rw, logger: logger}
}

func (h *handler) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /projects/{project}/capability-documents", h.listDocuments)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", h.readyz)
	return mux
}

func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	// Readiness proves the apiserver connection works: a bad kubeconfig or a
	// missing AgentBinding CRD surfaces here rather than as an empty 200 on
	// the documents endpoint.
	if _, err := h.lister.ListBindings(r.Context()); err != nil {
		h.logger.Warn("capability.readyz.list_failed", "error", err.Error())
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *handler) listDocuments(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")

	// Documents is always a non-nil slice so the body is "[]" (never "null")
	// for an unentitled project — the assistant parses either, but "[]" is
	// the honest empty answer.
	documents := []capabilityDocument{}

	if project == h.consumerProject {
		bindings, err := h.lister.ListBindings(r.Context())
		if err != nil {
			h.logger.Error("capability.list.failed", "project", project, "error", err.Error())
			http.Error(w, "failed to list agent bindings", http.StatusBadGateway)
			return
		}
		// Deterministic order so repeated fetches and golden tests are stable.
		sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
		for i := range bindings {
			documents = append(documents, translateBinding(&bindings[i], project, h.rewrite))
		}
	}

	h.logger.Info("capability.list.served", "project", project, "documents", len(documents))
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(documents); err != nil {
		h.logger.Error("capability.encode.failed", "error", err.Error())
	}
}
