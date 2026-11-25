# Per-user OAuth for MCP

Both modes are opt-in. Installations running both integrations use separate bot deployments. The main bot serves AIon requests. The dedicated Pardis bot connects individual Mattermost users to Pardis MCP.

## AIon calls the main bot

Enable the query API. For platform SSO, configure the issuer's signing keys and the audience assigned to the gateway. This example uses a staging audience.

```yaml
api:
  enabled: true
aionOAuth:
  resource: https://bot.example.org/oauth/mcp
  issuer: https://sso.example.org/realms/platform
  jwksURL: https://sso.example.org/realms/platform/protocol/openid-connect/certs
  audience: ai-staging
  trustedAuthorizedParties: [ai]
```

Set the AIon connector's `upstreamUrl` to the resource URL and `upstreamAuthType` to `oauth`. The gateway sends a user access token on each MCP request. The bot checks the RS256 signature by key ID, issuer, expiry and subject before MCP dispatch. It caches the public keys for five minutes and refreshes on a new key ID, with a rate limit on misses. A token must match the configured audience or carry a signed `azp` matching an explicitly trusted gateway client ID. Realms without a custom audience can configure only `trustedAuthorizedParties`. Platform SSO requires outbound HTTPS to the signing keys endpoint.

The optional `aionOAuth.subjectUsers` map binds immutable SSO subject IDs to OpenShift usernames. Otherwise the bot uses the signed `preferred_username` when available, falling back to the signed subject. When OpenShift usernames differ from the subject and the gateway omits `preferred_username`, configure the mapping for those users. Enable this mode with an issuer whose username claim is directory-controlled. Cluster authorization uses the existing mcp-authz resolver and its OpenShift group lookups.

For an issuer that provides opaque tokens, the existing introspection configuration uses `introspectionURL`, `clientID`, `scope` and `AION_INTROSPECTION_SECRET` with the resource URL as token audience. OpenShift clients use the existing `/mcp` endpoint.

## The Pardis bot calls the user's MCP server

Register the bot at the Pardis issuer for the authorization-code and refresh-token grants, scope `mcp:tools`, and the exact callback URI. Copy the endpoints from the issuer's metadata. Public clients use PKCE S256. For a confidential client registered with `client_secret_post`, also set `PARDIS_OAUTH_CLIENT_SECRET`.

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
