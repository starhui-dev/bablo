SHELL := /bin/sh

APP_NAME ?= bablo
GO ?= go
WEB_DIR ?= web

.PHONY: fmt lint test test-race migrate migrate-down dev build web-install web-dev

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
	$(GO) run ./cmd/bablo-migrate

migrate-down:
	BABLO_MIGRATION_ACTION=down $(GO) run ./cmd/bablo-migrate

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
