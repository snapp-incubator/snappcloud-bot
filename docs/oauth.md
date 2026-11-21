# Per-user OAuth for MCP

Both modes are opt-in. Installations running both integrations use separate bot deployments. The main bot serves AIon requests. The dedicated Pardis bot connects individual Mattermost users to Pardis MCP.

## AIon calls the main bot

Enable the query API and configure the resource server with your issuer's introspection endpoint. Example values use a sample domain.

```yaml
api:
  enabled: true
aionOAuth:
  resource: https://bot.example.org/oauth/mcp
  issuer: https://sso.example.org/realms/staging
  introspectionURL: https://sso.example.org/realms/staging/protocol/openid-connect/token/introspect
  clientID: bot-introspection
  scope: mcp:tools
```

Set `AION_INTROSPECTION_SECRET` for HTTP Basic authentication at the introspection endpoint. Register the AIon gateway with the issuer as an authorization-code client with PKCE, its exact callback URI and the requested scope. Configure the full resource URL as the token audience. The protected-resource metadata links to the issuer, which must publish OAuth or OIDC discovery metadata.

Point the staging connector at the resource URL with `upstreamAuthType: oauth`. Each MCP POST requires an active, unexpired bearer access token for this resource and scope. Introspection must return the configured issuer, subject, verified email and token type. Use directory-managed email addresses. The bot maps email using the existing Mattermost identity map and resolves cluster access with mcp-authz. Test `whoami` and `ask` as two users with different grants. OpenShift clients continue to use `/mcp`.

## The Pardis bot calls the user's MCP server

Register the bot at the Pardis issuer for the authorization-code and refresh-token grants, scope `mcp:tools`, and the exact callback URI. Copy the endpoints from the issuer's metadata. Public clients use PKCE S256. For a confidential client registered with `client_secret_basic`, also set `PARDIS_OAUTH_CLIENT_SECRET`.

```yaml
pardisOAuth:
  resource: https://pardis.example.org/mcp
  authorizeURL: https://pardis.example.org/authorize
  tokenURL: https://pardis.example.org/token
  revokeURL: https://pardis.example.org/revoke
  callbackURL: https://bot.example.org/oauth/callback
  clientID: registered-client-id
  storeDir: /data/oauth
```

Set `PARDIS_OAUTH_STORAGE_KEY` to a base64-encoded 32-byte random key. Persist the storage directory on a private volume for the single bot replica. Supply the usual Mattermost and LLM configuration, empty `authz.regions`, `agent.clusters` and `agent.globalServers`, and disable the query API, schedules, alerts and best-practice reports. Route only `GET /oauth/callback` to the bot's `api.addr` listener (default port 8081). Exclude callback query strings from proxy logs.

In a one-to-one bot DM send `connect pardis`, follow the link within five minutes, then ask a cloud question. Login uses one-time state and verifies the Pardis identity against the user's verified Mattermost email. Both services must report the same directory-managed email. Grants are encrypted per account and refreshed when needed. Each question uses its own MCP client and only the six configured-in-code read tools. Send `disconnect pardis` to revoke the grant and remove its stored credentials. If revocation is unavailable, retry when the issuer recovers.
