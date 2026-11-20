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

## Native executable and systemd

From the repository checkout, build with
`GOOS=linux CGO_ENABLED=0 go build -o preview-server ./cmd/preview-server`
and install the executable at `/usr/local/bin/preview-server`. For a new
installation, create `/etc/preview-server.json` from
[`config/preview-server.example.json`](../config/preview-server.example.json)
in the same checkout so the executable and example use the same settings schema.
Set both listeners to loopback when Caddy runs on the same host. Set
`state_root` and `workspace_root` to separate, dedicated directories owned by
the service account. Place the bearer token in `/etc/preview-server.env` with
mode `0600`:

```sh
PREVIEW_SERVER_TOKEN=replace-with-a-long-random-secret
```

An example systemd unit is:

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
ExecStart=/usr/local/bin/preview-server -config /etc/preview-server.json
Restart=on-failure
UMask=0077
NoNewPrivileges=true
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

## Docker Compose

Run the shipped Compose definition from the repository checkout on the Linux
preview host. It builds the controller image locally. Copy the
[example JSON](../config/preview-server.example.json) to
`/etc/preview-server.json` and configure your project allowlist and preview
domain. Keep these settings for the shipped definition:

- `state_root` is `/var/lib/preview-server`, backed by the Compose `state` volume.
- `workspace_root` is `/srv/preview-server`. Create this as a real directory on
  the host. The controller runs as root in the shipped image, so use
  `sudo install -d -o root -g root -m 0700 /srv/preview-server` for a new workspace.
- `deployment_token_env` is `PREVIEW_SERVER_TOKEN`.

To use a different config file, export `PREVIEW_SERVER_CONFIG` with its absolute
host path. To use a different workspace, export `PREVIEW_SERVER_WORKSPACE` with
the same absolute path as `workspace_root`. The host daemon resolves preview bind
mounts using that path, so use a dedicated host directory with no symlink
components. Keep the state and workspace paths separate.

Load the deployment token into the shell environment before starting the service.
For an interactive Bash session:

```bash
read -rsp 'Deployment token: ' PREVIEW_SERVER_TOKEN
export PREVIEW_SERVER_TOKEN
docker compose up -d --build
unset PREVIEW_SERVER_TOKEN
```

Supply the same token when recreating the service. Any variables referenced by
`compose_env_from` also need to be passed through the controller service environment.
Treat access to Docker as access to the controller's environment and credentials.

The controller uses host networking to reach preview ports on the host's loopback
interface. Keep both JSON listeners on loopback when Caddy runs on the same host.
The controller service mounts the rootful host Docker socket, which grants
host-level control. Preview services still use the controller's restricted Compose
policy and run as separate projects.

Keep the same Compose project name across upgrades so the controller reuses its
state volume. Remove active previews through the management API before uninstalling.
Stopping the controller leaves preview projects running, and removing the named
state volume discards the controller's deployment records.

## Portainer with a published image

Use a Portainer Docker Standalone environment on a dedicated Linux host
running amd64 or arm64. Prepare `/etc/preview-server.json` using
`config/preview-server.example.json` from the GitHub Release's source tree.
Configure the public repository allowlist and the preview domain. Use the
[state, workspace and token settings above](#docker-compose), including
creating the workspace directory on the host.

1. Create a stack in Portainer and paste the contents of
   `compose.portainer.yaml` from the same release into the web editor.
2. Set the stack environment variables `PREVIEW_SERVER_IMAGE` and
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
preview services. Use the optional `PREVIEW_SERVER_CONFIG` and
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

Match the Caddy upstream to `preview_listen`. Keep the management API on its
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

## Recover from an unknown settings field

If startup fails with `decode settings: json: unknown field "preview_listen"`,
the installed executable may predate the settings file. A typo or an
unsupported field can produce the same error.

1. On the preview host, run `systemctl cat preview-server` to review the unit
   and its drop-ins. Find the executable path in the loaded service settings with
   `systemctl show -p ExecStart preview-server`. For the example unit above,
   inspect it with `go version -m /usr/local/bin/preview-server`. If Go is only
   installed on the build machine, copy the installed binary there for inspection.
2. Compare the reported `vcs.revision`, if present, with `git rev-parse HEAD`
   in the checkout that supplied the settings example. If revision metadata is
   absent, rebuild from the intended checkout to establish which revision is installed.
3. Run the build command above from that checkout. On the preview host, use
   `sudo install -m 0755 preview-server /usr/local/bin/preview-server`, substituting
   the executable path from `ExecStart` if it differs. Keep the existing settings
   file. If an unknown field persists, compare the settings with the example
   from the build checkout and correct the field while preserving your local values.
4. Run `sudo systemctl restart preview-server` and check
   `systemctl status preview-server` for successful startup.

## GitHub metadata

To show live GitHub information on a preview detail page, set
`"github_token_env": "PREVIEW_GITHUB_TOKEN"` in the controller's JSON settings
and pass `PREVIEW_GITHUB_TOKEN` in the controller process environment. For systemd
use the service's environment file. The supplied Compose files pass
`PREVIEW_GITHUB_TOKEN` through from the Compose environment, including a local
`.env` file or a Portainer stack variable. Use a fine-grained token restricted to the configured
repositories with read access to Issues and Pull requests.

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
