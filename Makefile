# MockVision build and test entry points. The binary embeds the panel, so
# `make build` builds the frontend first.

GO ?= go
NPM ?= npm
VERSION ?= 0.1.0-dev+$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X github.com/corticoide/mockvision/backend/internal/buildinfo.Version=$(VERSION)
BIN := bin/mockvision
# Dropping privileges changes every thread at once, which needs a pure Go binary.
GOBUILD := CGO_ENABLED=0 $(GO)

.PHONY: all build frontend backend dev test test-integration e2e e2e-compose generate docker clean

all: build

build: frontend backend

frontend: frontend/node_modules
	cd frontend && $(NPM) run build

frontend/node_modules: frontend/package-lock.json
	cd frontend && $(NPM) ci --no-audit --no-fund
	@touch $@

backend:
	$(GOBUILD) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./backend/cmd/mockvision

# Development without privileges: cameras answer on 127.0.0.1.
dev: backend
	$(BIN) serve --net local --data ./data --listen 127.0.0.1:8080

test: frontend/node_modules
	$(GOBUILD) vet ./...
	$(GOBUILD) test ./...
	cd frontend && $(NPM) run typecheck

# Network namespaces and macvlan: builds the tests, runs them as root.
test-integration:
	$(GOBUILD) test -c -tags integration -o bin/netctl.test ./backend/internal/netctl
	sudo bin/netctl.test -test.v -test.count=1

# Acceptance criteria on an isolated virtual LAN (root, iproute2, ffmpeg, curl, python3).
e2e: backend
	sudo E2E_BIN=$(CURDIR)/$(BIN) backend/e2e/run.sh

e2e-compose:
	sudo E2E_MODE=compose backend/e2e/run.sh

generate:
	cd database && sqlc generate
	cd frontend && $(NPM) run gen:api

docker:
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t mockvision:latest .

clean:
	rm -rf bin
	find frontend/dist -mindepth 1 ! -name .gitkeep -delete
