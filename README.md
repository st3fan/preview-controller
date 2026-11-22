# Preview Controller

A self-hosted controller for stable preview environments associated with SDLC Intents.

The controller accepts an authenticated candidate commit, verifies it in an
allowlisted public GitHub repository, and builds an isolated Compose stack or a single Docker container.
The stable preview URL switches to the new loopback port after health checks pass.
The previous preview stays available if the candidate fails.

For Dockerfile-only applications, see [single-container Docker projects](docs/installation.md#single-container-docker-projects).

For a prebuilt image, see [Portainer installation](docs/installation.md#portainer-with-a-published-image).

## Run with Docker Compose

Use a dedicated Linux host with rootful Docker Engine and the Docker Compose
plugin. The controller gets host-level access through the Docker socket.

1. Clone the repository and prepare the workspace. For a fresh installation,
   install the example settings. If you already have
   `/etc/preview-server.json`, edit that file in place.

   ```bash
   git clone https://github.com/st3fan/preview-controller.git
   cd preview-controller
   sudo install -d -o root -g root -m 0700 /srv/preview-server
   ```

   On a fresh installation, run:

   ```bash
   sudo install -m 0600 config/preview-server.example.json /etc/preview-server.json
   ```

2. Edit the settings with `sudoedit /etc/preview-server.json`. Replace the
   example project with your public GitHub repository and set its Compose file,
   public service, port, and health path. Set `preview_domain` to your preview
   domain. Keep these values for the shipped Compose definition:

   | Setting | Value |
   | --- | --- |
   | `state_root` | `/var/lib/preview-server` |
   | `workspace_root` | `/srv/preview-server` |
   | `deployment_token_env` | `PREVIEW_SERVER_TOKEN` |

3. From the repository directory, enter your deployment bearer token and
   build and start the controller. Use the same token in deployment requests.

   ```bash
   read -rsp 'Deployment bearer token: ' PREVIEW_SERVER_TOKEN
   export PREVIEW_SERVER_TOKEN
   docker compose up -d --build
   unset PREVIEW_SERVER_TOKEN
   ```

   Supply the token again when recreating the controller.

4. Check the service on the preview host. With the example listener settings,
   the health endpoint returns HTTP 204 and the dashboard is at
   `http://127.0.0.1:8080/`.

   ```bash
   docker compose ps
   docker compose logs --tail=50 controller
   curl -i http://127.0.0.1:8080/healthz
   ```

For preview URLs, point wildcard DNS at Caddy and configure Caddy to forward
preview requests to `127.0.0.1:8081`. Then submit a candidate using the
[deployment API](#deployment-api). See the [installation guide](docs/installation.md#caddy-and-preview-ingress)
for the Caddy configuration and [Compose settings](docs/installation.md#docker-compose)
for custom paths.

Use `docker compose stop` to stop the controller. Active previews keep running.
Before uninstalling, remove them through the deployment API so the controller
can clean up their containers and workspaces.

## Run as a native executable

Use Go 1.27 or newer. Build a Linux executable with CGO disabled; set
`GOARCH` to the preview host's architecture when cross-compiling:

```sh
GOOS=linux CGO_ENABLED=0 go build -o preview-server ./cmd/preview-server
```

Copy [the example settings](config/preview-server.example.json) to
`/etc/preview-server.json` and adjust the project allowlist, state root,
workspace root, and management listener for the host. Put the bearer secret in
the environment variable named by `deployment_token_env`; the settings file
contains no deployment state. The management listener accepts only a loopback or private
IP address. The service account needs write access to the state and workspace
roots and access to the local Docker daemon. Run it only on a dedicated preview
host; Docker builds and containers can execute untrusted project code there.
The `preview_listen` address is a separate listener for Caddy. See
[Linux installation and ingress setup](docs/installation.md).

```sh
PREVIEW_SERVER_TOKEN='replace-with-a-secret' ./preview-server -config /etc/preview-server.json
```

Add `-verbose` to the server command to log startup reconciliation, deployment
progress, health checks, route switches, and cleanup. Use the deployment ID
to correlate those messages with the status API. Verbose logs omit Compose
output and variable values.

The controller writes one atomic JSON file per environment at
`<state-root>/<project>/<intent>.json`. It records attempts and errors there.
Docker is the source of runtime container state.

## Active preview health

The dashboard reports active preview health alongside the latest deployment
status. The controller checks each active preview's configured `health_path`
on its local port. It starts a round every 30 seconds, with up to four checks
in flight and a three-second timeout per check.

Health starts at Unknown, becomes Unhealthy after two consecutive failures,
and becomes Healthy on any successful check. The dashboard shows the last
success and any current failure summary. Observations reset on controller
restart or preview activation. Health failures leave the active route in place.

## Preview retention

Previews archive after seven days from their last healthy deployment or restore.
Existing previews receive a full archive interval when the controller first
applies this policy. The idle timer measures time since activation or restore.
Archived stacks stop using CPU and memory and keep their containers, volumes,
networks, images, and workspace. Restore restarts the same stack and waits for
health before reopening its URL. Restore an archived preview before deploying a
new commit to it.

The controller permanently deletes an archived preview seven days after it was
stopped. Set `archive_after_idle` and `delete_after_archive` in the global JSON
settings to change the defaults. Each project can override either interval
independently. Use positive Go duration strings such as `24h` or `168h`.
The dashboard shows each deadline and offers Archive, Restore, and Delete.
Delete starts permanent cleanup immediately. Cleanup removes identifiable
preview-owned build tags and retains shared images.

## Coding agents (MCP)

Connect a Streamable HTTP MCP client to `/mcp` on the controller’s management
address. By default, configure the client to send `Authorization: Bearer <deployment-token>`
on every request. The same credential grants access to all seven tools.
For browser sign-in with individual operator identities, enable Pocket ID as described below.

Use `list_projects` to match the checkout’s Git remote to a configured project
ID, including before its first preview. Use `list_previews` or `get_preview`
to inspect existing previews. To deploy, call `deploy_preview` with the project
ID, positive Intent number, full approved commit SHA, and a unique `request_id`.
Reuse that ID when retrying a lost response. Choose a new ID for a deliberate
redeployment. Retry identities are scoped to the project and Intent and last
for the lifetime of its deployment history.

Poll `get_deployment` with the returned deployment ID. Require that specific attempt to report `healthy`. Before reporting ready,
call `get_preview` and confirm `active_commit` matches the requested SHA. The
stable URL may still serve the previous revision during or after a failed
replacement. Use `get_deployment_logs` for the exact attempt’s retained
diagnostics and bounded service output, when available. Failed attempts retain
up to 8 KiB of service output before cleanup under the existing diagnostic
retention policy. Projects with configured Compose environment values suppress
persisted output. Use `remove_preview`
to permanently remove the environment and its history.

### Pocket ID sign-in on the LAN or VPN

Use one private HTTPS hostname for the dashboard, REST API and MCP. Restrict
access to trusted operators on the LAN or VPN: REST reads and the default open dashboard trust network access. For example, on a private Caddy ingress:

```caddyfile
preview.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Use your management listener address and usual TLS settings. Keep the wildcard
preview application hosts routed to the separate preview listener.

1. In Pocket ID **Administration → APIs**, add **Preview Controller** with the exact
   HTTPS resource URL including `/mcp`. Create the API permission
   `preview:operate`. Create a Pocket ID user group for Preview Controller
   operators and add the intended accounts to it.
2. Restrict Pocket ID’s `CIMD_URL_ALLOWLIST` to the exact native client metadata
   URLs you use. Codex can use
   `https://chatgpt.com/oauth/codex/client.json` or a callback-specific URL.
   If a client is missing from **Administration → OIDC Clients**, start a
   sign-in to let Pocket ID fetch its metadata and create the entry.
   For each client, set **Access → Allowed User Groups** to **Selected user
   groups** and select only the operator group. Then grant the client
   user-delegated API access to `preview:operate` under **Access → API Access**.
   Keep the API-wide **Allow all metadata document clients** setting off,
   and grant this permission only to operator-group-restricted clients.
3. Add the following object to the controller settings. Copy `issuer` and
   `jwks_uri` from the issuer’s `/.well-known/openid-configuration`. The
   controller must trust the certificate at the signing-key URL. It supports
   RS256 (the Pocket ID default), ES256 and EdDSA. Remove `operator_subjects`
   if you configured an earlier version of this draft.

```json
"mcp_oauth": {
  "issuer": "https://pocket-id.example.net",
  "jwks_uri": "https://pocket-id.example.net/.well-known/jwks.json",
  "resource": "https://preview.example.com/mcp"
}
```

4. Restart the controller. From the workstation, request `/mcp` without
   credentials. Its 401 challenge links to the protected-resource metadata.
   Check that the metadata advertises the exact HTTPS resource and issuer.
   From the trusted network, verify that the same host serves the dashboard at
   `/` and the REST environment list at `/api/environments`.
5. Remove any deployment-token header or bearer environment setting from the
   client's MCP entry. In Codex, run `codex mcp add preview --url https://preview.example.com/mcp`
   and `codex mcp login preview`. Complete the browser
   passkey prompt and consent on the client workstation.
6. Call `list_projects` in Codex. Repeat after restarting Codex and
   after access-token expiry to verify refresh. Try Codex with a Pocket ID
   user outside the operator group and confirm Pocket ID denies authorization.
   A 401 with a token can indicate expiry, the wrong issuer or audience, or an
   unverifiable signature. A valid access token missing `preview:operate`
   receives 403.

Operators with the permission have all seven tools, including permanent removal.
The controller logs the issuer and subject for deployment and removal calls.
In OAuth mode the shared deployment token works only on the private REST API.
Manage onboarding and offboarding in Pocket ID by changing group membership.
After removing an operator, verify that refresh and fresh authorization are
denied for Codex. An already-issued signed access token can remain usable
until it expires. Set the access token lifetime in Pocket ID to match the
offboarding window you need.

## Deployment API

The management API and operator dashboard are restricted to the configured home-lab listener.
By default, anyone who can reach the dashboard can archive, restore, or delete previews. To require browser sign-in and grant read-only access, follow the [dashboard Pocket ID setup](docs/installation.md#dashboard-sign-in-with-pocket-id). Keep the management
listener accessible only to trusted operators.
Open `/` on that listener to see preview links, active commits, and live deployment
status grouped by project. Select **Live logs** next to a deployment attempt to
see its last 100 lines per Compose service container, followed by new output.
The log URL stays tied to that attempt during replacement. Logs become
unavailable once the attempt is removed. Treat log URLs and output as operator
information.

Use the trash button on a dashboard row and confirm to remove the preview
and its deployment history. If cleanup fails, use the same button to retry.

Candidate requests require a full 40-character commit SHA and the bearer
secret. The authenticated request is the approval to preview that commit.

```http
POST /api/deployments
Authorization: Bearer <shared-secret>
Content-Type: application/json

{"project":"myproject","intent":123,"commit":"<40-character-commit-sha>"}
```

The API returns HTTP 202 with a deployment ID, `queued` status, and a stable
URL such as `https://myproject-123.preview.example.com`. Verification proceeds
asynchronously. Read status through `GET /api/deployments/{id}`,
`GET /api/environments`, and `GET /api/environments/{project}/{intent}`.
An authenticated `DELETE /api/environments/{project}/{intent}` removes a preview.
If Docker cleanup fails, the route remains removed and the deletion can be
retried with the same request.

If upgrading from intent-first URLs, update bookmarks and integrations to the
project-first hostname. Existing environments are reachable at their new URLs
without rebuilding or redeploying. The same wildcard DNS and TLS setup applies.

The project's Compose file is restricted to services and named volumes with a
small set of service options. Build contexts, Dockerfiles, and bind mounts must
resolve inside the checkout. The controller rejects host access, external
Compose files and unsupported options. It replaces all published ports in the
project file with one dynamically assigned loopback mapping for the configured
public service. The controller creates a unique Compose project per attempt.
A candidate passes the configured HTTP
health path before its status becomes `healthy`. Failed candidates are cleaned
up; existing stacks are left running. After a successful route switch, the
previous stack is removed. The preview listener accepts only configured stable
hostnames and proxies only to the active attempt's recorded loopback port.

## Preview inspector

Set `"preview_inspector": true` in a project's controller settings to enable
the π button. The panel links to the exact deployed commit and GitHub Intent
Issue. To show the issue title, include the optional `issue_title` string in the
authenticated deployment request. Supply it only when the title can be shown to
preview visitors. Submissions without it show the issue number alone.

Expand **Discuss this intent** to copy a starter prompt for your chat. **Open in
Codex** prefills the same prompt in the desktop app for you to review and send.

Set the optional top-level `dashboard_origin` to the browser-reachable origin
of your existing operator dashboard (for example, `https://operator.example.com`).
When live logs are available for the active attempt, the inspector links to that
attempt's logs through the existing operator listener. Keep its existing network
and access restrictions in place.

The inspector applies to top-level browser navigations with Fetch Metadata
headers and successful HTML responses with a known length of at most 2 MiB.
Streamed, compressed, downloadable, and partial responses pass through.
The application's Content Security Policy still applies. Policies that block
inline scripts or styles can prevent the inspector from appearing or styling itself.

## Compose preview contract

To add a project-side `preview.sh`, follow the [preview script guide](docs/preview-script.md).

Set `public_service` to the Compose service that receives preview traffic.
`public_port` is its container port and defaults to `8080`. Fixed-port apps can
set `public_port` to match their existing listener. Configurable apps can use
`${PREVIEW_PORT:-8080}` in their Compose command or environment. The controller
sets `PREVIEW_PORT` for Compose interpolation and assigns the host port itself.

Use `compose_env` in a project's settings for literal values, including
preview-only secrets. Keep settings files with literal secrets private and out
of version control. Use
`compose_env_from` to map Compose variables to controller service environment
variables. For example, these project settings supply a literal database
password and reference a token from the service environment:

```json
{
  "compose_env": {
    "POSTGRES_PASSWORD": "replace-with-a-preview-only-password"
  },
  "compose_env_from": {
    "APP_API_TOKEN": "PREVIEW_APP_API_TOKEN"
  }
}
```

Set referenced secrets in the controller's service environment and give each
project only the credentials its approved preview commits should receive. The
controller does not load repository `.env` files. Interpolation is supported in
service images, commands, entrypoints, environments, and health checks. Build
paths and mount declarations must remain literal so the controller can check
them before invoking Docker Compose.

On a disposable Linux host with Docker Compose, run the opt-in fixture checks
with `CGO_ENABLED=0 go test -tags=integration -timeout=30m ./internal/runtime
./internal/ingress`. They build and start fixture containers, exercise the
preview lifecycle, and remove the containers afterward.

Only public `https://github.com/owner/repo` repositories are supported in this
first version. Git operations use `go-git`; the server does not run the Git CLI
or use Git credentials.
