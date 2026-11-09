# Python SDK failure model (written before implementation)

The suite uses a real sandboxd and Docker. An HTTP fault proxy sits between the
installed Python distribution and the server, retaining request/event traces.
No post-implementation unit tests or mocked execution backend are added.

- Creation is not idempotent: transport errors, truncated responses and 5xx must
  never create a second sandbox. Repeatable methods respect the retry bound,
  status allowlist, jitter window and positive integer Retry-After.
- Start can be accepted before the response is lost. Retries and cancellation
  recovery reuse one 128-bit key, run once and preserve the key on failure.
- Cancellation before dispatch sends nothing. Cancellation in flight uses one
  independent five-second cleanup budget, including start recovery and
  confirmation. A running cancel snapshot, failed cleanup or lost response
  cannot prove termination and must produce cancel_unconfirmed with IDs/key.
- A connection can close mid-SSE frame or replay delivered IDs after reconnect.
  Only complete frames advance the cursor; deliver each event once in order,
  preserve UTF-8, send Last-Event-ID and never reset reconnect count on progress.
- SSE may include comments, multiline data, unknown future event types, invalid
  IDs/JSON or oversized lines. Additive events/fields must not break clients;
  malformed known payloads fail boundedly without canceling remote work.
- A callback can raise any ordinary exception, including a retryable SDK error.
  It must never reconnect or cancel work. Explicit caller cancellation must.
- Nonzero, timeout, canceled, OOM and eviction preserve stdout/stderr, exit code
  and truncation metadata in one stable exception family. Streaming returns
  outcomes directly. Exit 137 alone must not imply OOM.
- Server timeout and local cancellation are distinct: timeout_seconds is sent
  to the server; a local token deadline cancels and never invents timeout.
- Files preserve arbitrary bytes and escaped paths. Sandbox context exit
  deletes using a fresh cleanup budget even when the body raises/cancels.
- Old responses can omit additive resource fields; future responses can add
  fields and unknown strings. Omitted options differ from explicit zero.
- Auth, non-JSON errors, absent resources, busy and invalid state retain HTTP
  status/code. Redirects must not forward credentials to another origin.
- The distribution installs independently and includes py.typed. Existing Go
  consumers, CLI and server continue to compile and pass their checks.
- Each run emits repeatable commands, checks, HTTP fault/event evidence,
  source/build identities and cleanup results; containers must not leak.
- A TLS peer can accept TCP and never complete its handshake. Cancellation
  before HTTP dispatch must return promptly without recovery starting work.
- Malformed typed start/complete fields (including boolean exit codes) must
  never become success; a malformed accepted start retains its recovery key.
