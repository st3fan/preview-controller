# Set up Pocket ID in Arcane with Caddy on the host

Arcane runs one Pocket ID container, and Caddy runs directly on the same Linux host. You can enroll a local passkey user and verify sign-in now. Test the coding agents once a Preview Controller build with [#72](https://github.com/st3fan/preview-controller/issues/72) is deployed.

## 1. Create the Arcane project

Choose a stable DNS name for Pocket ID that resolves to the Caddy host from your workstation. This example uses `pocket-id.example.net`. Use your usual trusted HTTPS certificate. Passkeys are bound to the issuer’s domain. [Pocket ID installation](https://pocket-id.org/docs/setup/installation)

In **Projects → Create Project**, select the Arcane agent on the Caddy host. Name the project `pocket-id-lab` and paste this Compose file. [Arcane projects](https://getarcane.app/docs/docker/projects)

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

volumes:
  pocket-id-data: {}
```

Choose **Add .env file**. Replace the hostname and generate a fresh encryption key locally with `openssl rand -base64 32`. Save the key privately along with backups of the named volume. Pocket ID stores its SQLite database there. Choose **Create Project → Up**. [Pocket ID configuration](https://pocket-id.org/docs/configuration/environment-variables)

```dotenv
POCKET_ID_URL=https://pocket-id.example.net
POCKET_ID_ENCRYPTION_KEY=<paste-your-generated-key>
CIMD_URL_ALLOWLIST='["https://claude.ai/oauth/claude-code-client-metadata","https://chatgpt.com/oauth/codex/client.json"]'
```

## 2. Connect host Caddy and verify the issuer

Add this site to your existing Caddyfile, substituting your DNS name and usual TLS settings. Validate and reload Caddy through your normal host service workflow. Keep the setup page reachable only from your trusted LAN/VPN. [Pocket ID behind Caddy](https://pocket-id.org/docs/setup/reverse-proxy)

```caddyfile
pocket-id.example.net {
    reverse_proxy 127.0.0.1:1411
}
```

From your workstation, check that `issuer` matches `POCKET_ID_URL` and that discovery returns an HTTPS signing-key URL.

```sh
curl -fsS https://pocket-id.example.net/.well-known/openid-configuration | jq '{issuer, jwks_uri, client_id_metadata_document_supported}'
```

## 3. Verify local passkey sign-in

Open `https://pocket-id.example.net/setup`, create your local administrator account and enroll a passkey. Sign in again in a private browser window. To create a separate operator, open **Administration → Users**, create the user and use its one-time login link to enroll its passkey. A successful sign-in completes the standalone Pocket ID setup. [Pocket ID installation](https://pocket-id.org/docs/setup/installation)

## After #72: test Preview Controller

Once the OAuth-enabled controller is deployed, follow its release instructions to configure the Pocket ID issuer and grant your operator access by issuer and subject. Use the controller’s exact HTTPS MCP URL wherever this section shows `https://mcp.example.net/mcp`. Keep the management listener private.

1. In Pocket ID, open **Administration → APIs → Add API**. Name it **Preview Controller**. Set **API resource** to the exact controller URL including `/mcp`. Add the permissions specified by the implementation. Under **Access → Metadata document clients**, enable access for the allowlisted clients and grant the permissions required for `list_projects`. Pocket ID needs outbound HTTPS to fetch each client’s metadata.
2. From the workstation, request the MCP endpoint without credentials. Expect HTTP 401 and a `WWW-Authenticate` header with a `resource_metadata` URL. Fetch that URL and confirm its `resource` matches the controller URL and its `authorization_servers` includes your Pocket ID issuer.

### Codex

```sh
codex mcp add preview --url https://mcp.example.net/mcp
codex mcp login preview
```

Approve the request in your browser with your Pocket ID passkey. Start a fresh Codex session and ask it to **call the preview server’s list_projects tool and show the result**. If Pocket ID rejects the client ID, check the authorization request’s `client_id` locally. Codex can use the server-specific form `https://chatgpt.com/oauth/codex/<callback_id>/client.json`. If it does, add that exact URL to the existing `CIMD_URL_ALLOWLIST` in Arcane, redeploy Pocket ID and retry. [Codex MCP guide](https://developers.openai.com/codex/mcp/)

### Claude Code

```sh
claude mcp add --transport http preview https://mcp.example.net/mcp
```

Start Claude Code in the same project directory. Open `/mcp`, select **preview** and authenticate with your Pocket ID passkey in the browser. Ask Claude to **call the preview server’s list_projects tool and show the result**. [Claude Code MCP guide](https://code.claude.com/docs/en/mcp)

In each client, verify the tool call succeeded and returned the controller’s project list. Restart the client and repeat. Repeat once more after the access token expires to check refresh without a new login prompt. With a second local user who lacks a controller operator grant, verify that authentication succeeds and the controller denies tool access.
