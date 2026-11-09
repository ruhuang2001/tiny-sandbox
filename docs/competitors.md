# Competitor SDK review

What this document is: a comparison of the client surfaces of E2B and Daytona
against `sandbox-go`, and the decisions that follow from it. The purpose is to
decide **what to stay compatible with and what to deliberately do differently**,
not to rank the products.

## Method, and what was actually run

The two SDKs were installed and read in a clean virtualenv; every signature and
constant below was extracted with `inspect` from the installed package, not from
documentation or memory.

```text
python 3.15.0
e2b 2.63.0
daytona 0.231.0
```

Read from installed source (the `inspect` output is reproducible with the
commands in the appendix):

- method and parameter signatures, including keyword-only and defaulted ones;
- result and exception types, their fields, and their inheritance;
- retry constants and the retry loop;
- transport: how command output is delivered.

**Not run:** no live sandbox was created against either service. Both need an
API key, and there was none in this environment. So this document makes claims
about **interface shape and client-side semantics**, which source inspection
proves, and makes no claim about server behaviour, latency, or reliability.
Where a behaviour is only documented in a docstring, the sentence says so.

## The surfaces, side by side

| Concern | E2B 2.63.0 | Daytona 0.231.0 | `sandbox-go` |
|---|---|---|---|
| Client | `Sandbox` (a sandbox *is* the client) | `Daytona` control plane, `Sandbox`/`process` data plane | `Client` → `*Sandbox`, `*Execution` by ID |
| Create | `Sandbox.create(template, timeout, metadata, envs, idempotency_key, ...)` | `Daytona.create(CreateSandboxParams, timeout)` | `CreateSandbox(CreateSandboxRequest)` |
| Lifecycle | `kill()`, `pause(mode="full"\|"filesystem")`, `connect()` | `stop()`, `start()`, `pause()`, `delete()` | `KillSandbox`, `PauseSandbox`, `ResumeSandbox` |
| Run a command | `commands.run(cmd: str, timeout: float = 60, on_stdout, on_stderr, background, stdin, retain_output)` | `process.exec(command: str, timeout: int)` (blocking) or a session + `get_session_command_logs_async` | `Exec(ctx, id, argv []string)` or `StartExecution` + `StreamExecution` |
| Command form | one **string**, run through a shell | one **string**, run through a shell | **argv**, no shell |
| Output | `CommandResult{stdout, stderr, exit_code, error}` | `ExecuteResponse{exit_code, result{stdout, stderr, error}}` | `Execution{stdout, stderr, exit_code, termination_reason}` |
| Nonzero exit | **raises** `CommandExitException`, which *is* a `CommandResult` | returns the exit code | returns `*Execution` **and** an error matching `ErrNonZeroExit` |
| Resume a stream | `commands.connect(pid)` reattaches by PID | session log stream | `StreamExecution` + `Last-Event-ID` |
| Transport | event stream (`as_stream`) for command output | WebSocket for session logs | SSE + `Last-Event-ID` |
| Timeout units | seconds: `int` on the sandbox, `float` on a command | seconds: `int` | seconds: `int`, field named `*_seconds` |
| Timeout action | `set_timeout()`, `pause(mode=...)`, `SandboxLifecycle` | `auto_stop_interval`, `auto_pause_interval`, `ttl_minutes` | `timeout_action: kill\|pause` + `idle_timeout_*` |
| Idempotency | `idempotency_key` on **create** | not found in the client surface | key on **execution start** |
| Error model | 25 exception classes | 41 exception classes, one per condition | one `*Error` + `errors.Is` sentinels |
| Retry | statuses `{429, 502, 503, 504}` or a `Should-Retry: true` header, backoff `0.1·2^n` capped at 10s, jitter `0.5 + rand·0.5`, `Retry-After` authoritative | none in the client (no retry module in the package); `queue_timeout` is server-side | statuses `{429, 500, 502, 503, 504}`, backoff `0.2·2^n` capped at 2s, honours `Retry-After`, only for repeatable requests |

## What each design is optimising for

**E2B** optimises for a single object that *is* the sandbox: `Sandbox.create()`
returns something you call methods on, and `connect()` gives you the same
object back later. Streaming is callback-based (`on_stdout`, `on_stderr`), which
is the natural shape in Python. Its pause has an explicit and honest
`mode` parameter — `"full"` keeps memory and files, `"filesystem"` drops the
processes — and the docstring spells out that a filesystem pause cold-boots on
resume. That is the clearest statement in either SDK of what a pause actually
preserves.

The retry loop is worth reading closely, because it is the closest of the two to
ours: `RETRYABLE_STATUSES = {429, 502, 503, 504}`, backoff `min(0.1 · 2^n, 10s)`
multiplied by `0.5 + random() · 0.5`, and a `Retry-After` header that is
authoritative when present. It also honours a non-standard `Should-Retry: true`
response header, which lets the server mark a status that is not in the list.

**Daytona** separates a control plane (`Daytona`: create/get/list/delete) from
the sandbox's own data plane (`Sandbox.process`, `fs`, `git`, `computer_use`).
Its blocking `exec` returns a result object; long-running work goes through
sessions with a WebSocket log stream. Its timeout vocabulary is rich and
lifetime-oriented (`auto_stop_interval`, `auto_pause_interval`,
`auto_archive_interval`, `auto_delete_interval`, `ttl_minutes`), more so than
either of the others.

