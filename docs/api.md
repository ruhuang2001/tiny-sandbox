# HTTP API

Status: normative. [OpenAPI specification](../api/openapi.yaml) must describe exactly this contract; when they disagree, fix one of them in the same change.

## Conventions

- Base URL: `SANDBOX_DOMAIN`, default `http://localhost:8080`. All resource paths start with `/v1`.
- Bodies are UTF-8 JSON unless stated otherwise. Unknown request fields are ignored, so a newer client can talk to an older server; see [versioning.md](versioning.md). Malformed JSON and invalid values are still `400 invalid_request`.
- Timestamps are RFC 3339 in UTC. Durations are integer seconds and their field names end in `_seconds`.
- IDs are opaque strings. Clients must not parse them.
- Authentication: if the server was started with `SANDBOX_API_KEY` set, every `/v1` request must carry `Authorization: Bearer <key>`, otherwise `401 unauthorized`. `/healthz` never requires authentication. The key is compared in constant time.

## Errors

Errors emitted by the API handlers use this body. Router-level errors for
unknown paths or unsupported methods may use plain text:

```json
{"code": "busy", "message": "sandbox sbx_123 is already running execution exe_456"}
```

| Status | `code` | When | Retryable |
|---|---|---|---|
| 400 | `invalid_request` | malformed JSON, invalid value, or an invalid `Last-Event-ID` | no |
| 401 | `unauthorized` | missing or wrong API key | no |
| 404 | `not_found` | sandbox, execution, or file does not exist | no |
| 409 | `invalid_state` | operation not allowed in the sandbox's current state (see transition table) | no |
| 409 | `busy` | the sandbox already has a running execution | no |
| 409 | `idempotency_conflict` | idempotency key reused with a different command | no |
| 413 | `payload_too_large` | JSON request body or file upload exceeds a limit | no |
| 429 | `rate_limited` | reserved; the reference server does not emit it, clients must handle it | yes |
| 500 | `internal` | unexpected server error | yes |
| 502 | `backend_error` | the container runtime failed | yes |
| 503 | `unavailable` | server shutting down or not ready | yes |

Clients may honor a `Retry-After` header (seconds) on 429 and 503.

## Resources

### Sandbox

| Field | Type | Notes |
|---|---|---|
| `id` | string | |
| `state` | string | `pending`, `running`, `paused`, `killed` |
| `image` | string | default `alpine:3.20` |
| `memory_mb` | int | memory limit, default 512, minimum 16 |
| `timeout_seconds` | int | lifetime timeout, default 300, `0` disables |
| `timeout_action` | string | `kill` (default) or `pause` |
| `idle_timeout_seconds` | int | default `0` (disabled) |
| `idle_timeout_action` | string | `kill` (default) or `pause` |
| `created_at` | string | |
| `expires_at` | string or null | current lifetime deadline; null when disabled, pending, killed, or paused with timeout action `pause` |
| `kill_reason` | string or null | set when `killed`: `requested`, `timeout`, `idle_timeout`, `evicted` |

### Execution

| Field | Type | Notes |
|---|---|---|
| `id` | string | |
| `sandbox_id` | string | |
| `command` | string[] | argv, executed without a shell |
| `state` | string | `running` or `completed` |
| `exit_code` | int or null | null while running, or if the runtime could not report one |
| `termination_reason` | string or null | null while running; see below |
| `stdout`, `stderr` | string | retained output; omitted in list responses |
| `output_truncated` | bool | true when output exceeded the retention limit |
| `started_at` | string | |
| `finished_at` | string or null | |

Termination reasons used by the execution classifier. The first recorded
termination intent wins; otherwise the classifier checks OOM evidence,
container liveness, and finally the exit code:

| Reason | Condition |
|---|---|
| `canceled` | the server terminated it because of a cancel request or sandbox deletion |
| `timeout` | the server terminated it because the sandbox lifetime expired with action `kill` |
| `oom` | the kernel OOM killer acted in the sandbox's cgroup during the execution |
| `evicted` | the container stopped or disappeared without the server asking for it |
| `exited` | the process exited on its own, with any exit code, including 137 |

`canceled` and `timeout` are intents the server records before it acts; a later intent does not replace the first. The periodic eviction sweep can also finalize an execution directly as `evicted` when the container is gone. Exit code 137 alone never implies `oom`.

## Sandbox lifecycle

```text
pending ──► running ◄──► paused
   │           │            │
   └───────────┴────────────┴──► killed (terminal)
```

Creation is synchronous: `POST /v1/sandboxes` returns once the container is running, so `pending` is only observable by concurrent readers.

Operation results by current state:

