# tiny-sandbox

A small, complete Go implementation of a code-execution sandbox for AI agents:
an SDK, a CLI, and a reference server that runs sandboxes as Docker containers.

The server is a reference implementation, not a product. It exists so the SDK
and CLI can be exercised end to end against real containers, with real
cancellation, pause/resume, and OOM classification.

**Status:** complete. Contract, server, Docker backend, SDK, CLI, CI and the
release snapshot are implemented and verified; see `progress.md` for the
per-stage evidence and [docs/future.md](docs/future.md) for what was deliberately
left out.

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
- [docs/competitors.md](docs/competitors.md) — E2B and Daytona SDK comparison and the compatibility decisions that follow
- [docs/future.md](docs/future.md) — deliberate non-goals and what comes next

## Quick start

Requires Go 1.27 or newer and a reachable Docker daemon; no other dependency.

```sh
make build
./bin/sandboxd &                     # listens on 127.0.0.1:8080
./bin/sandboxctl doctor              # config, health, auth, docker, sandbox
./bin/sandboxctl run -- sh -c 'echo hello'
./bin/sandboxctl run -- code 'print(2 ** 10)'
```

```text
sandbox: sbx_1f2e... (created; remove with: sandboxctl kill sbx_1f2e...)
execution: exe_9a4c...
hello
execution exe_9a4c...: completed (exited), exit 0
```

`run` streams the command's output on stdout and keeps its own status on
stderr, so `sandboxctl run -- cmd > out.txt` captures only the output. The exit
code mirrors the command's (130 on Ctrl+C; everything that is not the command
itself collapses to 1). `sandboxctl attach`, `logs`, `inspect`, `list`,
`pause`, `resume`, and `kill` cover the rest; `sandboxctl help` lists the flags.

Cancelling an execution sends SIGTERM to its process group, then SIGKILL two
seconds later, and answers only once the processes are gone. A streaming
connection that drops reconnects with `Last-Event-ID` and never duplicates or
loses output.

## Examples

- [`examples/agent-tool/`](examples/agent-tool/README.md) — an example adapter
  that lets an agent application expose the Go SDK as a `run_python` tool.
  The model produces JSON arguments; your application calls the adapter and
  sends its structured result back to the model. This is not a complete agent
  or an MCP server. See the [usage and integration guide](examples/agent-tool/README.md)
  for the model/application/SDK boundary, real Docker execution, and error handling.
  Preview the tool definition and a simulated result without a server:

  ```sh
  go run ./examples/agent-tool/cmd/demo -definition
  env -u SANDBOX_DOMAIN go run ./examples/agent-tool/cmd/demo -code 'print(2 ** 10)'
  ```

  Without `SANDBOX_DOMAIN`, the demo uses a stub that only recognizes the exact
  code above; it does not execute Python. Set `SANDBOX_DOMAIN` to a running
  `sandboxd` to execute arbitrary Python in Docker.

## SDK

```go
import sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"

client, _ := sandbox.NewClient(sandbox.Config{})   // reads SANDBOX_DOMAIN / SANDBOX_API_KEY
sb, _ := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{})
ex, err := client.Exec(ctx, sb.ID, []string{"python3", "-c", "print(1)"})
switch {
case errors.Is(err, sandbox.ErrOOM):
    fmt.Println("killed by the kernel:", ex.ExitCode)
}
```

`Exec` returns the completed execution with full output alongside the error:
nil only when the command exited 0 with reason `exited`. `ErrOOM`,
`ErrTimeout`, `ErrCanceled`, `ErrEvicted`, and `ErrNonZeroExit` each match with
`errors.Is`, and `StreamExecution` gives live events through a callback.

## Development

```sh
make test     # unit tests
make race     # race detector
make vet      # go vet
make lint     # golangci-lint
```

```sh
make lint                                   # golangci-lint, pinned in CI
E2E_COVERAGE=1 bash scripts/e2e.sh artifacts/e2e
bash scripts/coverage.sh <unit-dir> artifacts/e2e/coverage 70
goreleaser release --snapshot --clean       # six archives plus checksums
```

CI runs four jobs: `lint`, `test` (race plus coverage), `build` (every command,
`goreleaser check`, a snapshot, checksum verification), and `e2e` (real Docker,
merged coverage at or above 70%, and an assertion that no containers remain).
Releases are built by GoReleaser from a tag; nothing is uploaded.

The Docker-backed tests are opt-in:

```sh
go test -tags docker ./internal/dockerbackend/ -run TestReal
```

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
