# Linux installation and ingress

Use a dedicated Linux preview host. Docker access is effectively host-level
access: repository builds and Compose services execute untrusted project code.
Do not place unrelated workloads or secrets on this host.

## Prerequisites

- Go 1.27 or newer on the build machine, Docker Engine and Docker Compose on
  the Linux host, and outbound HTTPS access to public GitHub repositories.
- Wildcard DNS for `*.preview.example.com` pointing at Caddy and a wildcard TLS
  certificate or Caddy DNS challenge setup. A wildcard covers the flat
  `<project>-<intent>.preview.example.com` hostname.
- A dedicated service account that can access the Docker daemon. Membership in
  the `docker` group grants powerful host access; restrict it to this host.

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
