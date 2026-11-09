# Python SDK

Status: normative. Contract for `sandbox-python`, the `tiny-sandbox` Python
distribution imported as `tiny_sandbox`. The SDK speaks the existing [HTTP
contract](api.md); it changes neither the Go SDK nor the server API.

## Installation and quick start

Python 3.10 or newer is required. The synchronous client has no runtime
dependencies outside the Python standard library. There is no async client.
Install from a repository checkout:

```sh
python3 -m pip install ./sandbox-python
```

Start `sandboxd` with access to Docker, then run:

```python
from tiny_sandbox import Client, CreateSandboxRequest, Error

with Client() as client:
    with client.sandbox(CreateSandboxRequest(image="alpine:3.20")) as sandbox:
        try:
            execution = client.exec_stream(
                sandbox.id, ["sh", "-c", "echo hello; sleep 1; echo done"],
                on_stdout=lambda chunk: print(chunk, end="", flush=True),
                on_stderr=lambda chunk: print(chunk, end="", flush=True),
            )
            assert execution.exit_code == 0
        except Error as error:
            print(error.code)
            if error.execution is not None:
                print(error.execution.stderr)
            raise
```

`client.sandbox(request)` creates a sandbox, yields its `Sandbox` response,
and kills it on exit, including exceptional exit, using a fresh five-second
cleanup budget. `with Client()` only closes client resources; it does not
remove sandboxes created through `create_sandbox`. Use `kill_sandbox` to
manage their lifetime explicitly.

## Configuration

```python
client = Client(domain="", api_key="", max_retries=3)
```

- `domain`: nonempty values override `SANDBOX_DOMAIN`; otherwise use that
  environment variable, then `http://localhost:8080`. A value without a scheme
  gets `http://`.
- `api_key`: nonempty values override `SANDBOX_API_KEY`; empty values use it.
- `max_retries`: additional attempts after the first request, and the total
  reconnect budget for one stream. `None` uses the default of 3; explicit `0`
  disables retries. This differs from Go, where zero selects the default.

There is no global request timeout that would cut off a long-lived stream.
Use an optional `CancellationToken` for a local deadline or explicit caller
cancellation. Authentication uses the HTTP contract's Bearer token.

## Requests and responses

