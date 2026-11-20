# Preview Controller

A self-hosted controller for stable preview environments associated with SDLC Intents.

The controller accepts an authenticated candidate commit, verifies it in an
allowlisted public GitHub repository, and builds a separate Docker Compose
stack. When the new stack passes health checks, its stable preview URL switches
to the new loopback port. The previous stack stays available if the candidate
fails.

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

## Deployment API

The management API and read-only dashboard are restricted to the configured home-lab listener.
Open `/` on that listener to see preview links, active commits, and live deployment
status grouped by project. Select **Live logs** next to a deployment attempt to
see its last 100 lines per Compose service container, followed by new output.
The log URL stays tied to that attempt during replacement. Logs become
unavailable once the attempt is removed. Treat log URLs and output as operator
information.

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