| Operation | `pending` | `running` | `paused` | `killed` |
|---|---|---|---|---|
| pause | 409 `invalid_state` | becomes `paused` | 200, no change | 409 `invalid_state` |
| resume | 409 `invalid_state` | 200, no change | becomes `running` | 409 `invalid_state` |
| delete | becomes `killed` | becomes `killed` | becomes `killed` | 200, no change |
| start execution | 409 `invalid_state` | allowed | resumes, then allowed | 409 `invalid_state` |
| read/write file | 409 `invalid_state` | allowed | resumes, then allowed | 409 `invalid_state` |
| cancel execution | n/a | allowed | resumes, then allowed | returns the stored record |
| get, list | allowed | allowed | allowed | allowed |

Pausing freezes every process in the sandbox, including a running execution, which stays `running` and continues when resumed. Deleting a sandbox with a running execution terminates it with reason `canceled`.

### Lifetime timeout

- With `timeout_action: kill`, the deadline is `created_at + timeout_seconds` and keeps counting while paused. At the deadline the sandbox is killed with `kill_reason: timeout`, and a running execution ends with reason `timeout`.
- With `timeout_action: pause`, the deadline only counts while `running`. At the deadline the sandbox is paused. Every transition to `running` (explicit or automatic resume) sets a fresh deadline `now + timeout_seconds`. Without this, an automatic resume would immediately expire again.
- `expires_at` always shows the current deadline.

### Idle timeout

- A sandbox is idle while it is `running`, has no running execution, and has no open event stream.
- The intended activity model includes authenticated requests addressing the sandbox, execution completion, and stream closure. Currently activity is refreshed by sandbox creation, `GET /v1/sandboxes/{sandbox_id}`, successful execution start, execution completion, and event-stream open/close. Other requests do not consistently refresh it; polling execution or file endpoints does not keep an otherwise idle sandbox alive.
- When idle time reaches `idle_timeout_seconds`, the configured action runs; `kill` sets `kill_reason: idle_timeout`.

### Eviction

If the container stops or disappears without the server asking, the sandbox becomes `killed` with `kill_reason: evicted` and a running execution ends with reason `evicted`. The periodic sweep checks live containers on a nominal one-second interval. Requests that encounter a missing container can detect eviction sooner, but ordinary get/list calls do not probe Docker. Five seconds is the intended detection budget, not a hard bound when backend calls block or sandbox locks remain busy.

Timeout transitions are applied by a periodic reaper configured to tick at most one second apart; backend calls and lock contention can delay observation. Ordinary reads do not apply timeouts. File reads can resume a paused sandbox, and requests may detect external eviction.

## Endpoints

### `GET /healthz`

`200 {"status": "ok", "version": "<build version>"}`. No authentication.

### `POST /v1/sandboxes`

Request (all fields optional):

```json
{"image": "python:3.12-alpine", "memory_mb": 256, "timeout_seconds": 600, "timeout_action": "pause", "idle_timeout_seconds": 60, "idle_timeout_action": "pause"}
```

`201` with the Sandbox. Not idempotent; clients must not retry it automatically.

### `GET /v1/sandboxes`

`200 {"sandboxes": [Sandbox, ...]}`, newest first. Killed sandboxes are included until the server restarts.

### `GET /v1/sandboxes/{sandbox_id}`

`200` Sandbox.

### `POST /v1/sandboxes/{sandbox_id}/pause`, `POST /v1/sandboxes/{sandbox_id}/resume`

No body. `200` Sandbox. Idempotent per the transition table.

### `DELETE /v1/sandboxes/{sandbox_id}`

`200` Sandbox in state `killed`. Requests container removal on a best-effort basis; removal errors are not returned. Repeating deletion returns the stored killed record without retrying removal.

### `POST /v1/sandboxes/{sandbox_id}/executions`

```json
{"command": ["python3", "-c", "print(1)"], "idempotency_key": "6f1c..."}
```

- `command` is required and non-empty.
- `idempotency_key` is optional, at most 128 characters, scoped to the sandbox.
- `201` with the new Execution (`state: running`).
- State validation precedes idempotency lookup: a killed or pending sandbox rejects the start, including a replay; a paused sandbox resumes first.
- Same key and same command as an existing execution: `200` with that execution, in whatever state it is now. No new process starts.
- Same key with a different command: `409 idempotency_conflict`.
- Another execution is running: `409 busy`. One running execution per sandbox keeps container-wide OOM evidence attributable to a single execution.

### `GET /v1/sandboxes/{sandbox_id}/executions`

`200 {"executions": [Execution without stdout/stderr, ...]}`, newest first.

### `GET /v1/sandboxes/{sandbox_id}/executions/{execution_id}`

`200` Execution including retained `stdout` and `stderr`.

### `POST /v1/sandboxes/{sandbox_id}/executions/{execution_id}/cancel`

