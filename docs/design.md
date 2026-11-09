# Design

tiny-sandbox is a small reference implementation of a code-execution sandbox for AI agents: a Go SDK, a CLI, and a Docker-backed server. It is not a production isolation service.

This document is the design source for the implementation. `api.md`, `sdk.md` and `cli.md` are normative contracts; where this document disagrees with them, they win.

## Motivation

Coding agents increasingly run the code they write. Running that code is easy; running it predictably is not. Most of the hard questions sit on the client side of the API:

- When a sandbox "times out", is it destroyed or paused, and what does a pause actually keep?
- If a streaming connection drops, does the command keep running? Can the client resume without losing or duplicating output?
- When the caller cancels, does the process really stop, including its children?
- A command exited with 137. Was it an OOM kill, a timeout, a cancellation, or an external eviction?
- Which requests are safe to retry, and how does a retry avoid running a command twice?

The module is `github.com/ruhuang2001/tiny-sandbox`; `sandbox-go` is the package `sandbox` inside it, so SDK imports read `github.com/ruhuang2001/tiny-sandbox/sandbox-go`.

This project answers those questions with working code and tests rather than prose. The API semantics follow the public documentation of PPIO Sandbox and E2B where they agree; it does not claim wire compatibility with either.

## Goals

1. **Lifecycle**: create, pause, resume, and kill sandboxes; a lifetime timeout and an idle timeout, each able to kill or pause.
2. **Execution and streaming**: executions as resources with IDs, output over SSE, lossless reconnect with `Last-Event-ID`, and command-level cancellation.
3. **Errors and retries**: five typed termination reasons and a retry policy that never replays a command.
4. **Developer experience**: a CLI that gets a new user from zero to a first command and helps debug a failing one.

## Non-goals

Templates, snapshots, volumes, secrets, multi-tenancy, billing, browser automation, non-Go SDKs, durable server state, and hardened isolation (microVMs, gVisor). Each has an extension point and a design sketch in [future.md](future.md).

## Precedents

Two earlier builds of this same demo exist. This design takes the parts of each that survived their own verification, and drops the parts that did not.

| Concern | Earlier build A | Earlier build B | This design |
|---|---|---|---|
| Server backend | Hand-written Docker Engine API over a unix socket | `docker` CLI via string-built argv | Engine API (A). No shell quoting, injectable faults, unit-testable parsers |
| Server testability | `Backend` interface with an in-memory implementation | None; Docker required for every path | Interface plus in-memory backend (A) |
| Evidence | Script exit codes only | `artifacts/<stage>/` with SSE transcripts, cgroup counters, JSON results | Artifacts (B), one directory per stage, referenced from `progress.md` |
| Coverage | `go test` coverage, 60% gate | E2E-derived coverage via `-coverpkg`, 74.7% | Both, merged into one number with a 70% gate |
| Contract | `api/openapi.yaml` alone | `go:embed` so the spec ships in the binary | `go:embed` plus a test that cross-checks routes against the spec |
| Output bounds | 2 MiB / 2000 events | Unbounded | 1 MiB / 4096 events, a `truncated` event, and `output_truncated` in the contract |
| Exec package layout | One package per concern | 472-line `main.go` CLI, 442-line `execution.go` | Split by command and by concern |

## Architecture

```text
sandboxctl / Go application
          |
      sandbox-go          HTTP JSON + SSE
          |
       sandboxd
          |
   daemon.Backend  ---- in-memory backend (unit and contract tests)
          |
   Docker Engine API over a unix socket
          |
   one container per sandbox
```

Dependency direction is strict: `daemon` knows only the `Backend` interface and the state machine; `dockerbackend` knows only `dockerapi`; `dockerapi` knows only HTTP over the socket. Nothing above `daemon` knows Docker exists.

```
cmd/sandboxd/            main, flags, wiring
cmd/sandboxctl/          one file per command group: run, doctor, ops, main
internal/daemon/         state machine, execution engine, classification, HTTP handlers
internal/dockerapi/      minimal Engine API client: create, start, exec, inspect, pause, kill
internal/dockerbackend/  Backend implementation over dockerapi; cgroup reads
api/                     openapi.yaml plus contract.go with go:embed
sandbox-go/              SDK: client, sandbox, exec, stream, retry, errors, files
examples/agent-tool/     the SDK used as an agent tool function (built and tested in CI)
```

## Key decisions

### Sandbox lifecycle