**`sandbox-go`** optimises for the questions this project is built around:
which requests are safe to retry, how a stream resumes without loss or
duplication, and how a 137 is classified. That is why it is the only one of the
three with an explicit idempotency key on the *execution* (the retryable
command), a server-side event log with `Last-Event-ID`, and a
`termination_reason` field rather than only an exit code.

## Decisions: what we keep compatible, what we deliberately differ on

### Kept compatible

1. **Seconds everywhere, named with their unit.** E2B and Daytona both use
   seconds; a client migrating from either should not have to convert. We go one
   step further and put the unit in the field name (`timeout_seconds`), because
   milliseconds and seconds are the single most common cross-SDK bug.
2. **A result object that always carries output.** `CommandResult` and
   `ExecuteResponse` both attach stdout/stderr to the outcome. We do the same,
   including on the error path.
3. **Resumable output.** E2B resumes by PID, Daytona by session; both accept
   that a dropped connection must not mean lost output. We resume by event ID.
   The *capability* is compatible even though the mechanism differs.
4. **`Retry-After` is honoured.** E2B parses it; we parse it and let it override
   the computed backoff.
5. **Explicit pause semantics.** E2B's `mode` parameter forced the question, and
   our answer is stated in `api.md`: pause freezes the container, memory stays,
   processes continue from the same point. We do not offer a filesystem-only
   mode, and we say so rather than leaving it ambiguous.

### Deliberately different

1. **argv, not a shell string.** Both competitors take `command: str` and run it
   through a shell, which means the caller owns quoting and the SDK cannot tell
   a single command from a pipeline. We take `argv []string`, execute without a
   shell, and therefore never interpolate user data into shell syntax. A caller
   who wants a shell asks for `sh -c` explicitly. This is a compatibility cost
   we accept: a migration is mechanical but real.
2. **One error type with sentinels.** E2B has 25 exception classes and Daytona
   41. Exhaustive classes are pleasant in Python, where `except SpecificError`
   reads well, but every new condition is a new type in every language — the
   exact drift a multi-language SDK is trying to avoid. One `*Error` with a
   stable `Code` plus `errors.Is` sentinels extends without breaking callers and
   maps 1:1 onto a JSON error body, which is what the wire actually carries.
3. **Nonzero exit is returned, not raised.** E2B raises `CommandExitException`,
   and does it well: the exception *is* a `CommandResult`, so output survives the
   raise. We return the `Execution` **and** an error matching `ErrNonZeroExit`.
   In Go that keeps the "check the error" idiom intact and makes `defer` cleanup
   natural; it also means a caller who ignored the error still holds the output.
4. **Termination is a reason, not an exit code.** Neither inspected surface
   exposes an OOM-specific outcome: the string `oom` appears in none of E2B's 25
   or Daytona's 41 exception names. Daytona does report one non-exit-code
   termination — `DaytonaSpotEvictedError`, for a spot instance going away —
   but not OOM. Both leave
   the caller to interpret 137. We classify `canceled`, `timeout`, `oom`,
   `evicted`, `exited` on the server, in a fixed precedence, because the exit
   code alone cannot distinguish them. This is the sharpest difference in the
   comparison and the one this project exists to make.
5. **Idempotency on the execution, not the create.** E2B's `idempotency_key`
   protects sandbox creation. The dangerous retry, though, is the one that runs
   a command twice. Our SDK always supplies a key for execution starts, so a
   retried start cannot begin a second process; sandbox creation is never
   retried at all.
6. **A tighter backoff cap.** E2B allows up to 10s between attempts; we cap at
   2s. For an agent waiting on a short command, a 10s stall is worse than a
   fast failure, and the caller can always retry at its own level.

## What this comparison changed in the code

- **`termination_reason` was kept as a first-class field.** Seeing that neither
  competitor exposes one was the argument for making it part of the contract
  rather than a convenience.
- **The idempotency key moved to `StartExecution`.** It is the retryable
  command; a key on create would have protected the wrong operation.
- **`Retry-After` overrides the computed delay** rather than being added to it,
  matching E2B's behaviour and naming the header's value authoritative.
- **The error surface stayed at one type.** Reading two SDKs with 66 exception
  classes between them was the strongest argument against copying that approach
  into a multi-language design.

## Limitations of this review

- No live calls; only installed-source inspection.
- E2B's `as_stream` and Daytona's WebSocket were identified by reading the
  client, not by observing a running stream.
- Both SDKs move quickly (0.231.0 is a pre-1.0 version for Daytona); names and
  defaults here are pinned to the versions above.
- Only these two products were reviewed. Cloudflare Sandbox, Modal, and runloop
  were not.

## Appendix: reproducing the inspection

```sh
python3 -m venv /tmp/compvenv
/tmp/compvenv/bin/pip install e2b daytona
/tmp/compvenv/bin/python - <<'PY'
import inspect, e2b
from e2b import Sandbox
from e2b.sandbox_sync.commands.command import Commands
print(inspect.signature(Sandbox.create))
print(inspect.signature(Sandbox.pause))
print(inspect.signature(Commands.run))
import e2b.retry as r
print(r.RETRYABLE_STATUSES, r.BACKOFF_BASE_SECONDS, r.BACKOFF_MAX_SECONDS)

from daytona import Daytona, Sandbox as DSandbox
from daytona._sync.process import Process
print(inspect.signature(Daytona.create))
print(inspect.signature(Process.exec))
print(inspect.signature(Process.get_session_command_logs_async))
PY
```
