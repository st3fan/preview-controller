FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /preview-server ./cmd/preview-server

FROM docker:29.10.0-cli
COPY --from=build /preview-server /usr/local/bin/preview-server
ENTRYPOINT ["/usr/local/bin/preview-server"]
CMD ["-config", "/etc/preview-server.json"]
