# TypeScript SDK E2E failure matrix

Written before the SDK implementation and before E2E test code for issue #3.
Every scenario uses a real sandboxd with a real Docker backend. The HTTP proxy
only injects transport/response faults around requests sent to that server.

| Failure | Observable acceptance evidence |
| --- | --- |
| Missing/incorrect bearer credentials | One rejected request, typed nonretryable auth error; valid authenticated lifecycle succeeds |
| Lifecycle or binary file contract drifts | Create/list/get/pause/resume/write/read/kill round trip, exact bytes including zero and invalid UTF-8 |
| Completed nonzero execution throws or reason mapping lies | Exit 0 and exit 42 resolve exited outcomes with original exit codes; server timeout resolves timeout |
| Midstream network drop loses or duplicates output | Forced close after a real stdout event; resumed request has Authorization and Last-Event-ID; injected duplicate is suppressed and exact ordered output survives |
| Reconnected long-running command cannot be canceled reliably | One numbered-output command survives a forced drop and duplicate replay, delivers exact unique prefix, then AbortSignal confirms canceled terminal server state with that retained prefix |
| Clean EOF cuts an SSE frame after a delivered event | Incomplete stdout JSON at EOF reconnects from the previous delivered cursor; exact output survives without duplicate IDs or premature cursor advance |
| Complete event lacks its final blank line | Valid final unterminated complete frame delivers once and finishes without reconnect |
| Retry budget resets when output arrives | Repeated drops each following new real output stop after initial attempt plus maxRetries |
| Retry-After is ignored | Injected retryable status with Retry-After delays the next repeatable request by the prescribed duration |
| Unsafe sandbox creation retries | Lost create response results in one forwarded POST and one real sandbox only |
| Keyed starts duplicate work | Lost start response retries with one unchanged key and returns the sole server execution; changed command with that key conflicts |
| Abort during ambiguous start leaves work running | Hold real successful start response, abort, observe same-key recovery and confirmed remote cancellation |
| Recovery or cancellation cannot confirm terminal state | Typed cancel_unconfirmed preserves idempotency key/execution and does not claim canceled |
| Callback exception cancels work or hides thrown SDK error | Error and SandboxError callbacks both become code callback; zero cancel requests; remote execution remains active |
| AbortSignal is mistaken for a callback error | Abort produces fresh cancellation request and code canceled only after completed server state |
| Async iterator break cancels or leaks stream | Early break closes stream without cancel; remote work remains running and can be explicitly canceled |
| Truncated output discards terminal event | Real output over retained limit produces truncated event followed by complete |
| Future server fields/reasons/events break decoding | Additive fields survive normal parsing; optional resource fields may be omitted; unknown reason resolves unknown; unknown SSE event retains original name/data |
| Retry configuration has ambiguous zero/negative values | Zero uses the documented default; negative yields only one attempt |
| Test leaves processes, ports, or containers | Finally blocks close proxy, kill owned sandboxes, stop isolated daemon; record scoped container leak check |
| Iterator return queues behind a silent pending next | Return settles promptly, closes its HTTP connection, and sends no cancel while real work remains running |
| Abort while iterator is suspended or callback awaits is ignored | Remote cancel starts before another next call or callback release; terminal server state is confirmed |
| Truncated ordinary HTTP bodies are treated as valid or not retried | Proxy cuts a real JSON response below Content-Length; repeatable GET succeeds on the second attempt |
| Retryable status handling differs by status | Real GET responses replaced with 429/500/502/504 retry and return the original resource |
| Browser entry accidentally depends on Node globals or built-ins | esbuild browser bundle executes authenticated real-server lifecycle/stream in a VM exposing Web APIs without process, Buffer, require, or Node imports |
| Published tarball cannot be consumed independently | npm-packed tarball installs in isolated artifact consumer; installed ESM import reaches actual daemon and strict TypeScript fixture compiles against installed declarations |
| Package metadata omits docs or retains stale sourcemaps | npm pack file list includes README.md and LICENSE and excludes every .map file after clean build |

## Repeatable artifacts

`scripts/e2e-typescript.sh [artifact-dir]` builds the SDK and isolated sandboxd,
uses a free loopback port, and records `results.json`, `requests.jsonl`,
`events.jsonl`, build output, daemon output, and container cleanup evidence.
Artifacts are overwritten per selected output directory; network fault rules
are scoped to one scenario and exact request paths. No unit test or fake-only
server is part of this suite.
