# Technology

## Application stack

- Build the controller as a Go service. Prefer standard-library HTTP handlers and server-rendered HTML; use `html/template` and locally served HTMX for the operator dashboard.
- Use `net/http/httputil.ReverseProxy` to route each preview hostname to its healthy local application port. Caddy terminates HTTPS and forwards preview requests to the controller; the controller does not modify Caddy configuration.
- Keep global settings in `/etc/preview-server.json`. Persist each environment's state and deployment history as an atomically written JSON file under a configured root, such as `<state-root>/<project>/<intent>.json`. Docker remains the source of truth for container runtime state. Do not use SQLite.
- Fetch only allowlisted public GitHub repositories over HTTPS using `github.com/go-git/go-git/v6`. Check out the exact authorized commit with the Go library; do not invoke the Git CLI or require Git credentials for the first version.
- Build locally with Docker Compose. Give each Intent a complete isolated application stack, including databases and other dependencies.

## Packaging and distribution

- Build a native Linux executable with `CGO_ENABLED=0`. Install from a repository source build, either as a native executable or as a controller image with the Docker CLI and Compose plugin.
- Release artifacts and an automated distribution channel are undecided.

## Hosting and deployment

- Run the controller on a dedicated self-hosted Linux preview server under systemd or as a Compose service. The Compose installation uses host networking, mounts the host Docker socket only in the controller service, and stores state in a named volume. Bind-mount its workspace at the same absolute path on the host and inside the controller so the host Docker daemon can resolve checkout-relative mounts. Both modes run preview attempts as separate Docker Compose projects on that host.
- The operator configures Caddy, wildcard DNS, and TLS once. Caddy forwards `*.preview.example.com` to the controller. Stable preview hostnames use the configurable flat form `<project>-<intent>.preview.example.com`.
- Restrict the dashboard and deployment API to the home-lab network. Authenticate deployment requests with a shared bearer secret. A valid authenticated request is approval for the exact commit; the controller also verifies the commit in the configured public repository.
- Build a replacement alongside the working environment. Change the Go proxy's active target only after health checks pass, and preserve the previous working preview if a replacement fails.
- Treat fetched code and Compose configuration as host-affecting inputs. Use a dedicated preview host and reject privileged containers, host networking/namespaces, devices, Docker socket access, host mounts outside the checkout, host-executed providers, and unsafe indirect file references. Unsupported Compose features fail closed.

## Development and quality

- Use Go modules and standard Go formatting. Favor small, explicit packages and standard-library components over a large web framework.
- Run Go unit tests and static checks for controller logic. Use targeted Docker Compose integration checks for build, health, replacement, proxy cutover, and cleanup on Linux.

## Rationale and open choices

- Per-Intent Compose stacks favor isolation and predictable behavior over lower memory use. Per-environment JSON files fit the small single-host first version and the CGO-free binary requirement.
- The controller's Go proxy keeps Caddy configuration static while providing stable Intent URLs. Public repositories avoid Git credential handling in the first version.
- The first version does not need a registry, GitHub Actions preview-build pipeline, or Kubernetes. Private repository support and release packaging remain future choices.
