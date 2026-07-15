# Agent-Framework Playground — Catalog Overlay

The catalog side of the real-environment playground (contract slice 6,
components 4–5): the service-catalog CRDs + controllers, admission webhooks
through the cluster's cert-manager, the StreamCo demo CRs, and the
**capability-provider adapter** that serves the assistant's capability-document
contract over HTTP.

This is the `--with-catalog` overlay. It installs on top of the shared
`test-infra` kind cluster additively — every resource is labeled
`app.kubernetes.io/part-of=agent-framework-playground` — and never touches the
cluster's other workloads or deletes the cluster.

## What gets installed

| Piece | Kind | Notes |
|---|---|---|
| Catalog CRDs (7) | CustomResourceDefinition | services, serviceconfigurations, serviceconsumers, serviceentitlements, serviceagents, serviceagentconfigurations, agentbindings |
| `playground-manager` | Deployment | the catalog reconcilers (Service, ServiceEntitlement, ServiceAgent, ServiceAgentConfiguration, AgentBinding) + admission webhooks |
| `capability-provider` | Deployment + Service | the adapter; serves `GET /projects/{name}/capability-documents` on :8080 |
| serving cert | cert-manager Certificate/Issuer | issues the webhook TLS cert |

Every catalog kind is cluster-scoped, so on one kind cluster the cluster models
**one consumer project's control plane**. The manager engages it under the
project name `demo-project` (provider alias `streamco-platform`), mirroring the
agent-framework e2e static-provider harness.

## Deploy

```bash
# Brings up everything and applies the StreamCo demo CRs (idempotent).
hack/playground/catalog-up.sh

# Point the adapter at the real Envoy AI Gateway MCPRoute (optional; there is a
# placeholder default). pg-infra provides the URL.
MCP_GATEWAY_BASE_URL=http://ai-gateway.envoy-gateway-system.svc:80/mcp \
  hack/playground/catalog-up.sh
```

Teardown removes exactly the labeled resources (never the cluster):

```bash
hack/playground/catalog-down.sh --dry-run   # list what would go (proof P8)
hack/playground/catalog-down.sh             # delete them
```

## Try it as a consumer

```bash
# The document set entitling demo-project (MCP endpoints rewritten onto the gateway).
hack/playground/demo/fetch-capabilities.sh demo-project

# Isolation: any other project has no entitlement here -> [] (proof P5).
hack/playground/demo/fetch-capabilities.sh other-project

# Live reconfiguration (proof P3): apply a narrower ServiceAgentConfiguration
# and watch the served capabilities shrink on the next fetch.
hack/playground/demo/reconfigure.sh            # apply v2 (streams_list only)
hack/playground/demo/reconfigure.sh --revert   # deprecate v2 -> falls back to v1
hack/playground/demo/reconfigure.sh --unpublish # delete entitlement -> capabilities gone
```

## Capability-provider API (published contract)

The adapter is the concrete "capability provider API" the Go-port contract
promised. The assistant's `HTTPCapabilitySource` is its client. **The assistant
owns the capability-document schema (`internal/capability`); this adapter
conforms to it.**

```
GET /projects/{project}/capability-documents  ->  200 application/json
```

Returns a JSON array of capability documents entitling `{project}`. A project
with no entitlement (any name other than the backing consumer project) returns
`[]` — the isolation boundary. `/healthz` and `/readyz` are also served;
`/readyz` fails until the AgentBinding list succeeds against the apiserver.

Each element is one `AgentBinding` projection, translated field-by-field:

```jsonc
{
  "apiVersion": "services.miloapis.com/v1alpha1",
  "kind": "AgentBinding",
  "metadata": { "name": "<binding>", "namespace": "<project>" },
  "spec": {
    "serviceRef": { "name": "streaming-streamco-example" },
    "serviceName": "streaming.streamco.example",     // required
    "serviceAgentRef": { "name": "streamco-streaming-agent" },
    "configurationVersion": "1.0.0",                  // required
    "knowledge": {
      "sources":  [ { "type": "LLMDocs|Runbook|Markdown", "title": "...", "url": "..." } ],
      "concepts": [ { "gvk": { "group": "...", "kind": "..." }, "summary": "..." } ]
    },
    "tools": {
      "mcpServers": [ {
        "name": "streamco",
        "endpoint": "http://ai-gateway.../mcp/streamco", // REWRITTEN onto the gateway
        "toolSelector": { "include": ["streams_list", "..."] },
        "mutating": []
      } ]
    },
    "authority": {
      "reads": [ { "gvk": { "group": "...", "kind": "*" } } ],
      "maxTaskDurationSeconds": 120
    }
  }
}
```

Required fields (mirrors the assistant's `CapabilityDocument.Validate`):
`spec.serviceName`, `spec.serviceRef.name`, `spec.serviceAgentRef.name`,
`spec.configurationVersion`, and `name`+`endpoint` on every MCP server.

### MCP endpoint rewrite

The provider declares a raw MCP endpoint on its ServiceAgentConfiguration;
consumers must never dial it directly. The adapter overwrites every
`tools.mcpServers[].endpoint` onto the gateway MCPRoute so tool traffic is
metered and project-attributed. Config (Deployment env):

| Env | Default | Meaning |
|---|---|---|
| `CONSUMER_PROJECT` | `demo-project` | the backing consumer project this control plane models |
| `MCP_GATEWAY_BASE_URL` | (placeholder) | gateway MCPRoute base; empty disables rewrite (pass through) |
| `MCP_GATEWAY_APPEND_SERVER_NAME` | `true` | append `/<serverName>`; set false for a single-endpoint gateway |
| `LISTEN_ADDR` | `:8080` | HTTP bind address |

## Proof map

| Proof | Covered by |
|---|---|
| P3 live reconfiguration | `demo/reconfigure.sh` — apply v2, binding reprojects, served capabilities narrow; `--unpublish` empties them |
| P5 entitlement isolation | `demo/fetch-capabilities.sh other-project` -> `[]` |
| P7 suites green | `go test ./...` (catalog envtest + adapter unit tests) |
| P8 down = exactly our resources | `catalog-down.sh --dry-run` |

Gateway token attribution (P4), sink usage events (P6), and the host-side chat
(P2) live on the BASE overlay (assistant + gateway + StreamCo), owned by
infra/assistant/qa.
