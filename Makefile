GO ?= go
export PATH := $(HOME)/.local/go/bin:$(HOME)/go/bin:$(PATH)

.PHONY: all build test test-short db db-down generate lint fmt

all: build test

build:
	$(GO) build -o bin/ ./cmd/...

# Full test suite against the docker-compose database.
test: db
	VX_REQUIRE_DB=1 $(GO) test -race ./...

# Tests that need no database.
test-short:
	$(GO) test ./...

db:
	docker compose up -d --wait postgres

db-down:
	docker compose down

generate:
	buf lint
	buf generate

lint:
	gofmt -l . | (! grep .)
	$(GO) vet ./...
	buf lint

fmt:
	gofmt -w cmd internal
