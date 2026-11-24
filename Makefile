GO ?= go
export PATH := $(HOME)/.local/go/bin:$(HOME)/go/bin:$(PATH)

.PHONY: all build test test-short db db-down minio test-minio test-backup generate lint fmt

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
	docker compose --profile minio down

# MinIO on 127.0.0.1:59000, for tests of the S3 blob store.
minio:
	docker compose --profile minio up -d minio

MINIO_ENV = VX_TEST_S3_URL='s3://vx-test?endpoint=http://127.0.0.1:59000' VX_TEST_S3_ACCESS_KEY_ID=vxdev VX_TEST_S3_SECRET_ACCESS_KEY=vxdevsecret

# The blob store against MinIO (the rest of the suite uses an in-process fake).
test-minio: minio
	$(MINIO_ENV) VX_REQUIRE_MINIO=1 $(GO) test -count=1 -run MinIO ./internal/blob/

# Back up a throwaway server into MinIO and restore it on a fresh one.
test-backup:
	bash deploy/production/backup_test.sh

generate:
	buf lint
	buf generate

lint:
	gofmt -l . | (! grep .)
	$(GO) vet ./...
	buf lint

fmt:
	gofmt -w cmd internal
