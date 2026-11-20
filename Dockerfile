FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
ARG CONTROLLER_REVISION
ARG CONTROLLER_RELEASE
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-X github.com/st3fan/preview-controller/internal/dashboard.buildRevision=${CONTROLLER_REVISION} -X github.com/st3fan/preview-controller/internal/dashboard.buildRelease=${CONTROLLER_RELEASE}" \
    -o /preview-server ./cmd/preview-server

FROM docker:29.10.0-cli
COPY --from=build /preview-server /usr/local/bin/preview-server
ENTRYPOINT ["/usr/local/bin/preview-server"]
CMD ["-config", "/etc/preview-server.json"]
