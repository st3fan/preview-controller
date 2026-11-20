# Write a project-side `preview.sh`

`preview.sh` is a client that a developer or an authorized workflow runs from
the application repository. It submits the exact commit to preview. Preview
Controller fetches that commit, builds its Docker Compose stack, checks its
health, and publishes it at the stable URL for the Intent.

## Configure the application preview

1. Inspect the application repository's existing preview and container setup.
   Reuse its conventions and tooling where they fit.
2. Identify a Compose file committed with the application, for example
   `compose.preview.yaml`. Define the application service and preview
   dependencies in that file. Keep build contexts, Dockerfiles, and any
   checkout-relative bind mounts within the checkout. Use preview-only
   configuration and follow the [Compose preview contract](../README.md#compose-preview-contract).
3. Confirm the project key, allowlisted public GitHub repository, Compose file,
   public service, container port, and HTTP health path with the controller
   operator. The application must listen on an interface reachable through
   the container port, typically `0.0.0.0`. The controller assigns the host port
   and checks the configured health path before switching the stable URL.
   See the [example controller configuration](../config/preview-server.example.json).

## Implement the client

1. Accept a positive integer Intent number and a full 40-character hexadecimal
   commit SHA. The SHA must identify a commit the controller can fetch from the
   allowlisted repository. Take the management base URL, configured project key,
   and bearer token from the calling environment or the project's existing
   configuration. Read the secret itself from the environment. Validate required
   inputs before making an HTTP request and keep the token out of logs and shell
   traces.
2. Use a JSON serializer to send the configured project key, numeric Intent,
   and candidate SHA to the management endpoint. A request has this shape:

   ```http
   POST /api/deployments
   Authorization: Bearer <token>
   Content-Type: application/json

   {"project":"myproject","intent":123,"commit":"0123456789abcdef0123456789abcdef01234567"}
   ```

   Clients can include an optional `issue_title` string to show the full Intent name
   even when the controller has no GitHub metadata access. If the project enables
   the preview inspector, this submitted title is also visible to preview visitors.

3. On HTTP 202, print the response's deployment `id` and stable preview `url`.
   The response's `queued` status means the request was accepted for asynchronous
   deployment. On a request failure, exit nonzero and report the HTTP status and
   controller error when available.
4. If the project needs a wait mode, poll `GET /api/deployments/{id}` with a
   finite deadline. Report the attempt's `error` on `failed`. Treat `retired` and
   timeout as unsuccessful. Before claiming a `healthy` attempt is the one the
   stable URL currently serves, check that `GET /api/environments/{project}/{intent}`
   returns its ID as `active_deployment_id`.

Use a stub HTTP server to verify input validation, request headers and JSON,
handling of HTTP 202 and API errors, and any wait behavior. A real submission
authorizes deployment of the supplied SHA, so get deployment approval before
running the script against the live controller.

## Prompt for an application repository agent

> Read the Preview Controller guide at
> https://github.com/st3fan/preview-controller/blob/main/docs/preview-script.md.
> Inspect this repository's existing preview setup and use the guide to create
> its `preview.sh`. Verify the script against a stub controller.
