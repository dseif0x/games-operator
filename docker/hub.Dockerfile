# syntax=docker/dockerfile:1.7
# Hub image: Node builds the SPA, Go embeds it, distroless runs it.

FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

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
COPY --from=web /src/internal/ui/dist internal/ui/dist
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -tags ui -ldflags="-s -w -X main.version=${VERSION}" -o /out/games-operator ./cmd/games-operator

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="games-operator" \
      org.opencontainers.image.description="Game streaming on Kubernetes: a Moonlight-compatible host whose apps run as pods next to Wolf" \
      org.opencontainers.image.source="https://github.com/dseif0x/games-operator" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/games-operator /games-operator
USER nonroot:nonroot
EXPOSE 8080 47989 47984
ENTRYPOINT ["/games-operator"]
