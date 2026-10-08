# syntax=docker/dockerfile:1.7
# wolf-bridge sidecar: exposes Wolf's unix socket API to the hub.

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
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/wolf-bridge ./cmd/wolf-bridge

# Runs as root in the pod (Wolf's socket is root-owned); distroless keeps
# the attack surface to one static binary.
FROM gcr.io/distroless/static-debian12:latest
ARG VERSION=dev
LABEL org.opencontainers.image.title="games-operator-bridge" \
      org.opencontainers.image.source="https://github.com/dseif0x/games-operator" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/wolf-bridge /wolf-bridge
EXPOSE 8443
ENTRYPOINT ["/wolf-bridge"]
