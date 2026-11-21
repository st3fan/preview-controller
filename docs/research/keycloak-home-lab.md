# Trying Keycloak for Preview Controller in an Arcane home lab

Keycloak can host the login page, broker GitHub or Google sign-in, and issue access tokens for the existing Go MCP server. This lab uses one Keycloak container and one PostgreSQL container. Your existing Caddy terminates HTTPS. **You can try social login today.** Native MCP login requires the controller changes proposed in [Intent #72](https://github.com/st3fan/preview-controller/issues/72). The current controller accepts a shared deployment token.

## Before opening Arcane

Choose two stable names under a domain you control. This example uses `id.example.net` for Keycloak and `https://mcp.example.net/mcp` for the eventual MCP resource. Make the names resolve over your LAN/VPN. Give Caddy a certificate trusted by the workstation and controller. For a private service, an existing wildcard certificate, DNS-01 with a DNS-enabled Caddy build, or Caddy's internal CA installed on each client will work. The browser follows GitHub's redirect to the private IdP. Keycloak needs outbound HTTPS for the social provider and client metadata. [Caddy HTTPS](https://caddyserver.com/docs/automatic-https)

Compose creates a default network for the stack. Keycloak reaches PostgreSQL at `db:5432` on that network. Publish only the Keycloak web port for Caddy. [Docker Compose networking](https://docs.docker.com/compose/how-tos/networking/)

## Create the Arcane project

In **Projects → Create Project**, choose the Docker agent that will run Keycloak and name the project `preview-idp`. Paste this Compose file. PostgreSQL 17 stores its data at the volume path below. The Keycloak version is pinned because its MCP features are experimental. [Arcane projects](https://getarcane.app/docs/docker/projects) · [Keycloak containers](https://www.keycloak.org/server/containers) · [Supported databases](https://www.keycloak.org/server/db)

```yaml
services:
  db:
    image: postgres:17
    restart: unless-stopped
    environment:
      POSTGRES_DB: keycloak
      POSTGRES_USER: keycloak
      POSTGRES_PASSWORD: ${IDP_DB_PASSWORD:?Set IDP_DB_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U keycloak -d keycloak"]
      interval: 5s
      timeout: 3s
      retries: 20

  keycloak:
    image: quay.io/keycloak/keycloak:26.8.2
    command: ["start"]
    restart: unless-stopped
    depends_on:
      db:
        condition: service_healthy
    environment:
      KC_DB: postgres
      KC_DB_URL: jdbc:postgresql://db:5432/keycloak
      KC_DB_USERNAME: keycloak
      KC_DB_PASSWORD: ${IDP_DB_PASSWORD:?Set IDP_DB_PASSWORD}
      KC_HOSTNAME: https://${IDP_HOST:?Set IDP_HOST}
      KC_HTTP_ENABLED: "true"
      KC_PROXY_HEADERS: xforwarded
      KC_PROXY_TRUSTED_ADDRESSES: ${CADDY_SOURCE_IP:?Set CADDY_SOURCE_IP}
      KC_FEATURES: cimd,resource-indicators
      KC_BOOTSTRAP_ADMIN_USERNAME: bootstrap-admin
      KC_BOOTSTRAP_ADMIN_PASSWORD: ${IDP_BOOTSTRAP_PASSWORD:?Set IDP_BOOTSTRAP_PASSWORD}
    ports:
      - "${IDP_BIND_IP:-127.0.0.1}:18080:8080"

volumes:
  pgdata: {}
```

Choose **Add .env file**. Replace each password placeholder with a different value generated locally, for example by running `openssl rand -hex 32` twice. Restrict access to this project and its files since the Arcane environment editor and Docker administrators can read these values.

```dotenv
IDP_HOST=id.example.net
IDP_BIND_IP=127.0.0.1
CADDY_SOURCE_IP=127.0.0.1
IDP_DB_PASSWORD=<first-generated-value>
IDP_BOOTSTRAP_PASSWORD=<second-generated-value>
```

Choose the bind and proxy source addresses described below, then click **Create Project → Up**. In the project logs, wait for PostgreSQL to become healthy and Keycloak to finish startup. Its first start can take longer while it prepares the server and schema.

## Connect Caddy

If Caddy runs directly on the same host or uses host networking, keep the loopback bind and add this site to your private Caddy configuration. Substitute your hostname and usual TLS settings.

```caddyfile
id.example.net {
    reverse_proxy 127.0.0.1:18080
}
```

If Caddy is on another machine or in a separate bridge-networked container, use the same Docker host LAN address you use for your other apps. For example, if the Arcane host is `192.168.1.20`, set `IDP_BIND_IP=192.168.1.20` and use `reverse_proxy 192.168.1.20:18080`. Limit access to that published port to Caddy using your Docker-aware firewall. [Docker port publishing](https://docs.docker.com/engine/network/port-publishing/)

For either setup, set `CADDY_SOURCE_IP` to the address Keycloak sees traffic **from**. For a separate proxy host this is usually Caddy's LAN IP. With a host-local proxy, Docker may present the project bridge gateway. To find the gateway after the network exists, run `docker network inspect preview-idp_default --format '{{(index .IPAM.Config 0).Gateway}}'` on the Arcane host (adjust the project name if needed). If necessary, start with `CADDY_SOURCE_IP=127.0.0.1`, verify the actual source and update the project before testing sign-in. Validate and reload Caddy. [Keycloak reverse proxy guide](https://www.keycloak.org/server/reverseproxy)

## Create the realm and try GitHub sign-in

1. Open `https://id.example.net/admin/` with the bootstrap credentials. Create a permanent administrator in the master realm, verify a fresh admin login, then delete the temporary bootstrap account. Remove both bootstrap environment entries from Compose and the bootstrap password from Arcane's .env editor, then redeploy. Create a realm named `preview`.
2. In the **preview** realm, select **Identity providers → GitHub**. Copy the redirect URI displayed by Keycloak. With the default alias it is `https://id.example.net/realms/preview/broker/github/endpoint`.
3. In GitHub **Settings → Developer settings → OAuth Apps**, create an OAuth app. Set its homepage to the Keycloak origin and paste the redirect URI exactly as displayed. Enter the resulting client ID and secret in the Keycloak provider and save.
4. Open `https://id.example.net/realms/preview/account/` in a private browser window. Choose GitHub and complete first login. Find the brokered account under **Users** and record its Keycloak ID for the proposed controller operator allowlist. For Google, add the Google identity provider and register its displayed callback in a Google OAuth web application. Each provider gets its own credentials. [Keycloak identity brokering](https://www.keycloak.org/docs/latest/server_admin/#_identity_broker)
5. From your workstation, check discovery. The issuer must equal the HTTPS realm URL, and the signing-key URL must be reachable from the controller. A successful account login here verifies the lab independently of #72.

```sh
curl -fsS https://id.example.net/realms/preview/.well-known/openid-configuration |
  jq '{issuer, jwks_uri, code_challenge_methods_supported, client_id_metadata_document_supported}'
```

## Configure the native MCP clients in Keycloak

The resource identifier is the **whole endpoint URL**, including `/mcp`. A token for another API must fail verification at the controller. In the preview realm create an OpenID Connect client named `preview-mcp` for the resource. Set its client attribute `resource_url` to `https://mcp.example.net/mcp` via realm import or the Admin REST API, preserving the client's other attributes. Disable interactive and direct access grants on this resource client. Codex and Claude Code are separate public clients. [Keycloak MCP guide](https://www.keycloak.org/securing-apps/mcp-authz-server)

In **Realm settings → Client policies → Profiles**, create one profile per agent and add the `client-id-metadata-document` executor. Set **Allow HTTP scheme**, **Restrict same domain**, and **Only allow confidential client** to OFF. The identity document is HTTPS, the browser callback uses loopback, and the agents use authorization code with PKCE. Add the exact MCP URL to **Resource indicator allow list**. Under **Policies**, create a policy per agent, add the `client-id-uri` condition, specify scheme `https`, and associate the matching profile.

| Agent | Trusted domain in policy condition | Trusted domains in profile executor |
| --- | --- | --- |
| Claude Code | `claude.ai` | `claude.ai`, `localhost`, `127.0.0.1` |
| Codex | `chatgpt.com` | `chatgpt.com`, `persistent.oaistatic.com`, `localhost`, `127.0.0.1` |

Keycloak documents the Claude Code settings. The Codex settings are derived from its published metadata, including its logo hostname. Confirm the actual document and callback selected by your installed client. Keep anonymous dynamic client registration disabled for this test. [Keycloak client policies](https://www.keycloak.org/securing-apps/mcp-authz-server) · [Codex client metadata](https://chatgpt.com/oauth/codex/client.json) · [Codex MCP guide](https://developers.openai.com/codex/mcp/)

## End-to-end test after #72

Configure the controller to advertise the Keycloak issuer in its protected resource metadata, verify signatures and token expiry, require the exact MCP audience, and authorize your issuer and subject pair. Use a dedicated private MCP route in Caddy that serves only the MCP endpoint and protected resource metadata. The existing management listener also serves destructive dashboard actions.

```sh
codex mcp add preview --url https://mcp.example.net/mcp
codex mcp login preview
claude mcp add --transport http preview https://mcp.example.net/mcp
```

In Claude Code open `/mcp`, select **preview**, and authenticate. With each client, call `list_projects`, repeat after the access token expires, and check that a second signed-in identity without an operator grant is denied. If Keycloak reports `invalid_target`, compare the requested resource, resource client attribute, and profile allowlist character for character. If login succeeds but MCP returns 401, check issuer, audience and expiry without sharing the token. [Claude Code MCP](https://code.claude.com/docs/en/mcp) · [MCP authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)

Keep a recoverable PostgreSQL backup before upgrading Keycloak. It contains realm configuration, broker identities, provider credentials and signing keys. Keep the named volume when recreating the project. Rehearse a restore separately, then retest fresh login and refresh in both clients after upgrades. This guide reflects the November 2026 documentation. The two MCP features are experimental, and this configuration still needs an end-to-end run in the target lab.
