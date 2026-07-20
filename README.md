# snappcloud-bot

The SnappCloud Mattermost bot. An authenticated user chats with it; the bot
resolves the user's authorization and runs an **in-process MCP agent** that
drives the per-cluster MCP servers (Kubernetes/OpenShift, Cilium/Hubble,
Envoy/Contour, docs) with a reasoning model — investigating workloads (pods,
crashes, rollouts, quotas, logs, events) and networking (flows, drops, ingress,
policy) across clusters in a single loop, while enforcing namespace scope on
every tool result.

Authorization is delegated to [mcp-authz](../mcp-authz) — one instance per
cluster. The bot holds **no cluster credentials**.

```
Mattermost user ── message (WebSocket)
        ▼
snappcloud-bot ── resolve SSO email
        │  scope = mcp-authz(every region): cluster -> {namespaces, clusterWide}
        │          (groups-aware SAR, admin fast-path, cached 5m)
        ▼  if authorized somewhere
   agent loop (streaming reasoning model, all authorized clusters at once):
        │  model proposes cluster-tagged tool calls (in parallel)
        │  ── infra tool (nodes/BGP/agent status)?
        │        cluster-admin → run, return unfiltered
        │        otherwise    → denied ("requires cluster-admin")
        │  ── tenant tool → MCP call → FILTER result vs the cluster's namespaces
        │        drop records in unauthorized namespaces; resolve bare IPs via
        │        mcp-authz /v1/resolve (fail-closed); cap oversized output
        │  ── feed only authorized data back
        ▼
   answer ── Mattermost (threaded in channels, split if long)
```

## Enforcement (why the model can't leak)

MCP tools take pods/IPs/services, not namespaces — the namespace lives in the
**result data**. So the bot filters every tenant-data tool result before the
model sees it: a record referencing a namespace the user can't access is
dropped; a bare IP is resolved to its namespace via mcp-authz and gated; if
resolution is unavailable the result is withheld (**fail-closed**). The model
only ever receives authorized data — authorization is not the model's job, and
the prompt requires withheld data to be reported as an access limitation, never
as "does not exist".

Two exemption classes:
- **Cluster-infrastructure tools** (`toolRules.<tool>.clusterAdminOnly`): nodes,
  BGP state, agent status. Denied outright for non-admins; returned unfiltered
  for callers whose cluster-wide SAR passed (`clusterWide` in the mcp-authz
  scope response). Deterministic RBAC, per cluster.
- **Global servers** (the general docs): namespace-agnostic, available to any
  authorized user, not scope-filtered.

## Behavior

- **Identity.** Sender's SSO **email** via the Mattermost API; `identityMap` can
  override email → username.
- **Authorization.** A user with no namespaces on any cluster never reaches the
  MCP servers (hard gate). Group-aware SARs; per-region fail-closed.
- **Multi-cluster.** Every authorized cluster's tools are exposed at once, tagged
  `[cluster X]`; the agent calls the right cluster and combines across clusters.
- **Thorough tool use.** The system prompt pushes the model to investigate with
  every relevant tool (pods + logs + events + flows + policy + ingress) and
  reconcile them. Extend with your own MCP "skills" via `agent.toolGuidance`.
- **Access refresh.** Scope is cached per user (`authz.cacheTTL`, default 5m). A
  user whose authorization just changed can say **"refresh"** to flush their own
  cache and get their live cluster/namespace list immediately — no wait, no
  restart. Lower `cacheTTL` for faster automatic propagation (more mcp-authz load).
- **Memory.** Per Mattermost thread (and each DM), a transcript is kept and
  replayed for context; persisted to a file (`memory.memoryPath`, a PVC) so it
  survives restarts.
- **Replies.** Channels: in-thread, only when @-mentioned. DMs: always. Typing
  indicator while working; long answers split transparently.
- **Singleton.** One WebSocket listener — a single replica on a single cluster.

## Reliability

- **Streaming LLM (SSE)** with retries: every text/tool-use delta is accumulated,
  so no part of a long answer is lost; if the stream ends before completion it is
  **retried** (never returns partial). Transient failures (network, `429`, `5xx`)
  retry with backoff+jitter; `4xx` do not. Falls back to a non-streaming JSON
  body if the endpoint ignores `stream:true`. HTTP/2 keep-alive transport.
- **Bounded prompts.** A single tool result is capped (~100k chars, applied after
  filtering) so a verbose dump cannot blow the model's request budget; empty
  content blocks are normalized (the API rejects them).
- **MCP mux** skips a dead server (best-effort tool listing); a cluster with no
  reachable servers is dropped, not fatal. SSE responses up to 32 MiB per line.

## Configuration

See [`config.example.yaml`](config.example.yaml). Secrets are read from the
environment (never YAML):

| Env | Purpose |
|-----|---------|
| `MATTERMOST_TOKEN` | bot account token |
| `LLM_API_KEY`      | `x-api-key` for the Anthropic-style endpoint |
| `MCP_AUTHZ_TOKEN`  | bearer to every mcp-authz |
| `<per-server>`     | Authorization header for an authed MCP server (e.g. `CILIUM_TEH1_AUTH`) |

Key config sections: `agent.llm` (endpoint/model), `agent.clusters[].servers[]`
(MCP servers per cluster), `agent.globalServers[]` (namespace-agnostic servers
like docs), `agent.toolGuidance` (tool-usage skills), `agent.toolRules`
(per-tool namespace-arg overrides + `clusterAdminOnly` infra gating),
`authz.regions[]` (mcp-authz endpoints). A cluster's `name` must match an
`authz.regions[].name`.

### Adding a new MCP server

Append one entry under the cluster — no code change:

```yaml
agent:
  clusters:
    - name: okd4-teh-1
      servers:
        - url: https://hubble-mcp.apps.private.okd4.teh-1.snappcloud.io/mcp
          authHeaderEnv: HUBBLE_TEH1_AUTH   # only if it needs auth (per region)
```

If authed, add the key to the `snappcloud_bot.mcpAuth` secret (the full
`Authorization` header). MCP Basic auth is **per region** — one key each.
Its tools appear automatically, cluster-tagged and enforced. A namespace-agnostic
server (docs) goes under `agent.globalServers`; cluster-infrastructure tools it
exposes should be listed in `agent.toolRules` with `clusterAdminOnly: true`.

## Develop

```bash
make build   # binary -> bin/snappcloud-bot
make test
make run      # config.example.yaml
make docker   # multi-arch via build/package/docker-bake.json
```

## Deploy

Helm chart: `core/helm/apps/snappcloud-bot`. Singleton (`replicas: 1`,
`Recreate`), no inbound Service, no cluster RBAC. Ships Deployment, ConfigMap
(the `config` values → `config.yaml`), ServiceAccount, a Secret (all keys exposed
as env via `envFrom` — including per-region `mcpAuth` entries), and a **PVC** for
conversation memory. Secrets are grouped under the `snappcloud_bot` sops key; the
shared `mcp_authz.authToken` sops key is read by both this chart and mcp-authz so
the bearer can never drift.
