GO_DIR := mycode-go
WEB_DIR := web
DIST_DIR := $(CURDIR)/dist
BIN := $(DIST_DIR)/mycode-go

.DEFAULT_GOAL := help

.PHONY: help
help:
	@printf '%s\n' \
		'Targets:' \
		'  make dev          Start backend and frontend dev servers.' \
		'  make run-web      Run the Go web command with go run.' \
		'  make web-install  Install web dependencies.' \
		'  make web-dev      Start only the frontend dev server.' \
		'  make web-check    Run web lint, typecheck, and tests.' \
		'  make web-build    Build web assets and sync them for embedding.' \
		'  make fmt          Format Go code.' \
		'  make lint         Lint Go code.' \
		'  make test         Run Go tests.' \
		'  make check        Run all Go and web checks.' \
		'  make build        Build the embedded binary.' \
		'  make clean        Remove build outputs.'

.PHONY: dev
dev:
	@trap 'kill 0' INT TERM EXIT; \
	go -C $(GO_DIR) run ./cmd/mycode-go web --dev & \
	pnpm --dir $(WEB_DIR) dev & \
	wait

.PHONY: run-web
run-web:
	go -C $(GO_DIR) run ./cmd/mycode-go web

.PHONY: web-dev
web-dev:
	pnpm --dir $(WEB_DIR) dev

.PHONY: web-install
web-install:
	pnpm --dir $(WEB_DIR) install --frozen-lockfile

.PHONY: web-check
web-check:
	pnpm --dir $(WEB_DIR) check
	pnpm --dir $(WEB_DIR) typecheck
	pnpm --dir $(WEB_DIR) test:run

.PHONY: web-build
web-build: web-install
	pnpm --dir $(WEB_DIR) build
	./scripts/sync_web_dist.sh

.PHONY: fmt
fmt:
	golangci-lint fmt ./$(GO_DIR)/...

.PHONY: lint
lint:
	golangci-lint run ./$(GO_DIR)/...

.PHONY: test
test:
	go -C $(GO_DIR) test ./...

.PHONY: check
check:
	go -C $(GO_DIR) vet ./...
	go -C $(GO_DIR) test -race ./...
	golangci-lint run ./$(GO_DIR)/...
	$(MAKE) web-check

.PHONY: build
build: web-build
	mkdir -p $(DIST_DIR)
	go -C $(GO_DIR) build -tags embedweb -o $(BIN) ./cmd/mycode-go

.PHONY: clean
clean:
	rm -rf $(DIST_DIR)
	rm -rf $(GO_DIR)/internal/server/webdist
