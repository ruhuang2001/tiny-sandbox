# TypeScript SDK

Status: normative for the ESM package in [sandbox-ts](../sandbox-ts). It speaks
the existing [HTTP contract](api.md); Go and daemon interfaces are unchanged.
There are no runtime dependencies or Node built-in imports. Node 22+ and modern
browsers must supply fetch, ReadableStream, TextDecoder, AbortController and
Web Crypto. Declarations support TypeScript 5.9+ with DOM types.

## Install from this repository

This change prepares a package; it does not publish to npm. Build a tarball,
then install its actual local path in your application:

~~~sh
cd sandbox-ts
npm ci
npm run check
npm pack
# In your application:
npm install /path/to/tiny-sandbox/sandbox-ts/tiny-sandbox-sdk-0.0.1.tgz
~~~

~~~ts
import { Client, SandboxError } from '@tiny-sandbox/sdk';

const client = new Client({ domain: 'http://localhost:8080' });
const sandbox = await client.createSandbox({ timeout_seconds: 300 });
try {
  const result = await client.exec(sandbox.id, ['sh', '-c', 'echo hello; exit 42']);
  switch (result.reason) {
    case 'exited': console.log(result.execution.exit_code, result.execution.stdout); break;
    case 'timeout': case 'canceled': case 'oom': case 'evicted':
      console.log(result.reason, result.execution.stdout); break;
    case 'unknown': console.log(result.terminationReason); break;
    default: { const exhaustive: never = result; throw exhaustive; }
  }
} catch (error) {
  if (error instanceof SandboxError) console.error(error.code, error.execution);
  else throw error;
} finally {
  await client.killSandbox(sandbox.id);
}
~~~

## Configuration and methods

The constructor accepts an optional ClientConfig:

| Option | Meaning |
| --- | --- |
| domain | Nonempty option, then Node's SANDBOX_DOMAIN, then http://localhost:8080. Missing scheme becomes http://. Base paths are preserved; credentials, query, fragment and non-HTTP(S) schemes are rejected. |
| apiKey | Nonempty option, then Node's SANDBOX_API_KEY; sent as a bearer header, including streams. Environment access is optional and requires no Node imports. |
| maxRetries | Omitted or zero means three; negative disables retries, matching Go. Positive integers count additional attempts. Noninteger/nonfinite values are rejected. |
| fetch | Fetch-compatible injectable transport; defaults to global fetch. Must honor AbortSignal and streaming bodies. |

No global timeout is imposed. Redirects are rejected instead of changing
request semantics. Methods use camelCase, request/response fields use the HTTP
snake_case names. Dates remain RFC3339 strings, binary files are Uint8Array.
Optional fields use undefined; explicit zero is transmitted unchanged.
RequestOptions contains an optional signal and is always the last argument.

~~~ts
createSandbox(request?: CreateSandboxRequest, options?: RequestOptions): Promise<Sandbox>
getSandbox(sandboxID, options?): Promise<Sandbox>
listSandboxes(options?): Promise<Sandbox[]>
pauseSandbox(sandboxID, options?): Promise<Sandbox>
resumeSandbox(sandboxID, options?): Promise<Sandbox>
killSandbox(sandboxID, options?): Promise<Sandbox>

startExecution(sandboxID, request: ExecuteRequest, options?): Promise<Execution>
getExecution(sandboxID, executionID, options?): Promise<Execution>
listExecutions(sandboxID, options?): Promise<Execution[]>
cancelExecution(sandboxID, executionID, options?): Promise<Execution>

streamExecution(sandboxID, executionID, options?): AsyncIterableIterator<Event>
streamExecution(sandboxID, executionID, onEvent: EventHandler, options?): Promise<Execution>
exec(sandboxID, command: readonly string[], options?): Promise<ExecutionOutcome>
execStream(sandboxID, command: readonly string[], onEvent: EventHandler, options?): Promise<ExecutionOutcome>

readFile(sandboxID, path, options?): Promise<Uint8Array>
writeFile(sandboxID, path, bytes: Uint8Array, options?): Promise<void>
~~~

EventHandler returns void or Promise<void>; callbacks are awaited in order.
Execution stdout/stderr are optional because lists omit them. All request and
response interfaces, Event, ExecutionOutcome, SandboxError and executionOutcome
are exported from the package root. Deep imports are not supported.

## Outcomes and errors

Completed commands resolve values, including nonzero exits. ExecutionOutcome
is a discriminated union with reason exited, canceled, timeout, oom, evicted or
unknown, plus execution. The unknown variant carries the original
terminationReason. Only exited with exit_code zero is command success.
executionOutcome(snapshot) converts a terminal snapshot, rejecting nonterminal
ones with invalid_state. An exhaustive strict switch is shown above.

SandboxError covers API, transport, protocol, callback and cancellation failures.
Inspect code, statusCode (zero for client failures), cause, execution (a full
snapshot or just IDs), idempotencyKey and the retryable boolean getter. Retryable
classifies a failure; it does not authorize retrying unsafe operations like
creation. API codes, including future ones, are preserved. Plain-text HTTP
errors use http_error.

Client codes: invalid_config, invalid_request, decode (invalid JSON), transport,
stream (invalid SSE/protocol), callback, canceled, cancel_unconfirmed and
invalid_state. Protocol/callback failures never retry. Completed timeout/nonzero
outcomes do not throw; an aborted caller signal does, even after cleanup succeeds.

## Streaming and retries

The SDK uses **fetch plus one SSE parser everywhere**. EventSource cannot send
Authorization or enforce the SDK retry budget/backoff. An unauthenticated
connection uses the same implementation and guarantees.

