SHELL := /bin/bash
.DEFAULT_GOAL := build

VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
OWNER        ?= dseif0x
HUB_IMAGE    ?= ghcr.io/$(OWNER)/games-operator
BRIDGE_IMAGE ?= ghcr.io/$(OWNER)/games-operator-bridge
PLATFORMS    ?= linux/amd64,linux/arm64
LDFLAGS      := -s -w -X main.version=$(VERSION)

.PHONY: build ui hub bridge test test-pg lint fmt image image-hub image-bridge chart chart-deps chart-docs dev dev-db dev-db-stop hash-password clean

## build: frontend first, then both binaries with the UI embedded
build: ui hub bridge

ui: web/node_modules
	cd web && npm run build

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci --no-audit --no-fund

hub:
	CGO_ENABLED=0 go build -tags ui -ldflags "$(LDFLAGS)" -o bin/games-operator ./cmd/games-operator

bridge:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/wolf-bridge ./cmd/wolf-bridge

## test: Go tests (Postgres tests run when GAMES_OPERATOR_TEST_DATABASE_URL is set)
test:
	go test -race -count=1 ./...

## test-pg: Go tests against the compose Postgres
test-pg: dev-db
	GAMES_OPERATOR_TEST_DATABASE_URL=postgres://games_operator:games_operator@localhost:5433/games_operator_test?sslmode=disable go test -race -count=1 ./...

## lint: vet, golangci-lint, typecheck, helm lint
lint:
	go vet ./...
	golangci-lint run ./...
	cd web && npm run typecheck
	helm lint charts/games-operator --strict

fmt:
	gofmt -w cmd internal

image: image-hub image-bridge

image-hub:
	docker build -f docker/hub.Dockerfile --build-arg VERSION=$(VERSION) -t $(HUB_IMAGE):$(VERSION) .

image-bridge:
	docker build -f docker/bridge.Dockerfile --build-arg VERSION=$(VERSION) -t $(BRIDGE_IMAGE):$(VERSION) .

## chart: package the chart into dist/
chart: chart-deps
	helm lint charts/games-operator --strict
	mkdir -p dist && helm package charts/games-operator -d dist

chart-deps:
	helm dependency update charts/games-operator

chart-docs:
	go run github.com/norwoodj/helm-docs/cmd/helm-docs@v1.14.2 --chart-search-root=charts

## dev: run the hub against a kubeconfig and the compose Postgres
dev: dev-db
	@test -f hack/dev.env || cp hack/dev.env.example hack/dev.env
	set -a && source hack/dev.env && set +a && go run -tags ui ./cmd/games-operator

dev-db:
	docker compose -f hack/docker-compose.yml up -d --wait

dev-db-stop:
	docker compose -f hack/docker-compose.yml down

hash-password:
	go run ./cmd/games-operator hash-password

clean:
	rm -rf bin dist internal/ui/dist web/node_modules
