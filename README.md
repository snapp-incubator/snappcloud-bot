# snappcloud-bot

The SnappCloud Mattermost bot. An authenticated user chats with it; the bot
resolves the user's authorization and runs an **in-process MCP agent** that
drives the per-cluster MCP servers (Kubernetes/OpenShift, Cilium/Hubble,
Envoy/Contour, docs) with a reasoning model — investigating workloads (pods,
crashes, rollouts, quotas, logs, events) and networking (flows, drops, ingress,
policy) across clusters in a single loop, while enforcing namespace scope on
every tool result.

Authorization is delegated to [mcp-authz](https://github.com/snapp-incubator/mcp-authz) — one instance per
cluster. The bot holds **no cluster credentials**.

```
Mattermost user ── message (WebSocket)
        ▼
snappcloud-bot ── resolve SSO email
        │  scope = mcp-authz(every region): cluster -> {namespaces, clusterWide}
        │          (groups-aware SAR, admin fast-path, cached — authz.cacheTTL)
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

Three exemption classes:
- **Cluster-infrastructure tools** (`toolRules.<tool>.clusterAdminOnly`): nodes,
  BGP state, agent status. Denied outright for non-admins; returned unfiltered
  for callers whose cluster-wide SAR passed (`clusterWide` in the mcp-authz
  scope response). Deterministic RBAC, per cluster.
- **Self-authorized servers** (`servers[].selfAuthorized`): trusted,
  identity-aware MCP servers (e.g. argocd-mcp). The caller's SSO identity is
  forwarded as the `X-Remote-User` header and the server authorizes the caller
  itself, so the bot skips namespace filtering and returns results unfiltered —
  trusting it the same way it trusts mcp-authz. The identity comes from the
  authenticated Mattermost user (never a tool argument, so the model can't spoof
  it); a request with no identity is refused, never sent unscoped (fail-closed).
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
- **Access refresh.** Scope is cached per user (`authz.cacheTTL`). A
  user whose authorization just changed can say **"refresh"** to flush their own
  cache and get their live cluster/namespace list immediately — no wait, no
  restart. Lower `cacheTTL` for faster automatic propagation (more mcp-authz load).
- **Schedules.** A user can save a recurring query — `schedule every day at
  09:00 are any pods failing in my-ns?` — plus `schedules` to list and
  `unschedule <id>` to remove. An interval schedule can name its first run
  (`every 4h starting at 16:10 ...`). Times are read and displayed in
  `schedules.timezone`, not the pod's zone. Bounded by `schedules.perUser` (5) / `total`
  (500) / `minInterval` (4h) — `total ÷ minInterval` is the worst-case hourly
  load — run by a small worker pool (`concurrency`) so recurring work never
  crowds out interactive users, and disabled automatically after repeated
  failures. **Authorization is resolved at run time, never stored**: a schedule
  cannot outlive the access it was created with.
- **Memory.** Per Mattermost thread (and each DM), a transcript is kept and
  replayed for context; persisted to a file (`memory.memoryPath`, a PVC) so it
  survives restarts.
- **Replies.** Channels: in-thread, only when @-mentioned. DMs: always. Typing
  indicator while working; long answers split transparently.
- **Mentions from other bots.** The mention is detected from the event's mention
  list *or* from the message text, because Mattermost does not populate mention
  metadata for posts authored by other bots, webhooks, and integrations. An
  ignored channel message is logged at debug and counted as
  `messages_total{outcome="ignored"}`. A calling bot still needs an identity the
  platform can authorize: bot accounts usually have no email, so map theirs with
  `mattermost.identityMap` (`"" -> username` is not possible; give the bot
  account an email or map it) — otherwise the call is refused as unauthorized
  and logged as `no identity for sender`.
- **Singleton.** One WebSocket listener — a single replica on a single cluster.

## Reliability

- **Streaming LLM (SSE)** with retries: every text/tool-use delta is accumulated,
  so no part of a long answer is lost; if the stream ends before completion it is
  **retried** (never returns partial). Transient failures (network, `429`, `5xx`)
  retry with backoff+jitter; `4xx` do not. Falls back to a non-streaming JSON
  body if the endpoint ignores `stream:true`. HTTP/2 keep-alive transport.
- **Model failover.** An optional backup model (`agent.fallbackLLM`) behind a
  circuit breaker: after `failureThreshold` consecutive primary failures traffic
  moves to the backup for `cooldownPeriod`, then the primary is probed once and
  reinstated if healthy. The failing request itself is retried on the backup, so
  users still get an answer. A single primary success resets the counter.
  Watch `snappcloud_bot_llm_model_requests_total{role}` and
  `snappcloud_bot_llm_failover_total{state}`.
- **Bounded prompts.** A single tool result is capped (~100k chars, applied after
  filtering) so a verbose dump cannot blow the model's request budget; empty
  content blocks are normalized (the API rejects them).
- **MCP mux** skips a dead server (best-effort tool listing); a cluster with no
  reachable servers is dropped, not fatal. SSE responses up to 32 MiB per line.
- **Crash isolation.** Each message is handled in its own goroutine with a panic
  recover, so one bad message can never take the singleton process down.
- **Abuse guards** (`limits`): per-user token-bucket rate limit
  (`ratePerMin`/`rateBurst`) and a max query length (`maxQueryRunes`). Both
  protect the LLM budget and the downstream MCP servers from a single client.

## HTTP query API

The same enforced agent loop, callable outside Mattermost (`api.enabled`, port
`8081` by default). Callers authenticate with **their own OpenShift token** — a
user token or a **ServiceAccount** token:

```bash
TOKEN=$(oc whoami -t)                     # or: oc create token my-sa -n my-ns
BOT=https://snappcloud-bot.apps.private.okd4.teh-1.snappcloud.io

curl -sS -H "Authorization: Bearer $TOKEN" "$BOT/v1/whoami"

curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"query":"why are pods in my-ns crashing on okd4-teh-1?"}' "$BOT/v1/query"
```

| Endpoint | Purpose |
|---|---|
| `POST /v1/query` | `{"query": "...", "history": "..."}` → `{"answer", "user", "requestId"}` |
| `GET /v1/whoami` | verify a token and see the scope it grants (no agent run) |

The bot holds no cluster credentials, so the token is verified by **mcp-authz
via TokenReview** (`POST /v1/authenticate`, requires `create` on
`authentication.k8s.io/tokenreviews`). Every region is probed; the cluster that
issued the token authenticates it. The resulting identity — including a
ServiceAccount's `system:serviceaccounts:*` groups — is scoped and filtered
exactly like a Mattermost user's, so an API caller can never see more than they
can with `oc`. No token is ever logged or stored. Rate limiting is shared with
Mattermost, so one identity has a single budget across both entrypoints.

## Metrics

Prometheus metrics on `/metrics` (same port as the health probes), scraped via
the chart's ServiceMonitor. Labels are deliberately low-cardinality — bounded
enums only, never a user identity, namespace, or free text.

| Metric | Labels | Use |
|---|---|---|
| `snappcloud_bot_api_requests_total` | `outcome` | HTTP query API traffic |
| `snappcloud_bot_messages_total` | `outcome` | answered / denied / rate_limited / agent_error / … |
| `snappcloud_bot_message_duration_seconds` | — | end-to-end answer latency |
| `snappcloud_bot_tool_calls_total` | `cluster`, `tool`, `outcome` | per-tool call rate |
| `snappcloud_bot_tool_errors_total` | `cluster`, `tool`, `reason` | which MCP tool is broken, and why (`timeout`, `unreachable`, `auth`, `not_found`, `bad_args`, `server_error`) |
| `snappcloud_bot_tool_call_duration_seconds` | `cluster` | slow MCP servers |
| `snappcloud_bot_llm_requests_total` / `_duration_seconds` | `outcome` | model health |
| `snappcloud_bot_authz_requests_total` / `_duration_seconds` | `region`, `outcome` | per-region mcp-authz health |
| `snappcloud_bot_schedules`, `_schedule_owners`, `_schedule_limit` | — | stored recurring queries, distinct owners, configured ceiling |
| `snappcloud_bot_schedule_runs_total` | `outcome` | scheduled runs: `ok` / `error` / `skipped` (owner lost access) |
| `snappcloud_bot_schedule_run_duration_seconds`, `_schedule_runs_in_flight` | — | scheduled run latency and worker-pool saturation |
| `snappcloud_bot_schedules_disabled_total` | — | schedules dropped after repeated failures |
| `snappcloud_bot_active_conversations`, `_messages_in_flight`, `_handler_panics_total` | — | live state |

Dashboard: `core/dashboards/Network/SnappCloudBot`. Alerts:
`core/helm/apps/monitoring-int/templates/rules/snappcloud-bot.yaml`.

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

For a trusted, identity-aware server that authorizes the caller itself (e.g.
argocd-mcp), set `selfAuthorized: true` on the server entry. The caller's SSO
identity is then forwarded as `X-Remote-User` and all of that server's tools skip
the bot's namespace enforcement, returning results unfiltered:

```yaml
agent:
  clusters:
    - name: okd4-teh-1
      servers:
        - url: https://argocd-mcp.apps.private.okd4.teh-1.snappcloud.io/mcp
          selfAuthorized: true   # forwards X-Remote-User; server scopes the result
```

Enable it only for servers you trust to enforce the caller's access from the
forwarded identity — it bypasses the bot's own filtering.

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
