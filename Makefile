SHELL := /bin/sh

APP_NAME ?= bablo
GO ?= go
WEB_DIR ?= web

.PHONY: fmt lint test test-race migrate dev build web-install web-dev

fmt:
	$(GO) fmt ./...

lint:
	$(GO) vet ./...
	cd $(WEB_DIR) && pnpm lint

test:
	$(GO) test ./...
	cd $(WEB_DIR) && pnpm test

test-race:
	$(GO) test -race ./...

migrate:
	@echo "no migrations exist yet; run bablo-data before using this target" >&2
	@exit 1

dev:
	$(GO) run ./cmd/bablo

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "-s -w" -o bin/$(APP_NAME) ./cmd/bablo
	cd $(WEB_DIR) && pnpm build

web-install:
	cd $(WEB_DIR) && pnpm install

web-dev:
	cd $(WEB_DIR) && pnpm dev
