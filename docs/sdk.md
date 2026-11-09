# Go SDK

Status: normative. Contract for `sandbox-go` (package `sandbox`). The SDK speaks the contract in [api.md](api.md).

## Configuration

```go
type Config struct {
    Domain     string       // default: $SANDBOX_DOMAIN, then "http://localhost:8080"
    APIKey     string       // default: $SANDBOX_API_KEY
    HTTPClient *http.Client // default: a client without a global timeout (streams are long-lived)
    MaxRetries int          // default 3; 0 means use the default, negative disables retries
}

func NewClient(cfg Config) (*Client, error)
```

A domain without a scheme gets `http://`. Nonempty `Domain` and `APIKey` values override the environment; empty strings use the environment defaults.

## Methods

All methods take a `context.Context` first and return typed results.

```go
// sandboxes
CreateSandbox(ctx, CreateSandboxRequest) (*Sandbox, error)
GetSandbox(ctx, sandboxID) (*Sandbox, error)
ListSandboxes(ctx) ([]*Sandbox, error)
PauseSandbox(ctx, sandboxID) (*Sandbox, error)
ResumeSandbox(ctx, sandboxID) (*Sandbox, error)
KillSandbox(ctx, sandboxID) (*Sandbox, error)

// executions
StartExecution(ctx, sandboxID, ExecuteRequest) (*Execution, error)
StreamExecution(ctx, sandboxID, executionID, onEvent func(Event) error) (*Execution, error)
GetExecution(ctx, sandboxID, executionID) (*Execution, error)
ListExecutions(ctx, sandboxID) ([]*Execution, error)
CancelExecution(ctx, sandboxID, executionID) (*Execution, error)

// conveniences
Exec(ctx, sandboxID, command []string) (*Execution, error)                      // = ExecStream with nil onEvent
ExecStream(ctx, sandboxID, command []string, onEvent func(Event) error) (*Execution, error)

// files
ReadFile(ctx, sandboxID, path string) ([]byte, error)
WriteFile(ctx, sandboxID, path string, data []byte) error
```

Optional request fields use pointers (`*int`) so that "unset" and `0` are distinct.

`Exec` and `ExecStream` are `StartExecution` followed by `StreamExecution`. When execution completes, they return its Execution together with an error: `nil` only when `termination_reason` is `exited` and `exit_code` is 0.

## Errors

```go
type Error struct {
    StatusCode     int        // 0 for client-side errors
    Code           string     // API code, or "canceled" / "cancel_unconfirmed" / "nonzero_exit" / reason name
    Message        string
    Execution      *Execution // when available; carries retained output and exit code
    IdempotencyKey string     // set for cancel_unconfirmed during start
    Cause          error
}
func (e *Error) Error() string
func (e *Error) Unwrap() error
func (e *Error) Is(target error) bool
func (e *Error) Retryable() bool

var (
    ErrNonZeroExit       // exited with a nonzero code
    ErrTimeout           // termination_reason "timeout"
    ErrCanceled          // canceled outcome or an accepted SDK cancellation response; inspect execution state
    ErrOOM               // termination_reason "oom"
    ErrEvicted           // termination_reason "evicted"
    ErrCancelUnconfirmed // the caller's context ended but the SDK could not confirm the server stopped the work
    ErrNotFound, ErrUnauthorized, ErrBusy, ErrInvalidState
)
```

`errors.Is(err, sandbox.ErrOOM)` must work on every error the SDK returns for that outcome. When a final execution is available, `Error.Execution` holds its retained `stdout`, `stderr`, and `exit_code`. Check `output_truncated` before treating output as complete. Transport or start failures may return no execution; an unconfirmed cancellation may carry only IDs.

## Retries

- Retry candidates: transport errors before a complete response, truncated response bodies, and status 429, 500, 502, 503, 504.
- Only requests that are safe to repeat are retried: `GET`, `DELETE`, `PUT` (files), pause, resume, cancel, and execution starts carrying an idempotency key. `POST /v1/sandboxes` is attempted exactly once.
- Backoff cap: `min(2s, 200ms * 2^attempt)`. The delay is sampled uniformly from half the cap to the cap. For ordinary requests, a positive integer `Retry-After` value in seconds overrides the delay; zero, HTTP-date values, and invalid values use the calculated backoff.
- Retries stop as soon as the context is done.

## Starting an execution

1. If `ExecuteRequest.IdempotencyKey` is empty, generate 128 random bits, hex-encoded.
2. If the context is already done, return its error without sending anything.
3. Send the start request, retrying as above with the same key.
4. If the context ends while the start is in flight, the SDK cannot know whether the server accepted it. Using a fresh context with a 5-second deadline, it repeats the start with the same key (the server returns the existing execution, or creates one if the original request was never accepted), then cancels that execution. Result: `ErrCanceled` with the Execution. If this recovery fails, return `ErrCancelUnconfirmed` with `IdempotencyKey` set, so the caller can clean up.

## Streaming

- `StreamExecution` connects to the event endpoint and calls `onEvent` for `stdout`, `stderr`, `truncated`, and `complete`, in order, each exactly once.
- On disconnect it reconnects with `Last-Event-ID` set to the last delivered ID and drops any event with an ID not greater than it.
- Reconnect attempts use the retry backoff. The current implementation allows up to `MaxRetries` reconnects during one `StreamExecution` call; receiving output does not reset this budget.
- It returns the Execution from the `complete` event. `StreamExecution` does not turn a nonzero exit or termination reason into an outcome error; `Exec` and `ExecStream` do that.
- If `onEvent` returns an ordinary error, the SDK closes the stream and returns a callback error wrapping it. The execution keeps running: a rendering bug in the caller must not terminate remote work. Call `CancelExecution` explicitly when the command itself should stop.
- If the caller's context ends, the SDK calls `CancelExecution` with a fresh 5-second context, because the original one can no longer carry requests. Success: `ErrCanceled` with the Execution. Failure: `ErrCancelUnconfirmed` with an Execution holding the IDs.
- Ending a stream for any other reason never cancels the execution.

### Current streaming limitations

These are implementation limits, not additional guarantees:

- Reconnects count against one budget per call, even when events arrive between
  disconnections. The counter does not reset on progress.
- SSE reconnects do not apply the response's `Retry-After` header.
- Callback errors already typed as `*sandbox.Error` are passed through. A
  retryable SDK error returned by a callback may therefore trigger a reconnect;
  return an application error to stop consumption reliably.
- In context-cancellation recovery, a successful cancel HTTP response becomes
  `ErrCanceled` without
  checking that the snapshot is terminal. During the server's removal fallback,
  inspect the execution state; see [cancellation and removal](api.md#cancellation-and-removal).
- Replay covers retained output only. A `truncated` event means further output
  was discarded; reconnecting cannot recover it.
- Output is treated as UTF-8 text. Bytes that are not valid UTF-8 arrive as
  U+FFFD, because output events carry JSON strings.
- One SSE line is capped at 16 MiB. That covers the largest `complete` event
  the server can send: `command` comes from a request body of up to 1 MiB, the
  retained output is 1 MiB, and JSON escaping can expand each byte to six
  characters, so the two parts together can approach 12 MiB. A server that sent
  a larger single event would end the stream with a `stream` error.

## Dependencies

Standard library only. Minimum Go version is the `go` directive in `go.mod`.

## Implementation references

- [Client configuration](../sandbox-go/client.go) and [HTTP retries](../sandbox-go/retry.go).
- [Execution start and recovery](../sandbox-go/exec.go).
- [SSE reconnects and context cancellation](../sandbox-go/stream.go).
- [Error matching and outcomes](../sandbox-go/errors.go).