- States: `pending -> running <-> paused`, terminal `killed`.
- `timeout_seconds` (default 300) counts from creation. `idle_timeout_seconds` (default 0, disabled) counts while there is no running execution and no open event stream.
- Each timeout selects `kill` or `pause`. When the lifetime timeout pauses, a resume starts a fresh interval of the same length; otherwise an auto-resume would immediately expire again.
- Starting an execution in a paused sandbox resumes it first.
- A single reaper performs all timeout transitions; reads never change state. The store takes an injectable clock, so timeout tests are deterministic.
- Pause uses Docker pause, which freezes the container's cgroup. Process memory is retained and a paused process continues from the same point after resume. A filesystem commit or snapshot is a different thing: it keeps files, not running processes.

### Executions and streaming

- An execution is a resource under a sandbox: `/v1/sandboxes/{id}/executions/{execution_id}`. Its ID is what makes replay and cancellation possible.
- A sandbox runs one execution at a time; an overlapping start returns `409 busy`. This keeps container-wide OOM evidence attributable to a single execution.
- Output is recorded as an event log with monotonically increasing IDs: `stdout`, `stderr`, and one terminal `complete` event. The log is bounded; when the bound is hit a `truncated` event tells the client the stream is incomplete.
- SSE was chosen over WebSocket: output flows one way, SSE works through ordinary HTTP proxies, is readable with `curl`, and has event IDs built in. The cost is no interactive stdin, and control operations need separate requests.
- `Last-Event-ID` replays only later events; the SDK also de-duplicates by ID. Closing a stream never cancels the execution.
- The blocking `Exec` call is a projection of the same stream, so there is one source of truth for output.

### Cancellation

- Cancellation is an explicit request: `POST .../executions/{execution_id}/cancel`. The server terminates the command's process group, including from the paused state, and confirms only after the processes are gone.
- In the SDK, canceling the caller's context triggers that request with a fresh, short-lived context, because the original context can no longer carry a request.
- If the caller cancels while an execution start is in flight, the SDK cannot know whether the server accepted it. It repeats the start with the same idempotency key to learn the execution ID, then cancels it. If that recovery fails, it returns a `cancel_unconfirmed` error carrying the key, so nothing runs unnoticed.
- The CLI never calls HTTP directly except for `doctor`'s health probe. Earlier builds had a CLI path that bypassed the SDK's start-recovery logic and leaked a running command; the SDK is the only client of the HTTP layer.

### Termination reasons

| Reason | Meaning |
|---|---|
| `exited` | the process exited on its own, with any exit code |
| `timeout` | the sandbox lifetime expired |
| `canceled` | an explicit cancel request |
| `oom` | the kernel OOM killer stopped the command |
| `evicted` | the container disappeared or was stopped externally |

Classification order is fixed: recorded intent (`canceled`, `timeout`) first, then a positive delta in the cgroup `oom_kill` counter, then container liveness, then the exit code. Exit 137 alone never means OOM: an explicit `kill -9`, an eviction, and a runtime stop all produce 137.

An earlier build misclassified an externally stopped container as `exited` because `docker inspect` intermittently reported a lowercase `no such object` before the container record settled. The current design checks the exact container list instead of `inspect`, and for an ambiguous 137 it samples liveness until the state settles or a two-second budget runs out, deciding on the last observation: the state can be stale in either direction — briefly missing, or briefly still `running` right after an external kill — so neither a single "not running" nor a single "running" answer decides. The wait ends early only on two consecutive `stopped` answers, which is a state that cannot be stale; `missing` and `running` never end it early, because each is the race the window exists for. The budget is a time, not a sample count, because the lag this covers is a property of the runtime, not of the daemon: a local daemon settles the record 75–134 ms after `docker kill` returns, while a loaded CI runner exceeded the earlier five-sample window. Both races are covered by unit tests, one of them found by the end-to-end suite on a slow runner.

### Retries

- Retry candidates: transport errors before a complete response, truncated response bodies, and status 429, 500, 502, 503, 504.
- Safe to repeat: `GET`, `DELETE`, `PUT` (files), pause, resume, cancel, and execution starts carrying an idempotency key. `POST /v1/sandboxes` is attempted exactly once.
- Backoff: `min(2s, 200ms * 2^attempt)` with full jitter; a `Retry-After` header overrides the computed delay. The SSE reconnect loop uses the same policy and resets its attempt counter whenever a new event arrives.
- Nothing is retried after the context is done.

### SDK and CLI