No body. Records canceled intent, sends SIGTERM to the process group, and waits up to 2 seconds for completion before SIGKILL. It then waits up to 8 more seconds before requesting container removal and marking the sandbox `killed` with `kill_reason: requested`. These are wait budgets: backend calls add time, and the caller disconnecting its cancel request does not shorten them — the server finishes the escalation on its own so a client timeout cannot turn a slow signal into container removal. After removal, it waits up to 2 more seconds and returns `200` with the latest Execution snapshot, which can still be nonterminal. Canceling an already completed execution returns it unchanged. See [cancellation and removal](#cancellation-and-removal).

### `GET /v1/sandboxes/{sandbox_id}/executions/{execution_id}/events`

Server-sent events; see below.

### `GET /v1/sandboxes/{sandbox_id}/files?path=/abs/path`

`200` with the raw bytes (`application/octet-stream`). `404 not_found` if missing. `path` must be absolute.

### `PUT /v1/sandboxes/{sandbox_id}/files?path=/abs/path`

Raw bytes in the body; creates parent directories; replaces existing files. `204`. Idempotent.

## Event stream

`GET .../events` with `Accept: text/event-stream`.

```text
id: 1
event: stdout
data: {"data":"hello\n"}

id: 2
event: stderr
data: {"data":"warning\n"}

: ping

id: 3
event: complete
data: {"id":"exe_456","state":"completed","exit_code":0,"termination_reason":"exited", ...}

```

- Event IDs are decimal integers starting at 1 and increasing by 1 per event within an execution.
- Types: `stdout` and `stderr` carry `{"data": "<chunk>"}`, at most 32 KiB per chunk; `truncated` carries `{"limit_bytes": <n>}` and is sent at most once; `complete` carries the full Execution and is always the last event.
- Output is text. Chunks are cut on UTF-8 boundaries, so a valid multi-byte character is never split across two events; a client that concatenates the chunks in order reproduces the retained output exactly. Bytes that are not valid UTF-8 cannot be represented in a JSON string: each output stream replaces them with U+FFFD independently, both in the events and in the retained `stdout`/`stderr` field, so a program that writes binary data should encode it (for example with base64) rather than rely on the stream.
- `Last-Event-ID: <n>` replays only events with ID greater than `n`, then continues live. A non-integer value, or a value beyond the newest event of the execution, is `400 invalid_request`: no such event will ever exist, and a running execution would otherwise leave the caller waiting for a replay that cannot arrive.
- The server sends a `: ping` comment every 15 seconds while the stream is open, including while output is flowing.
- The server closes the stream after `complete`. A finished execution replays its events and closes immediately.
- Closing a stream never affects the execution. An open stream counts as activity for the idle timeout.

## Limits

| Limit | Value | Behavior when exceeded |
|---|---|---|
| Retained output per execution (stdout + stderr) | 1 MiB | further output is discarded, one `truncated` event is sent, `output_truncated: true`; the process keeps running |
| Output events per execution | 4096 | same as above |
| Executions retained per sandbox | 200 | the oldest completed execution is dropped, including its retained output and its idempotency key; the running execution is never dropped. `404 not_found` afterwards |
| JSON request body | 1 MiB | `413 payload_too_large` |
| File upload | 10 MiB | `413 payload_too_large` |
| File download through Docker | 10 MiB | currently `502 backend_error` from archive extraction |

The stored output is exactly the data delivered in events, so a replay from ID 0 reproduces `stdout` and `stderr`.

Because retention is bounded, an idempotency key is remembered only while its
execution is retained. A start that reuses a key older than the 200 most recent
executions is treated as a new start and runs the command again; clients that
need a longer window must track execution IDs themselves.

## Implementation limits

### Cancellation and removal

In the normal cancellation path, the handler waits for completion. In the current
container-removal fallback, the handler waits for a bounded period and returns
the latest execution snapshot; backend removal failures or delayed completion
can leave that snapshot nonterminal. Clients must inspect `state` and must not
treat an HTTP response alone as proof that every process has stopped. The SDK
has its own five-second cancellation deadline, so it can return
`ErrCancelUnconfirmed` while server cleanup is still in progress.

Container removal is best effort in `terminateLocked`. A sandbox can be recorded
as `killed` even if Docker removal fails; the error is not returned by the delete
endpoint. A repeated delete of that killed record does not retry removal. Startup
cleanup with the same owner label is the recovery path.

## Server behavior

- Listens on `127.0.0.1:8080` by default (`-addr` flag). Refuses to start on a non-loopback address unless `SANDBOX_API_KEY` is set.
- Each container is labeled `tiny-sandbox.owner=<listen address>`. On startup the server attempts to remove containers carrying its own label and logs cleanup failures. State is in memory and is lost on restart. Shutdown does not itself remove containers; startup cleanup matches the listen address, so changing it will not clean containers owned by the previous address.

## Implementation references

- [HTTP handlers](../internal/daemon/handler.go): routing, authentication, activity updates, and SSE framing.
- [Sandbox store](../internal/daemon/store.go): lifecycle, deadlines, and best-effort removal.
- [Execution engine](../internal/daemon/engine.go): idempotency, cancellation, and file access.
- [Reaper](../internal/daemon/reaper.go): timeout and eviction checks.
- [Server entry point](../cmd/sandboxd/main.go): startup cleanup and HTTP shutdown.
