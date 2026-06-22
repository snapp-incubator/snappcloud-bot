# snappcloud-bot

The SnappCloud Mattermost bot. An authenticated Mattermost user chats with it;
the bot checks the user's authorization and, if allowed, forwards the query to a
[Dify](https://dify.ai) workflow whose agent drives per-cluster MCP servers.

Authorization is delegated to [mcp-authz](../mcp-authz) — one instance per
cluster. The bot holds **no cluster credentials**: it calls every region's
mcp-authz API, aggregates the per-cluster namespace scope, and passes it to Dify.

```
Mattermost user
   │  message (WebSocket)
   ▼
snappcloud-bot ── resolve SSO email
   │  GET /v1/namespaces?user=<email>  (bearer) ─▶ mcp-authz (every region, concurrent)
   │  aggregate Scope{cluster: [namespaces]}; cache 5m per user
   ▼  if non-empty
Dify workflow  (inputs.allowed_namespaces = cluster-qualified scope)
   ▼
answer ─▶ Mattermost
```

## Behavior

- **Identity.** Resolves the sender's OpenShift/SSO **email** via the Mattermost
  API (`/api/v4/users/{id}`). `identityMap` can override email → username.
- **Authorization.** Calls every `authz.regions[]` mcp-authz endpoint. A user
  with no namespaces on any cluster never reaches Dify (hard gate). Per-region
  fail-closed: a region that errors is omitted; only if **all** error does the
  bot report "temporarily unavailable".
- **Scope to Dify.** `allowed_namespaces` is a cluster-qualified block the agent
  must obey:
  ```
  okd4-teh-1: team-a, team-b
  okd4-ts-2: team-c
  ```
- **Caching.** Each user's aggregated scope is cached (default 5m, singleflight
  collapse + background sweep).
- **Where it answers.** Direct messages always; channels only when @-mentioned
  (`requireMention`, default true). The bot must be a channel member.
- **Singleton.** A WebSocket bot is one listener per Mattermost — run a single
  replica on a single cluster.

## Configuration

See [`config.example.yaml`](config.example.yaml): `mattermost`, `dify`, and
`authz` (the per-region mcp-authz endpoints + cache TTL). Secrets are read from
the environment, never YAML:

| Env | Purpose |
|-----|---------|
| `MATTERMOST_TOKEN` | bot account token |
| `DIFY_API_KEY`     | Dify app API key |
| `MCP_AUTHZ_TOKEN`  | bearer presented to every mcp-authz (shared secret) |

Each region name in `authz.regions[]` must match the per-cluster MCP tool group
in the Dify workflow.

## Develop

```bash
make build   # binary -> bin/snappcloud-bot
make test    # unit tests
make run     # run with config.example.yaml
make docker  # multi-arch image via build/package/docker-bake.json
```

## Deploy

Helm chart: `core/helm/apps/snappcloud-bot`. Singleton (`replicas: 1`,
`Recreate`), no inbound Service/ingress, no cluster RBAC. Registered in the
bootstrap `primary_apps` so it runs on one cluster. Ships Deployment, ConfigMap,
ServiceAccount, and a Secret with the three tokens (from the encrypted
`secrets-<region>.yaml`).