- The SDK reads `SANDBOX_DOMAIN` and `SANDBOX_API_KEY`; the HTTP transport is injectable.
- Errors are a single `*Error` type carrying status, code, the full `Execution` where one exists, and an idempotency key where recovery failed. `errors.Is` works against sentinel values for every termination reason.
- Time fields carry their unit in the name (`timeout_seconds`), because SDKs in different languages disagree on units.
- The CLI writes command output to stdout and its own status to stderr, mirrors the command's exit code, returns 130 on Ctrl+C, and attaches a suggested fix to common errors.
- `sandboxctl doctor` checks configuration, server health, authentication, and Docker, then creates a sandbox, runs a command, and removes it.

### Server state

State lives in memory. A restart forgets sandboxes while Docker keeps their containers, so the server removes containers carrying its own label on startup and on shutdown, and the end-to-end suite asserts the container list is empty afterwards. Persistence and a boot-time reconciler are the interesting parts of a real service and are deliberately out of scope.

## Verification

Three layers, each answering a different question:

| Layer | Where | Question |
|---|---|---|
| Unit | `internal/daemon` with the in-memory backend; `dockerapi` parsers; SDK SSE decoding | Is the semantics right: transition table, classification precedence, reconnect de-duplication, backoff bounds |
| Contract | `httptest` with injected faults (reset connections, 429, 5xx, half-written bodies) and a fake Docker daemon on a unix socket | Is the behaviour right when transport breaks: retry boundaries, unconfirmed cancel, truncated stream |
| End-to-end | `scripts/e2e.sh` against real Docker | Is it right in reality: pause retains memory, OOM is distinguished from `kill -9`, cancellation kills the process tree |

Rules:

- Every layer must exist. A build with a passing end-to-end suite but no unit tests cannot regress the state machine; a build with unit tests but no artifacts cannot be audited.
- Coverage is measured twice: `go test -coverprofile` for the unit and contract layers, and `-coverpkg` counted during the end-to-end run for the Docker backend and CLI. CI merges both profiles and gates on the merged number (70%).
- The end-to-end suite writes evidence under `artifacts/<stage>/`: the commands run, raw SSE transcripts, cgroup counters, JSON responses, server logs, and the final container list. `progress.md` records conclusions and links, not pasted logs.
- `api/openapi.yaml` and the router must not drift: a test parses the spec's path set and compares it both ways with the routes the server registers.
- Anything not actually run is labelled `not run` in the documentation. Local `act` runs are not reported as remote CI.

## CI/CD

One workflow, four jobs:

| Job | Contents |
|---|---|
| `lint` | `golangci-lint` at a pinned version, `go vet` |
| `test` | unit and contract tests under `-race`, merged coverage gate |
| `build` | `go build ./...`, both commands, the example, `goreleaser check` |
| `e2e` | Docker-backed suite with coverage instrumentation; uploads `artifacts/`; asserts no containers remain |

Releases are built by GoReleaser from a tag: six platform archives plus a checksum file. Versioning follows semantic versioning at the repository level; CLI flags, environment variables, JSON fields, error types, timeout units, and SSE event semantics are all part of the compatibility surface. [versioning.md](versioning.md) holds the details, and [competitors.md](competitors.md)
records which parts of the E2B and Daytona client surfaces this design keeps
compatible and which it deliberately changes.

## Known pitfalls

Recorded because earlier builds hit them, not as hypotheticals:

1. **137 is not OOM.** Order: intent, cgroup `oom_kill` delta, container liveness, exit code. Handle the container-record race explicitly.
2. **The cancellation gap.** Between "start accepted" and "execution ID received" the client does not know whether work exists. Recover with the same idempotency key; never report success silently.
3. **The CLI bypass.** Any CLI path that talks HTTP directly skips SDK recovery logic. Restrict direct HTTP to the health probe.
4. **Unbounded server memory.** Output retention, event count, and upload size all need contract-level limits; a single `yes` otherwise exhausts the daemon.
5. **Pause must be measured, not asserted.** Prove it with a counter that advances across pause and resume, not with the state field.
6. **Documentation bloat.** Evidence goes in `artifacts/`; prose stays readable.

## Roadmap

Every item below is implemented and verified; `progress.md` links the evidence
for each stage.

- [x] Contract freeze: `api/openapi.yaml` matches `api.md`; the route-versus-spec test fails first, then passes
- [x] State machine, timeouts, and classification on the in-memory backend
- [x] Execution engine, bounded event log, SSE with replay, cancellation
- [x] Docker backend over the Engine API, cgroup reads, external-stop reconciliation
- [x] Go SDK: client, sandbox, exec, stream, retry, errors, files
- [x] CLI: doctor, run, attach, logs, inspect, list, pause, resume, kill, version
- [x] CI, GoReleaser, README, versioning notes
- [ ] Interview notes mapped to evidence — the notes themselves live outside this repository
