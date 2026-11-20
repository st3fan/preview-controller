# Preview Controller

A self-hosted controller for stable preview environments associated with SDLC Intents.

The current implementation slice accepts an authenticated candidate commit,
checks that the commit exists in an allowlisted public GitHub repository, and
records deployment history in JSON. A successful check has status `verified`.
Building a Compose stack and serving the preview URL are the next two slices,
tracked in [#6](https://github.com/st3fan/preview-controller/issues/6) and
[#7](https://github.com/st3fan/preview-controller/issues/7).

## Build and run

Use Go 1.27 or newer. Build a Linux executable with CGO disabled; set
`GOARCH` to the preview host's architecture when cross-compiling:

```sh
GOOS=linux CGO_ENABLED=0 go build -o preview-server ./cmd/preview-server
```

Copy [the example settings](config/preview-server.example.json) to
`/etc/preview-server.json` and adjust the project allowlist, state root, and
management listener for the host. Put the bearer secret in the environment
variable named by `deployment_token_env`; the settings file contains no secret
or deployment state. The management listener accepts only a loopback or private
IP address. The service account needs write access to the state root.

```sh
PREVIEW_SERVER_TOKEN='replace-with-a-secret' ./preview-server -config /etc/preview-server.json
```

The controller writes one atomic JSON file per environment at
`<state-root>/<project>/<intent>.json`. It records attempts and errors there;
Docker will remain the source of runtime container state once the runtime
slice is implemented.

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

Only public `https://github.com/owner/repo` repositories are supported in this
first version. Git operations use `go-git`; the server does not run the Git CLI
or use Git credentials.
