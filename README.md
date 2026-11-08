# tiny-sandbox

A small, complete Go implementation of a code-execution sandbox for AI agents:
an SDK, a CLI, and a reference server that runs sandboxes as Docker containers.

The server is a reference implementation, not a product. It exists so the SDK
and CLI can be exercised end to end against real containers, with real
cancellation, pause/resume, and OOM classification.

**Status:** scaffold. The contracts and design are complete; the implementation
is in progress.

```
cmd/sandboxd/            reference server (Docker backend)
cmd/sandboxctl/          developer CLI
internal/daemon/         state machine, execution engine, classification
internal/dockerapi/      minimal Docker Engine API client
internal/dockerbackend/  engine-backed Backend implementation
sandbox-go/              Go SDK (package sandbox)
docs/                    design and normative contracts
```

## Documentation

- [docs/design.md](docs/design.md) — architecture, decisions, verification
- [docs/api.md](docs/api.md) — HTTP contract
- [docs/sdk.md](docs/sdk.md) — Go SDK contract
- [docs/cli.md](docs/cli.md) — CLI contract
- [docs/versioning.md](docs/versioning.md) — versioning and release checks
- [docs/future.md](docs/future.md) — deliberate non-goals and what comes next

## Quick start

Requires Go 1.27 or newer.

```sh
make build
./bin/sandboxd &
./bin/sandboxctl version
curl -s localhost:8080/healthz
```

## Development

```sh
make test     # unit tests
make race     # race detector
make vet      # go vet
make lint     # golangci-lint
```

CI runs lint, vet, race tests, and a build of every command on push and pull
request. Releases are built by GoReleaser from a tag.

## Commits

Subjects follow `type(scope): summary`; the scope is required.

- Types: `feat`, `fix`, `style`, `docs`, `refactor`, `test`, `chore`, `build`, `ci`
- Scope: lowercase kebab-case, for example `sdk`, `cli`, `daemon`, `docker`, `contracts`
- Summary: lowercase, at most 72 characters; `!` before the colon marks a breaking change

```text
feat(sdk): resume streams from the last event id
fix(daemon): classify external stop as evicted, not exited
```

A tracked `commit-msg` hook rejects anything else once per clone:

```sh
make install-hooks   # point git at .githooks
make check-commit    # validate the latest commit
```