Requests are typed dataclasses. `CreateSandboxRequest` accepts `image`,
`memory_mb`, `pids_limit`, `nano_cpus`, `network`, `timeout_seconds`,
`timeout_action`, `idle_timeout_seconds`, and `idle_timeout_action`.
An optional value of `None` is omitted from JSON. Explicit zero is sent to the
server, preserving the difference between omission and disabling a timeout.
The [HTTP resource and request tables](api.md#resources) define units, defaults,
and accepted values; the client does not invent different defaults.

```python
from tiny_sandbox import CreateSandboxRequest, ExecuteRequest

sandbox_request = CreateSandboxRequest(
    image="alpine:3.20", memory_mb=256, pids_limit=512,
    nano_cpus=1_000_000_000, network="none", timeout_seconds=0,
)
execute_request = ExecuteRequest(
    command=["sh", "-c", "sleep 30"],
    idempotency_key="my-operation-1",
    timeout_seconds=5,
)
```

`ExecuteRequest(command=..., idempotency_key="", timeout_seconds=None)` describes
a start. An empty key causes the SDK to generate 128 random bits encoded as
hex, reused for every retry or recovery of that start. Keys are scoped to a
sandbox and only remembered while the execution is retained. Replaying a
retained key with the same command returns that execution; changing the
timeout does not update its deadline. A different command produces
`idempotency_conflict`.

`Sandbox`, `Execution`, and `Event` are keyword-only response dataclasses.
Resource fields use Python snake_case names matching the JSON contract.
Unknown resource response fields are retained in `extra` and do not fail decoding.
Unknown state, reason, network, or action strings are preserved rather than
rejected by a closed enum. Missing additive resource fields from an older
server are `None`, not assumed server defaults. Do not use missing resource
limits as evidence that the server enforced them. The package includes
`py.typed` for type-aware callers.

## Methods

All methods are synchronous. Every request or stream method accepts optional
keyword-only `cancel=token`; omitting it means no caller deadline.

| Area | Methods |
|---|---|
| Sandboxes | `create_sandbox(request)`, `get_sandbox(sandbox_id)`, `list_sandboxes()`, `pause_sandbox(sandbox_id)`, `resume_sandbox(sandbox_id)`, `kill_sandbox(sandbox_id)` |
| Executions | `start_execution(sandbox_id, request)`, `get_execution(sandbox_id, execution_id)`, `list_executions(sandbox_id)`, `cancel_execution(sandbox_id, execution_id)` |
| Streaming | `stream_execution(sandbox_id, execution_id, *, on_event=None, on_stdout=None, on_stderr=None)` |
| Conveniences | `exec(sandbox_id, command)`, `exec_stream(sandbox_id, command, *, on_event=None, on_stdout=None, on_stderr=None)` |
| Files | `read_file(sandbox_id, path) -> bytes`, `write_file(sandbox_id, path, data)` |

Files preserve raw bytes; paths must be absolute. List methods return lists of
typed resources. Execution lists omit retained output at the HTTP layer; get
or stream an execution to retrieve its output.

`exec` and `exec_stream` start an execution and wait for completion. They return
normally only for `termination_reason == "exited"` and `exit_code == 0`.
`stream_execution` returns the final `Execution` for all server outcomes,
including nonzero exit, timeout, cancellation, OOM, or eviction. Transport,
callback, and caller-cancellation failures still raise `Error`.

### Server deadlines versus caller cancellation

The current API supports a per-execution `ExecuteRequest.timeout_seconds`.
Call `start_execution` with that request, then `stream_execution` when you need
that deadline. The server applies it and reports `termination_reason="timeout"`;
the sandbox normally remains available. An omitted or zero timeout leaves the
execution without its own deadline. The sandbox lifetime policy still applies.

```python
from tiny_sandbox import CancellationToken, ExecuteRequest

execution = client.start_execution(
    sandbox.id, ExecuteRequest(command=["sh", "-c", "sleep 30"], timeout_seconds=5)
)
execution = client.stream_execution(sandbox.id, execution.id)
assert execution.termination_reason == "timeout"

# A local deadline requests cancellation; it does not invent a server timeout.
token = CancellationToken(timeout=2)
client.exec(sandbox.id, ["sh", "-c", "sleep 30"], cancel=token)
```

`CancellationToken(timeout=None)` has no deadline. Call `token.cancel()` from
another thread to request cancellation. A token deadline is also caller
cancellation, so its error code is `canceled` or `cancel_unconfirmed`, never an
invented `timeout`. A token already canceled before a start or ordinary request sends no request.
Passing an already canceled token to stream_execution requests cancellation
of that existing execution without opening its stream.

If cancellation interrupts a start, the SDK repeats the start with its existing
key on an independent five-second budget, then cancels the recovered execution.
The same total budget covers start replay, cancellation, and confirmation; each
step does not receive a new five seconds. During streaming, caller cancellation
uses a fresh five-second total budget to cancel and confirm the execution.

Confirmation requires a terminal execution snapshot. If the cancel response
is still running, the SDK polls within the remaining budget. If confirmation
fails, it raises `cancel_unconfirmed` carrying the execution IDs when known and
the start's idempotency key when applicable. Server cleanup may still continue;
retain those identifiers to inspect or clean up later. A confirmed terminal
snapshot is attached to the `canceled` error; inspect it because natural
completion can race cancellation. Plain `cancel_execution` returns the server's
latest snapshot, which may still be running; it does not add this confirmation
loop.

## Streaming and callbacks

`on_event(event)` receives typed events; `on_stdout(chunk)` and
`on_stderr(chunk)` receive strings. Callbacks run synchronously in the reading
thread, so keep them short. `on_event` includes `stdout`, `stderr`, `truncated`,
and `complete`. If using both general and output callbacks, avoid rendering the
same chunk twice.

On disconnect, the SDK reconnects with `Last-Event-ID` set to the last fully
delivered event ID and ignores replayed IDs already delivered. Partial frames
do not advance that cursor. Reconnects share one `max_retries` budget for the
entire stream; receiving output does not replenish it. SSE reconnects use
backoff but do not apply `Retry-After`. A successful `complete` event supplies
the final execution.

An exception from any callback becomes `Error(code="callback")`, chained to
that exception, even if the callback raised another SDK `Error`. The stream
closes without retrying or canceling the remote execution. Explicitly call
`cancel_execution` if remote work should stop. Other stream failures also leave
remote work running; only caller cancellation invokes automatic cancellation.

Replay covers retained output, not unlimited output. A `truncated` event or
`execution.output_truncated` indicates data was discarded; reconnecting cannot
recover it. Output follows the server's UTF-8 text contract, including U+FFFD
replacement of invalid bytes. Encode binary stdout before sending it. SSE lines
and complete event frames are bounded to 16 MiB, covering the largest valid
server event. Go bounds individual lines to the same size.

## Errors

Operational SDK failures use `Error`. Its stable fields are `code`, `status_code`
(0 for client-side failures), `execution`, `idempotency_key`, and `cause`.
Python exception chaining preserves the underlying exception where available.
Prefer checking `error.code` for exact classification. `error.matches(code)`
also supports Go-style sentinel matching: status 404 matches `not_found`,
status 401 matches `unauthorized`, and a retained nonzero exit code matches
`nonzero_exit` even if the termination reason is different. There is no
exception subclass per API code. The package exports the string constants
`ERR_NONZERO_EXIT`, `ERR_TIMEOUT`, `ERR_CANCELED`, `ERR_OOM`, `ERR_EVICTED`,
`ERR_CANCEL_UNCONFIRMED`, `ERR_NOT_FOUND`, `ERR_UNAUTHORIZED`, `ERR_BUSY`,
and `ERR_INVALID_STATE` for use with `matches` or direct code comparison.

```python
from tiny_sandbox import Error

try:
    result = client.exec(sandbox.id, ["sh", "-c", "echo useful-output; exit 7"])
except Error as error:
    if error.code == "nonzero_exit" and error.execution is not None:
        print(error.execution.exit_code, error.execution.stdout)
    else:
        raise
```

Outcome codes include `nonzero_exit`, `timeout`, `canceled`, `oom`, and
`evicted`. `cancel_unconfirmed` means stopping remote work was not confirmed;
`callback` means application callback code failed. API failures retain codes
such as `not_found`, `unauthorized`, `busy`, and `invalid_state`, along with
HTTP status. Unknown future termination reasons remain observable as errors in
`exec` and `exec_stream`, never silent success. Exit code 137 alone does not
mean OOM. When `execution` exists, it carries retained output and truncation
metadata; start or transport failures may have none.

## Retries

The policy matches Go except for configuring zero retries:

- Retry transport failures before a complete response, truncated bodies, and
  statuses 429, 500, 502, 503, and 504.
- Repeat only safe operations: GET, DELETE, file PUT, pause, resume, cancel,
  and execution starts carrying an idempotency key. Sandbox creation is
  attempted once, including on a lost response.
- For retry index `n` starting at zero, the cap is
  `min(2 seconds, 200 milliseconds * 2**n)`; uniformly sample a delay from
  half that cap to the cap.
- A positive integer `Retry-After` in seconds overrides ordinary request
  backoff. Zero, invalid values, and HTTP-date values use calculated backoff.
- Caller cancellation interrupts requests and backoff. Recovery uses the
  independent cleanup budget described above.

## Compatibility and differences from Go

The wire API, idempotency rules, safe retry set, retry status set, jitter,
constant reconnect budget, retained-output replay, files, and outcome mapping
match the Go contract. Additive response fields and unknown string values are
accepted. A newer request field may be ignored by an older server; successful
HTTP status does not prove an optional capability was enforced. Consult the
server's matching API specification before depending on execution deadlines or
resource controls.

Three differences are deliberate:

1. Python's `max_retries=0` disables retries; Go's zero uses its default.
2. Python automatic cancellation requires a terminal snapshot before returning
   `canceled`. Go currently accepts a successful cancel response without that
   check and may return a nonterminal snapshot.
3. Python always wraps callback exceptions as `callback`, without retrying.
   Go currently passes through callback errors already typed as its SDK error,
   which can make a retryable callback error reconnect.

Python uses synchronous callbacks, dataclasses, one exception type, and context
managers instead of Go contexts, pointer options, and `errors.Is`.

### Contract clarifications discovered during the port

Issue #2 predates execution deadlines. The current HTTP/OpenAPI contract already
supports `timeout_seconds`; this SDK sends that server field and distinguishes
it from local caller cancellation. No server timeout feature is invented here.

The HTTP cancellation endpoint can return a running snapshot; a `200` alone
cannot prove termination. Automatic cancellation therefore needs the explicit
confirmation rule and one shared five-second recovery budget. Callback
exceptions need an unconditional wrapper to prevent an SDK-shaped exception
from accidentally entering the transport retry path. Forward compatibility also
requires preserving unknown strings and absent additive fields, beyond merely
ignoring extra JSON keys. These are SDK contract clarifications; existing Go
and HTTP behavior is preserved.

## Reproduce the entry gate

The end-to-end suite uses a real `sandboxd` and Docker, with an HTTP fault proxy
for dropped connections and replay. The central scenario runs a long command,
drops its stream, verifies reconnect with `Last-Event-ID` and output
deduplication, then cancels and requires a terminal snapshot. It also exercises
unconfirmed cancellation instead of treating an accepted request as success.

```sh
python3 -m pip install build
python3 -m build --outdir artifacts/python-dist sandbox-python
python3 -m pip install --force-reinstall artifacts/python-dist/*.whl
PYTHON=python3 SDK_INSTALLED=1 bash scripts/e2e-python.sh artifacts/python-e2e
```

The suite retains a JSON report, HTTP proxy trace, daemon logs, source identity,
and cleanup results under `artifacts/python-e2e`. Without `SDK_INSTALLED=1`,
the launcher creates a virtual environment and installs a wheel itself. See the
pre-implementation [failure model](../sandbox-python/e2e/FAILURES.md). CI runs
the installed package against real Docker on Python 3.10 and 3.15 and uploads
evidence even on failure. The wheel and source distribution are verification
artifacts, not publication to a package index.
