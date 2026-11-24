# vx: notes for Claude

vx is a Go CLI and server: a workbench where coding agents land work into shared efforts before it goes to git as pull requests. Read README.md and docs/architecture.md for the big picture.

## Build and test

```sh
make db                               # Postgres in Docker on 127.0.0.1:54329 (needed for most tests)
VX_REQUIRE_DB=1 go test -race ./...   # full suite; tests skip without a database unless VX_REQUIRE_DB is set
go test ./internal/merge/ -run TestMerge3   # one package or test
make build                            # bin/vx, bin/vx-server
make generate                         # after editing proto/vx/v1/vx.proto (needs buf, protoc-gen-go, protoc-gen-connect-go)
make lint                             # gofmt, go vet, buf lint
```

Run the full suite before saying a change works.

## Layout

- `cmd/vx`, `cmd/vx-server`: binaries; keep them thin.
- `internal/service`: application logic. No HTTP here.
- `internal/api`: connect-go handlers that adapt the service. No business logic here.
- `internal/store`: Postgres. New schema changes are new files in `internal/store/migrations/` (NNN_name.sql); never edit an existing migration.
- `internal/engine`, `internal/merge`, `internal/object`: pure logic with property tests; keep them free of I/O beyond `blob.Store`.
- `internal/web`: server-rendered UI (html/template, embedded static files, strict CSP: no inline scripts or styles).
- `internal/cli`: the `vx` command; `e2e_test.go` runs the real CLI against an in-process server.
- `plugins/vx`: the Claude Code plugin users install. Validate with `claude plugin validate ./plugins/vx --strict` and `claude plugin validate . --strict`.

## Conventions

- Errors that reach users say what happened and what to do next.
- Every CLI command supports `--json`; exit code 2 means conflicts.
- Tests that need Postgres call `storetest.New(t)`, which creates and drops a fresh database.
- html/template already escapes values in URLs: do not pre-escape query values in templates.
- vx only ever pushes branches under `vx/` and never merges; keep it that way.
- Do not use em-dashes in user-facing text or docs.