- On reconnect, Last-Event-ID is the last delivered ID. Events at or below it
  are suppressed, so delivery within a call is ordered and duplicate-free.
- The cumulative maxRetries budget never resets on output. EOF before complete
  is retryable; complete ends iteration without reconnecting.
- Output events have id, type stdout/stderr and data. A truncated event has
  limit_bytes. A complete event has execution. Unknown event types become type
  unknown, with original event_type and raw data; their IDs advance the cursor.
- Parsing handles comments, multiline data, CR/LF/CRLF, fragmented UTF-8 and a
  final unterminated frame. IDs must be positive safe integers (the bounded
  server event count fits). Lines/frames are capped at 16 Mi UTF-16 code units;
  the valid worst-case complete event fits. Incoming data is scanned once.
- Breaking for-await or calling iterator.return closes the connection, even
  with next pending. Neither cancels the execution. Abandoning an iterator
  without closing it is not a cleanup mechanism.
- Callback failures always become callback with the original cause, even when
  application code throws SandboxError. They close the stream without retry or
  remote cancellation. Independently aborting the caller signal still cancels.
- Retention is a server limit. Truncated events/output_truncated mean output was
  discarded and cannot be recovered through replay. Backpressure does not
  increase retention.

Repeatable ordinary requests retry transport failures (including interrupted
response bodies) and HTTP 429/500/502/503/504. Repeatable operations are GET,
DELETE, file PUT, pause/resume/cancel and keyed starts. Creation is attempted
once. A complete malformed JSON body is a decode error, not a transport retry.

Delay is sampled uniformly between half and all of the cap
min(2000ms, 200ms * 2^attempt). Positive integer Retry-After seconds override
ordinary delays; zero, dates, fractions and invalid values use backoff.
Like Go, SSE reconnects do not apply Retry-After. Waits and in-flight fetches
stop when their signal aborts.

## Cancellation and start recovery

startExecution copies the request and, absent a supplied key, generates 128
random bits as a 32-digit hex idempotency key. Retries reuse key and command.
A signal already aborted before starting sends no request.

An aborted in-flight start triggers a fresh five-second deadline: repeat the
start with the same key, learn the ID, then cancel it. Failed or nonterminal
recovery returns cancel_unconfirmed with idempotencyKey and any known snapshot
or IDs, allowing later reconciliation. Keys expire with server retention; do
not replay an ancient key as if it were permanent.

Aborting an active stream starts cancellation with a fresh five-second deadline,
even while an iterator is suspended after an event or an async callback waits.
The next consumer observation reports cleanup. A canceled error requires a
cancel snapshot with state completed; otherwise the error is cancel_unconfirmed.
The attached execution may have exited naturally before cleanup; inspect its
actual reason. HTTP 200 alone never confirms termination.

cancelExecution itself returns the raw snapshot, which may still be running
during removal fallback; it does not use the recovery wrapper. Aborting ordinary
get/list/file requests only aborts those requests.

## Worked failure scenario and evidence

~~~sh
bash scripts/e2e-typescript.sh artifacts/typescript-e2e
~~~

The entry gate runs one long-lived numbered-output command in real Docker
behind an authenticated proxy. After its first output event the proxy drops
the stream, then inserts that event into the reconnect. The SDK sends
Authorization and Last-Event-ID, delivers an exact duplicate-free four-line
prefix, then aborts the caller signal. Both recovery and a fresh GET must show
a canceled terminal snapshot retaining every delivered byte. Additional abort
scenarios cover ambiguous starts and nonterminal cancel responses.

The runner records requests, IDs, event content/hashes, results, build/daemon
logs and scoped container cleanup. The [failure matrix](../sandbox-ts/e2e/FAILURE-MATRIX.md)
precedes implementation. Faults wrap real server responses; there is no fake-only
unit suite. Package validation installs the tarball, compiles a strict consumer
and exercises a browser bundle without Node globals against the real daemon.

## Compatibility and Go parity

Existing HTTP paths, Go public types and Go error behavior are unchanged. The
TypeScript package has its own semver version. Preserve constructor defaults,
signatures, exported types, codes, outcome discriminants, units and iterator
cleanup semantics in compatible releases. Optional additions are preferred;
changing zero-retry behavior or throwing completed outcomes would break clients.
Keep the explicit unknown outcome for new wire reasons instead of adding a
closed union member in a patch/minor version.

Unknown response fields are retained and response enum strings remain open.
Additive resource fields can be absent. Unsupported controls are not filled in
as if they were enforced. Server v0.0.1 supports pids_limit, nano_cpus, network
and execution timeout_seconds; older development servers may silently ignore
them. Use a known compatible server when those controls matter. No capability
negotiation or client-side per-execution timer is invented.

| Guarantee | Go and TypeScript |
| --- | --- |
| Auth, lifecycle, files, keyed starts | Same HTTP behavior |
| Replay cursor, de-duplication, total budget, jitter | Same guarantees; TypeScript uses fetch for Node and browsers |
| Callback failures | TypeScript always wraps and never retries; Go has a documented typed-error caveat |
| Completed outcomes | Go Exec returns outcome errors; TypeScript resolves a discriminated union |
| Abort/context cleanup | Both use fresh five-second recovery; TypeScript also requires a terminal snapshot |
| Stream consumption | Go callback; TypeScript async iterator plus async callback overload |

Browser-safe packaging does not enable cross-origin access on sandboxd. Serve
through the same origin or use a trusted CORS/preflight proxy allowing
Authorization, Content-Type, Last-Event-ID and the used methods. Expose
Retry-After for browser retries. Never ship privileged bearer keys to untrusted
browser users. The SDK does not change daemon CORS behavior.
