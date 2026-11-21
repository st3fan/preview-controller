# Try Pocket ID with Codex and Claude Code in an Arcane home lab

This trial uses local Pocket ID users and passkeys. Arcane runs Pocket ID and a disposable notes MCP server. Caddy runs directly on the same Linux host and handles HTTPS. The notes server lets you test both clients today. Preview Controller still expects its shared token until [#72](https://github.com/st3fan/preview-controller/issues/72) adds OAuth support to the existing binary. [Pocket ID’s upstream demo](https://github.com/pocket-id/mcp-oauth-demo/tree/8a1dbf50038c6558bf509072083f699a8d2bc49f)

## 1. Create the Arcane project

Choose two private DNS names that resolve to the Caddy host from your workstation. The examples use `pocket-id.example.net` for the issuer and `notes-mcp.example.net` for the demo. Give both names trusted HTTPS certificates using your usual Caddy setup. Pick a stable issuer name before enrolling a passkey, because the browser binds it to the domain. Pocket ID needs outbound HTTPS to fetch client metadata. The Docker builder needs access to GitHub, the build images and Go dependencies. [Pocket ID installation](https://pocket-id.org/docs/setup/installation)

In **Projects → Create Project**, select the Arcane agent on the Caddy host. Name the project `pocket-id-lab` and paste this Compose file. It publishes two loopback ports and uses Compose’s default network. The demo builds from the upstream Dockerfile in the pinned repository’s `mcp-server` directory. [Arcane projects](https://getarcane.app/docs/docker/projects)

```yaml
services:
  pocket-id:
    image: ghcr.io/pocket-id/pocket-id:v2.19.3
    restart: unless-stopped
    environment:
      APP_URL: ${POCKET_ID_URL:?Set POCKET_ID_URL}
      ENCRYPTION_KEY: ${POCKET_ID_ENCRYPTION_KEY:?Set POCKET_ID_ENCRYPTION_KEY}
      UI_CONFIG_DISABLED: "true"
      CIMD_URL_ALLOWLIST: ${CIMD_URL_ALLOWLIST:?Set CIMD_URL_ALLOWLIST}
    volumes:
      - pocket-id-data:/app/data
    ports:
      - "127.0.0.1:1411:1411"
    healthcheck:
      test: ["CMD", "/app/pocket-id", "healthcheck"]
      interval: 15s
      timeout: 5s
      retries: 5

  notes-mcp:
    build:
      context: "https://github.com/pocket-id/mcp-oauth-demo.git#8a1dbf50038c6558bf509072083f699a8d2bc49f:mcp-server"
    restart: unless-stopped
    depends_on:
      pocket-id:
        condition: service_healthy
    environment:
      MCP_RESOURCE_URL: ${NOTES_MCP_URL:?Set NOTES_MCP_URL}
      OAUTH_ISSUER_URL: ${POCKET_ID_URL:?Set POCKET_ID_URL}
      OAUTH_JWKS_URL: http://pocket-id:1411/.well-known/jwks.json
      OAUTH_REQUIRED_SCOPES: notes:read
    ports:
      - "127.0.0.1:18111:8080"

volumes:
  pocket-id-data: {}
```

Choose **Add .env file**. Replace the hostnames and generate a fresh encryption key locally with `openssl rand -base64 32`. Save the key privately along with backups of the named volume. Pocket ID stores its SQLite database there. Choose **Create Project → Build & Deploy**. [Pocket ID configuration](https://pocket-id.org/docs/configuration/environment-variables)

```dotenv
POCKET_ID_URL=https://pocket-id.example.net
NOTES_MCP_URL=https://notes-mcp.example.net/mcp
POCKET_ID_ENCRYPTION_KEY=<paste-your-generated-key>
CIMD_URL_ALLOWLIST='["https://claude.ai/oauth/claude-code-client-metadata","https://chatgpt.com/oauth/codex/client.json"]'
```

## 2. Point host Caddy at the loopback ports

Add these sites to the host’s existing Caddyfile, using your chosen names and TLS settings. Validate and reload Caddy using your usual host service commands. Keep access within your LAN/VPN during setup. [Pocket ID behind Caddy](https://pocket-id.org/docs/setup/reverse-proxy)

```caddyfile
pocket-id.example.net {
    reverse_proxy 127.0.0.1:1411
}

notes-mcp.example.net {
    reverse_proxy 127.0.0.1:18111
}
```

From the workstation where you run the agents, check these three URLs. Expect the Pocket ID issuer to equal `POCKET_ID_URL`, the notes metadata to name the exact notes URL and issuer, and the unauthenticated MCP request to return **401** with a `WWW-Authenticate` header pointing to resource metadata.

```sh
curl -fsS https://pocket-id.example.net/.well-known/openid-configuration | jq '{issuer, jwks_uri, client_id_metadata_document_supported}'
curl -fsS https://notes-mcp.example.net/.well-known/oauth-protected-resource/mcp | jq '{resource, authorization_servers, scopes_supported}'
curl -si https://notes-mcp.example.net/mcp
```

## 3. Create your local account and authorize the notes API

1. Open `https://pocket-id.example.net/setup`, create your local administrator account and enroll a passkey. Sign in again in a private browser window to confirm it works. For a separate operator account, create a local user under **Administration → Users** and use its one-time login link to enroll that user's passkey. [Pocket ID installation](https://pocket-id.org/docs/setup/installation)
2. Open **Administration → APIs → Add API**. Use `Notes MCP trial` as its name and the exact `NOTES_MCP_URL`, including `/mcp`, as the **API resource**.
3. Add permission `notes:read` with display name `Read notes`. Under **Access → Metadata document clients**, enable **Allow all metadata document clients**, grant `notes:read`, and save. The instance-level URL allowlist in Compose limits which client documents Pocket ID accepts. This trial exercises only the demo’s read tool. [Upstream API setup](https://github.com/pocket-id/mcp-oauth-demo/tree/8a1dbf50038c6558bf509072083f699a8d2bc49f#3-configure-pocket-id)

## 4. Test each agent from your workstation

**Claude Code.** Register the notes URL from your project directory:

```sh
claude mcp add --transport http pocket-notes https://notes-mcp.example.net/mcp
```

Start Claude Code in that directory, open `/mcp`, select **pocket-notes** and authenticate. In the browser, use your Pocket ID passkey and approve read access. Ask Claude to **call the pocket-notes list_notes tool and show its result**. A successful empty list is expected for a new user. Check that the tool was actually invoked. [Claude Code MCP](https://code.claude.com/docs/en/mcp)

**Codex.** Register the same URL and start native login:

```sh
codex mcp add pocket-notes --url https://notes-mcp.example.net/mcp
codex mcp login pocket-notes --scopes notes:read,offline_access
```

Approve in your browser. Start a fresh Codex session and ask it to **call the pocket-notes list_notes tool and show its result**. Codex may select a server-specific metadata URL depending on the issuer’s discovery flags. If Pocket ID rejects the client, inspect the authorization request’s `client_id` locally. If it is the documented `https://chatgpt.com/oauth/codex/<callback_id>/client.json` form, add that **exact** URL to the JSON allowlist in Arcane, redeploy, then retry login. Keep the other entries. To diagnose registration specifically, retry with `--oauth-client-registration cimd`. Pocket ID publishes a Claude Code walkthrough. Codex interoperability is a trial outcome. [Codex login and registration](https://developers.openai.com/codex/mcp/)

**Check persistence and refresh.** Record each CLI version and whether login and the read succeeded. Restart each client and repeat the read. Repeat again after the access-token lifetime shown for that client in Pocket ID. A read that succeeds without another browser prompt after expiry is the refresh check. An `invalid_target` error points to the resource URL, including its path. A scope error warrants checking the API permission, client access grant and consent. Use the Pocket ID audit log for sign-in failures. Keep callback URLs and tokens private.

## 5. Use Preview Controller after #72

The notes test exercises the IdP and client flow. Once the controller supplies OAuth discovery, audience validation and the operator allowlist, register its **own** canonical HTTPS MCP URL as a separate API with the controller’s documented permissions. Configure the Pocket ID issuer and the operator's subject in the controller, add the controller in each agent under a different name, and call `list_projects`. Verify a second local user without an operator grant is denied. Keep the management dashboard on its private listener. After the trial you can remove the notes service and its Caddy site while retaining the Pocket ID volume and encryption key.
