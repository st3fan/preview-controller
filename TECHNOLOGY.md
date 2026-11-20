# Technology

## Application stack

- Build the controller as a Go service. Prefer the standard library for HTTP handlers and server-rendered HTML; use `html/template` and locally served HTMX for the dashboard.
- Use SQLite for environment metadata, port assignments, deployed commits, and deployment history. Docker is the source of truth for container runtime state.
- Fetch source directly from allowlisted GitHub repositories and build the exact authorized commit locally. Use Docker Compose to give each Intent its own complete application stack, including databases and other dependencies.
- Use Caddy for HTTPS and reverse proxying to stable Intent URLs.

## Packaging and distribution

- Build a native Go executable for Linux. For the first version, install it from a repository source build on the home-lab server.
- Release artifacts and an automated distribution channel are undecided.

## Hosting and deployment

- Run the controller as a systemd-managed service on a self-hosted Linux home-lab server. Run preview environments as separate Docker Compose projects on that host.
- Keep the dashboard and deployment API within the home-lab network. Authenticate deployment requests with a shared bearer secret. Require an allowlisted repository and verified preview authorization before executing a requested commit.
- Deploy replacement stacks alongside the current environment and switch Caddy traffic only after health checks pass. Preserve the previous working preview if a replacement fails.

## Development and quality

- Use Go modules and standard Go formatting. Favor small, explicit packages and standard-library components over a large web framework.
- Run Go unit tests and static checks for controller logic. Use targeted Docker Compose integration checks for build, health, replacement, and cleanup behavior on Linux.

## Rationale and open choices

- Per-Intent Compose stacks favor isolation and predictable behavior over lower memory use. A native controller and SQLite fit a single-host first version without a registry, GitHub Actions preview-build pipeline, or Kubernetes.
- The exact GitHub preview-authorization signal, Docker/Compose execution restrictions, Caddy update mechanism, and release packaging remain to be defined during architecture and Intent planning.
- Treat fetched repository code and Compose configuration as code that can execute on or affect the host; do not assume Compose isolation alone is a security boundary.
