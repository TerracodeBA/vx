# Contributing to vx

Thanks for helping. vx is early, so issues, design discussion and code are all welcome.

## Ways to help

- **Try it and report what breaks.** Bug reports with exact commands and output are the most useful thing right now.
- **Pick an issue.** Look for `good first issue` and `help wanted`. The [organizations roadmap](docs/organizations.md) lists the larger pieces.
- **Discuss design** in [GitHub Discussions](https://github.com/TerracodeBA/vx/discussions) before starting anything large, so we agree on the approach first.

## Development setup

You need Go (see `go.mod` for the version), Docker, and optionally [buf](https://buf.build) if you change the API.

```sh
git clone https://github.com/TerracodeBA/vx && cd vx
make db          # Postgres in Docker on 127.0.0.1:54329
make test        # full test suite with the race detector
make build       # bin/vx and bin/vx-server
```

Working with Claude Code? The repository's `CLAUDE.md` tells Claude how to build, test and follow the conventions below.

## Guidelines

- **Tests come with changes.** Logic gets unit tests; behaviour that crosses the API gets a test in `internal/cli/e2e_test.go` or `internal/web/web_test.go`. Tests that need Postgres use `storetest.New(t)`.
- **Keep packages focused.** See the layout in [docs/architecture.md](docs/architecture.md). The service layer does not know about HTTP; handlers do not contain business logic.
- **API changes** go in `proto/vx/v1/vx.proto`, then `make generate`. Keep fields backward compatible.
- **Database changes** are new numbered files in `internal/store/migrations/`; never edit a released migration.
- **User-facing text** is plain and specific: say what happened and what to do next.
- **Formatting:** `make lint` (gofmt, go vet, buf lint) must pass.

## Pull requests

- One topic per pull request, with a description of what changed and why.
- CI must be green.
- By contributing you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).

## Reporting security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
