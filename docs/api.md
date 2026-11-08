# HTTP API

Status: normative. `api/openapi.yaml` must describe exactly this contract; when they disagree, fix one of them in the same change.

## Conventions

- Base URL: `SANDBOX_DOMAIN`, default `http://localhost:8080`. All resource paths start with `/v1`.
- Bodies are UTF-8 JSON unless stated otherwise. Unknown request fields are ignored, so a newer client can talk to an older server; see [versioning.md](versioning.md). Malformed JSON and invalid values are still `400 invalid_request`.
- Timestamps are RFC 3339 in UTC. Durations are integer seconds and their field names end in `_seconds`.
- IDs are opaque strings. Clients must not parse them.
- Authentication: if the server was started with `SANDBOX_API_KEY` set, every `/v1` request must carry `Authorization: Bearer <key>`, otherwise `401 unauthorized`. `/healthz` never requires authentication. The key is compared in constant time.

## Errors

Every non-2xx response has this body:

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
| 413 | `payload_too_large` | request body or file exceeds a limit | no |
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
| `expires_at` | string or null | current lifetime deadline; null when disabled or killed |
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

Termination reasons, in precedence order (the first that applies wins):

| Reason | Condition |
|---|---|
| `canceled` | the server terminated it because of a cancel request or sandbox deletion |
| `timeout` | the server terminated it because the sandbox lifetime expired with action `kill` |
| `oom` | the kernel OOM killer acted in the sandbox's cgroup during the execution |
| `evicted` | the container stopped or disappeared without the server asking for it |
| `exited` | the process exited on its own, with any exit code, including 137 |

`canceled` and `timeout` are intents the server records before it acts, so they win over whatever exit status follows. Exit code 137 alone never implies `oom`.

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
- Idle time is measured from the latest of: the last authenticated request addressing the sandbox, the end of the last execution, the close of the last event stream.
- When idle time reaches `idle_timeout_seconds`, the configured action runs; `kill` sets `kill_reason: idle_timeout`.

### Eviction

If the container stops or disappears without the server asking, the sandbox becomes `killed` with `kill_reason: evicted` and a running execution ends with reason `evicted`. The server must detect this within 5 seconds, or earlier on the next request addressing the sandbox.

Timeout transitions are applied by a periodic reaper (at most 1 second between checks). Read requests never change state.

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

`200` Sandbox in state `killed`. Removes the container. Idempotent.

### `POST /v1/sandboxes/{sandbox_id}/executions`

```json
{"command": ["python3", "-c", "print(1)"], "idempotency_key": "6f1c..."}
```

- `command` is required and non-empty.
- `idempotency_key` is optional, at most 128 characters, scoped to the sandbox.
- `201` with the new Execution (`state: running`).
- Same key and same command as an existing execution: `200` with that execution, in whatever state it is now. No new process starts.
- Same key with a different command: `409 idempotency_conflict`.
- Another execution is running: `409 busy`. One running execution per sandbox keeps container-wide OOM evidence attributable to a single execution.

### `GET /v1/sandboxes/{sandbox_id}/executions`

`200 {"executions": [Execution without stdout/stderr, ...]}`, newest first.

### `GET /v1/sandboxes/{sandbox_id}/executions/{execution_id}`

`200` Execution including retained `stdout` and `stderr`.

### `POST /v1/sandboxes/{sandbox_id}/executions/{execution_id}/cancel`

No body. Terminates the execution's whole process group: SIGTERM, then SIGKILL after 2 seconds. Responds only once the execution is `completed`, with `200` and the Execution (`termination_reason: canceled`). If the processes cannot be confirmed gone within 10 seconds, the server removes the container (the sandbox becomes `killed`, `kill_reason: requested`) so nothing keeps running. Canceling a completed execution returns it unchanged.

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
- `Last-Event-ID: <n>` replays only events with ID greater than `n`, then continues live. A non-integer value is `400 invalid_request`.
- The server sends a `: ping` comment every 15 seconds while the stream is idle.
- The server closes the stream after `complete`. A finished execution replays its events and closes immediately.
- Closing a stream never affects the execution. An open stream counts as activity for the idle timeout.

## Limits

| Limit | Value | Behavior when exceeded |
|---|---|---|
| Retained output per execution (stdout + stderr) | 1 MiB | further output is discarded, one `truncated` event is sent, `output_truncated: true`; the process keeps running |
| Output events per execution | 4096 | same as above |
| JSON request body | 1 MiB | `413 payload_too_large` |
| File upload / download | 10 MiB | `413 payload_too_large` |

The stored output is exactly the data delivered in events, so a replay from ID 0 reproduces `stdout` and `stderr`.

## Server behavior

- Listens on `127.0.0.1:8080` by default (`-addr` flag). Refuses to start on a non-loopback address unless `SANDBOX_API_KEY` is set.
- Each container is labeled `tiny-sandbox.owner=<listen address>`. On startup the server removes containers carrying its own label, so a restart does not leak sandboxes. State is in memory and is lost on restart.
