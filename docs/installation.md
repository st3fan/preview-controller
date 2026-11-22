# Linux installation and ingress

Use a dedicated Linux preview host. Docker access is effectively host-level
access: repository builds and Compose services execute untrusted project code.
Do not place unrelated workloads or secrets on this host.

## Prerequisites

- Docker Engine and Docker Compose on the Linux host, and outbound HTTPS
  access to public GitHub repositories. The native build also needs Go 1.27 or newer.
- Wildcard DNS for `*.preview.example.com` pointing at Caddy and a wildcard TLS
  certificate or Caddy DNS challenge setup. A wildcard covers the flat
  `<project>-<intent>.preview.example.com` hostname.
- For systemd, a dedicated service account that can access the Docker daemon. Membership in
  the `docker` group grants powerful host access; restrict it to this host.

## Docker Compose

Follow the [Compose quickstart](../README.md#run-with-docker-compose) to copy the examples, set your secrets and start the controller. After editing either file, run `docker compose up -d --force-recreate` to reload the environment and remount the current projects file.

The controller uses host networking to reach loopback preview ports. Keep both listeners on loopback when Caddy runs on the same host. The controller mounts the rootful Docker socket, which grants access to the host and to controller credentials. Preview services run as separate projects.

Keep the workspace at the same absolute path inside and outside the controller, in a dedicated directory with no symlink components. The host daemon resolves checkout bind mounts at that path. Keep the Compose project name stable so upgrades reuse its named `state` volume. Stopping the controller leaves previews running. Remove previews through the API before uninstalling.

## Controller settings

For Compose, set the following in `.env` beside the manifest. Portainer accepts the same values as stack variables. Native installs supply them through the process environment. Empty values use the defaults below.

| Old JSON setting | Environment variable | Default or requirement |
| --- | --- | --- |
| `management_listen` | `PREVIEW_MANAGEMENT_LISTEN` | `127.0.0.1:8080`, loopback or private IP and port |
| `preview_listen` | `PREVIEW_LISTEN` | `127.0.0.1:8081`, a separate loopback or private listener |
| `preview_domain` | `PREVIEW_DOMAIN` | Required DNS suffix for preview hosts |
| `state_root` | `PREVIEW_STATE_ROOT` | `/var/lib/preview-server`, the named state volume’s mount target |
| `workspace_root` | `PREVIEW_SERVER_WORKSPACE` | `/srv/preview-server`, absolute host and container workspace path |
| `deployment_token_env` | `PREVIEW_SERVER_TOKEN` | Required deployment bearer token value |
| `github_token_env` | `PREVIEW_GITHUB_TOKEN` | Optional GitHub metadata token value |
| `archive_after_idle` | `PREVIEW_ARCHIVE_AFTER_IDLE` | `168h`, positive Go duration |
| `delete_after_archive` | `PREVIEW_DELETE_AFTER_ARCHIVE` | `168h`, positive Go duration |
| `dashboard_origin` | `PREVIEW_DASHBOARD_ORIGIN` | Optional browser-reachable origin. HTTPS required for dashboard sign-in |
| `dashboard_auth.issuer` | `PREVIEW_DASHBOARD_ISSUER` | HTTPS Pocket ID issuer |
| `dashboard_auth.client_id` | `PREVIEW_DASHBOARD_CLIENT_ID` | Public browser client ID |
| `dashboard_auth.view_group` | `PREVIEW_DASHBOARD_VIEW_GROUP` | View group name |
| `dashboard_auth.operate_group` | `PREVIEW_DASHBOARD_OPERATE_GROUP` | Distinct Operate group name |
| `mcp_oauth.issuer` | `PREVIEW_MCP_ISSUER` | HTTPS Pocket ID issuer |
| `mcp_oauth.resource` | `PREVIEW_MCP_RESOURCE` | Exact HTTPS management URL ending in the path `/mcp` |
| `mcp_oauth.jwks_uri` | `PREVIEW_MCP_JWKS_URI` | HTTPS signing-key URL |

Set all four dashboard sign-in variables and the HTTPS origin together. Leaving the four empty enables the open dashboard. Set all three MCP variables together to enable OAuth. Leaving all three empty enables bearer-token MCP. When both use Pocket ID, their issuer URLs must match.

`PREVIEW_PROJECTS_FILE` selects the host bind source for Compose (default `./projects.json`) or Portainer (default `/etc/preview-server/projects.json`). Both mount it read-only at `/etc/preview-server/projects.json`. For project-specific `compose_env_from` variables, add each source name to the controller service’s `environment` list and supply its value in the local environment or stack variables. Keep the operator's files private. Use single quotes around literal values containing dollar signs in Compose’s `.env`.

For local builds, optionally set `PREVIEW_CONTROLLER_REVISION` to the Git commit for the dashboard. Without it the dashboard displays “Development build”. Portainer requires `PREVIEW_SERVER_IMAGE` as described below.

## Migrate from the combined settings file

1. Before upgrading, record the existing Compose project or Portainer stack name, actual state volume and mount target, absolute workspace path, and settings file location (previously configurable with `PREVIEW_SERVER_CONFIG`). Keep a backup of the settings and state. Reuse the same volume and paths. Existing state files and deployment history use the same format.
2. In a private directory, copy the old file’s `projects` object into `projects.json`. For example, on a host with jq, run `(umask 077; jq -e '.projects | objects' /etc/preview-server.json > projects.json)` with a fresh destination. Preserve every slug and project entry, including lifecycle overrides and environment mappings.
3. Copy `.env.example` to a private `.env`. Transfer every configured global field using the table above. For custom token variable names, copy the **values** of the formerly named variables into the two fixed token variables. Retain any project-specific environment sources. Copy all dashboard and MCP auth settings before startup so each endpoint retains its access policy.
4. Point `PREVIEW_PROJECTS_FILE` at the new file. Review the mount paths and project or stack name before recreating the controller. Validate locally with `docker compose config --quiet`. Apply with `docker compose up -d --build --force-recreate` or redeploy the same Portainer stack. Keep the existing state volume attached throughout.
5. Check for HTTP 204 at the management listener’s `/healthz`, then verify existing previews, history and sign-in from the dashboard. Native installs perform the same settings transfer into the service environment file and update the startup command below.

## Native executable and systemd

Build with `GOOS=linux CGO_ENABLED=0 go build -o preview-server ./cmd/preview-server` and install at `/usr/local/bin/preview-server`. Install `projects.json` at `/etc/preview-server/projects.json` with read access for the service account. Give that account write access to both storage roots. Place controller variables and any project-specific sources in the root-owned, mode `0600` file `/etc/preview-server.env` using systemd EnvironmentFile syntax. The binary reads the process environment directly. After updating the unit, run `systemctl daemon-reload` and restart the service.

```ini
[Unit]
Description=Preview Controller
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=preview-server
SupplementaryGroups=docker
EnvironmentFile=/etc/preview-server.env
ExecStart=/usr/local/bin/preview-server -projects /etc/preview-server/projects.json
Restart=on-failure
UMask=0077
NoNewPrivileges=true
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

## Portainer with a published image

Use a Portainer Docker Standalone environment on a dedicated Linux host
running amd64 or arm64. Prepare `/etc/preview-server/projects.json` on the Docker host from `config/projects.example.json` in the release source tree, and create the workspace directory as in the Compose quickstart.

1. Create a stack in Portainer and paste the contents of
   `compose.portainer.yaml` from the same release into the web editor.
2. Set the stack environment variables `PREVIEW_DOMAIN`, `PREVIEW_SERVER_IMAGE` and
   `PREVIEW_SERVER_TOKEN`. Use the exact `ghcr.io/st3fan/preview-controller@sha256:…`
   reference from the **Publish release image** workflow summary to pin the
   multi-platform index digest. You can also use the release version tag,
   such as `ghcr.io/st3fan/preview-controller:v1.2.3`. For SemVer build
   metadata, the image tag uses `_` in place of `+`.
3. Deploy the stack. Check the controller logs in Portainer and run
   `curl -i http://127.0.0.1:8080/healthz` on the host. The example
   configuration returns HTTP 204.
4. To update, edit `PREVIEW_SERVER_IMAGE` to the new release digest and
   redeploy the same stack. Keep its name and named state volume so
   deployment records survive controller replacement.

The stack uses host networking, the host Docker socket, a read-only settings
file and a named state volume. The workspace bind uses the same absolute path
inside and outside the controller so the host daemon can resolve mounts for
preview services. Use the optional `PREVIEW_PROJECTS_FILE` and
`PREVIEW_SERVER_WORKSPACE` stack variables to customize host paths.

### Publishing and verifying a release image

Publish a GitHub Release with a tag such as `v1.2.3` to start the workflow.
The workflow runs CI before publishing the version tag. The highest stable
published SemVer release also updates `latest` to the same index. GitHub
prereleases and SemVer prereleases only get the version tag.

On first publication, open the GHCR package settings and set visibility to
**Public**. GitHub defaults new packages to private even for a public source
repository. If the verification jobs fail before the visibility change, rerun
the failed jobs. Verification pulls both the version tag and index digest
without registry credentials on amd64 and arm64, starts the Portainer stack,
and checks the health endpoint and Docker Compose access. The workflow summary
records the digest for pinning.

## Caddy and preview ingress

The operator configures Caddy once. With a previously provisioned wildcard
certificate, a Caddyfile route can be:

```caddyfile
*.preview.example.com {
    tls /etc/caddy/certs/preview-wildcard.pem /etc/caddy/certs/preview-wildcard-key.pem
    reverse_proxy 127.0.0.1:8081
}
```

Match the Caddy upstream to `PREVIEW_LISTEN`. Keep the management API on its
own home-lab or loopback listener, for example `127.0.0.1:8080`. Caddy does not
need to reload when previews are created or replaced; the Go controller reads
the active route from JSON state. Do not route the management API through the
public wildcard site.

Send a deployment request to the management listener with the bearer token and
a full 40-character commit SHA. The request queues work; check
`GET /api/deployments/{id}` or `GET /api/environments/{project}/{intent}` until
the attempt becomes `healthy`. If a replacement fails, `active_commit` still
identifies the previous working preview. To remove an environment, send an
authenticated `DELETE /api/environments/{project}/{intent}`. Retry the same
request if Docker cleanup reports a failure.

## GitHub metadata

To enable GitHub titles and live metadata on detail pages, set `PREVIEW_GITHUB_TOKEN` in the controller environment. Use a fine-grained token restricted to the configured
repositories with read access to Issues and Pull requests.

The dashboard uses the newest nonempty `issue_title` supplied by a deployment.
When no attempt has a title, the controller looks up the issue for that
configured project in the background, including for archived environments.
Open dashboards update when the title arrives. Failed lookups retain the last
known title, or show the issue number if no title is available. These lookups
supply operator pages only.

The detail page uses the existing deployment Intent number to find the issue in
the project's configured repository. Related tickets are direct, same-repository
GitHub sub-issues. PRs targeting that repository appear when GitHub records a
closing reference to the Intent or one of those sub-issues. For a single chain,
the list runs from the lowest PR to the tip by matching each PR's base repository
and branch to another PR's head. When the relationships are ambiguous or the
results are incomplete, the page lists available PRs by number and labels the
stack order unavailable.

Each lookup reads up to 20 sub-issues and 20 closing PR references per issue.
The page flags incomplete results. The deployment-supplied title remains
available when the live title cannot be fetched.

### Single-container Docker projects

For a project with one web process, configure the project entry in `projects.json` like this:

```json
{
  "backend": "docker",
  "repository": "https://github.com/example/web",
  "health_path": "/health"
}
```

The controller builds the root `Dockerfile` with the authorized checkout as its context and starts the image's default command. The application must listen on a container network interface and serve HTTP. If the built image exposes exactly one TCP port, the controller uses it. Otherwise set `public_port` to the internal TCP port. Docker allocates a separate loopback host port for each attempt. The stable preview hostname switches only after the candidate passes its health check.

Use the default `compose` backend for projects with dependencies or multiple services. Its `compose_file`, `public_service`, interpolation settings and default internal port of 8080 still apply.

### Dashboard sign-in with Pocket ID

Leave the dashboard sign-in variables empty for the default open dashboard. Everyone who can reach it can operate previews. To require browser sign-in, keep the private management hostname behind HTTPS and follow these steps.

1. In Pocket ID, create View and Operate groups or reuse existing groups. Use each group's **Name** in the controller configuration. Pocket ID sends these names in the signed ID token when the client requests the `groups` scope. [Pocket ID scopes and claims](https://pocket-id.org/docs/guides/scopes-and-claims)
2. Register a dedicated **public OIDC client** with PKCE enabled. Set its exact callback URL to `https://preview.example.com/dashboard/auth/callback`, substituting your management hostname. Allow both groups on this client. Keep the MCP client's existing operator-only group restriction.
3. Set these variables in the controller environment, using the issuer and client ID from Pocket ID and your own group names. When MCP OAuth is enabled, both clients use the same issuer.

```dotenv
PREVIEW_DASHBOARD_ORIGIN=https://preview.example.com
PREVIEW_DASHBOARD_ISSUER=https://id.example.com
PREVIEW_DASHBOARD_CLIENT_ID=your-dashboard-client-id
PREVIEW_DASHBOARD_OPERATE_GROUP=preview-operators
PREVIEW_DASHBOARD_VIEW_GROUP=preview-viewers
```

Recreate the Compose service (or restart the native service) and open the dashboard. View members can read all dashboard pages, host metrics, events and live logs. Operate members can also archive, restore and delete previews. Operate wins when both groups match. Users in neither group receive no dashboard access.

Group membership is checked at sign-in. A session expires at the earlier of one hour or ID token expiry. Signing out ends the local session and its streams. Restarting the controller clears all sessions. The next sign-in picks up group changes. The configured issuer must be reachable during startup for discovery.

Dashboard authentication applies only to the dashboard. Keep the management listener private since REST reads still trust network access. MCP and preview hostnames retain their own access policies.
