# Preview Controller

A self-hosted controller for stable preview environments associated with SDLC Intents.

The controller accepts an authenticated candidate commit, verifies it in an
allowlisted public GitHub repository, and builds a separate Docker Compose
stack. A healthy candidate remains available on a loopback port. Serving the
stable preview URL and replacing the active target are tracked in
[#7](https://github.com/st3fan/preview-controller/issues/7).

## Build and run

Use Go 1.27 or newer. Build a Linux executable with CGO disabled; set
`GOARCH` to the preview host's architecture when cross-compiling:

```sh
GOOS=linux CGO_ENABLED=0 go build -o preview-server ./cmd/preview-server
```

Copy [the example settings](config/preview-server.example.json) to
`/etc/preview-server.json` and adjust the project allowlist, state root,
workspace root, and management listener for the host. Put the bearer secret in
the environment variable named by `deployment_token_env`; the settings file
contains no secret or deployment state. The management listener accepts only a loopback or private
IP address. The service account needs write access to the state and workspace
roots and access to the local Docker daemon. Run it only on a dedicated preview
host; Docker builds and containers can execute untrusted project code there.

```sh
PREVIEW_SERVER_TOKEN='replace-with-a-secret' ./preview-server -config /etc/preview-server.json
```

The controller writes one atomic JSON file per environment at
`<state-root>/<project>/<intent>.json`. It records attempts and errors there.
Docker is the source of runtime container state.

## Deployment API

The management API is restricted to the configured home-lab listener.
Candidate requests require a full 40-character commit SHA and the bearer
secret. The authenticated request is the approval to preview that commit.

```http
POST /api/deployments
Authorization: Bearer <shared-secret>
Content-Type: application/json

{"project":"myproject","intent":123,"commit":"<40-character-commit-sha>"}
```

The API returns HTTP 202 with a deployment ID, `queued` status, and a stable
URL such as `https://123-myproject.preview.example.com`. Verification proceeds
asynchronously. Read status through `GET /api/deployments/{id}`,
`GET /api/environments`, and `GET /api/environments/{project:intent}`.

The project's Compose file is restricted to services and named volumes with a
small set of service options. Build contexts, Dockerfiles, and bind mounts must
resolve inside the checkout. The controller rejects host access, external
Compose files, interpolation, public port mappings, and unsupported options.
It creates a unique Compose project per attempt and publishes only the chosen
service on a random loopback port. A candidate passes the configured HTTP
health path before its status becomes `healthy`. Failed candidates are cleaned
up; existing stacks are left running.

On a Linux host with Docker Compose, run the opt-in fixture integration test
with `CGO_ENABLED=0 go test -tags=integration ./internal/runtime`. It builds
and starts fixture containers and then removes them.

Only public `https://github.com/owner/repo` repositories are supported in this
first version. Git operations use `go-git`; the server does not run the Git CLI
or use Git credentials.
