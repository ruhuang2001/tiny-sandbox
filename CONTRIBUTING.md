# Contributing

Requires Go 1.27 or newer and a reachable Docker daemon. Build the server and
CLI with `make build`; see the [quick start](README.md#quick-start) for usage.

## Local checks

```sh
make test     # unit tests
make race     # race detector
make vet      # go vet
make lint     # golangci-lint
```

```sh
E2E_COVERAGE=1 bash scripts/e2e.sh artifacts/e2e
mkdir -p artifacts/cov-unit
go test -race -covermode=atomic -coverpkg=./... ./... \
  -args -test.gocoverdir="$PWD/artifacts/cov-unit"
bash scripts/coverage.sh artifacts/cov-unit artifacts/e2e/coverage 70
goreleaser release --snapshot --clean       # six archives plus checksums
```

CI runs five jobs: `lint`, `test` (race plus coverage), `build` (every command,
`goreleaser check`, a snapshot, checksum verification), `e2e` (real Docker,
merged coverage at or above 70%, and an assertion that no containers remain),
and `python-sdk` (Python 3.10 and 3.15, package builds and real-Docker E2E).
See [versioning and release checks](docs/versioning.md) for release preparation.

The Python SDK requires Python 3.10+ and has no runtime dependencies. Its
behavioral gate uses real Docker and an HTTP fault proxy, with repeatable
evidence retained even on failure:

```sh
python3 -m pip install build
make python-build
make python-e2e
```

`python-e2e` creates a virtual environment and installs a wheel. To exercise an
already installed wheel, set `SDK_INSTALLED=1` and select its interpreter with
`PYTHON`. Write the failure model before implementation when testing an isolated
system. Prefer end-to-end coverage for new behavior; do not add unit tests after
writing the implementation. See the [Python SDK contract](docs/sdk-python.md)
and [failure model](sandbox-python/e2e/FAILURES.md).

The Docker-backed Go tests are opt-in:

```sh
go test -tags docker ./internal/dockerbackend/ -run TestReal
```

## Commits

Subjects follow `type(scope): summary`; the scope is required.

- Types: `feat`, `fix`, `style`, `docs`, `refactor`, `test`, `chore`, `build`, `ci`
- Scope: lowercase kebab-case, for example `sdk`, `cli`, `daemon`, `docker`, `contracts`
- Summary: starts lowercase; the entire subject is at most 72 characters
- `!` before the colon marks a breaking change

```text
feat(sdk): resume streams from the last event id
fix(daemon): classify external stop as evicted, not exited
```

A tracked `commit-msg` hook rejects anything else once per clone:

```sh
make install-hooks   # point git at .githooks
make check-commit    # validate the latest commit
```

## Architecture and contracts

Read the [design](docs/design.md) before changing execution or lifecycle
behavior. The [HTTP API](docs/api.md), [Go SDK](docs/sdk.md),
[Python SDK](docs/sdk-python.md), and
[CLI](docs/cli.md) documents define the public contracts; update them with
any behavior changes.

For changes to execution, cancellation, streaming, or container lifecycle,
prefer end-to-end scenarios against real Docker. Keep repeatable evidence in
`artifacts/`, including commands, responses, logs, and cleanup results.
Only report checks that actually ran.
