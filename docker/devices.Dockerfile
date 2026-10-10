# syntax=docker/dockerfile:1.7
# games-operator-devices: the device plugin that hands app pods their input
# device numbers (internal/devices), one per game node.

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/games-operator-devices ./cmd/games-operator-devices

# Debian for nsenter: the modules (uinput, uhid, hidraw) are loaded on the
# node through the host's namespaces.
FROM debian:bookworm-slim
ARG VERSION=dev
LABEL org.opencontainers.image.title="games-operator-devices" \
      org.opencontainers.image.source="https://github.com/dseif0x/games-operator" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/games-operator-devices /games-operator-devices
ENTRYPOINT ["/games-operator-devices"]
