# CLI

Status: normative. Contract for `sandboxctl`. It is a thin layer over the [Go SDK](sdk.md); it must not talk HTTP directly except for `doctor`'s health probe.

## Global behavior

- Global flags `--domain` and `--api-key` go before the subcommand. Nonempty values override `SANDBOX_DOMAIN` and `SANDBOX_API_KEY`, for example `sandboxctl --domain http://127.0.0.1:8080 list`.
- Execution stdout goes to stdout; execution stderr, progress, and status lines go to stderr. Inspection, lifecycle, list, version, help, and doctor results use stdout. `sandboxctl run -- cmd > out.txt` captures the command's stdout only.
- Errors are printed to stderr as `sandboxctl: <message>`, followed by `hint: <next step>` when one is known.
- Ctrl+C or SIGTERM cancels the context; commands that wait on an execution cancel it remotely through the SDK.

## Commands

| Command | Behavior |
|---|---|
| `doctor` | environment and connectivity checks, see below |
| `run [flags] -- CMD [ARG...]` | create a sandbox (or use `--sandbox ID`), start `CMD`, stream output, print the final status |
| `run [flags] -- code 'PYTHON'` | shorthand for `python3 -c 'PYTHON'`; a newly created sandbox uses `python:3.12-alpine` unless `--image` is given |
| `attach SANDBOX EXECUTION` | stream an existing execution from the beginning; Ctrl+C cancels it |
| `logs SANDBOX EXECUTION` | print retained stdout/stderr and the current status without streaming |
| `inspect SANDBOX` | print the sandbox |
| `list` | print sandboxes, one per line: `ID STATE IMAGE CREATED` |
| `pause SANDBOX`, `resume SANDBOX`, `kill SANDBOX` | lifecycle operations; print the resulting sandbox |
| `version` | print the build version (injected with `-ldflags`) |
| `help` | usage |

`run` flags: `--sandbox ID`, `--image`, `--memory MB`, `--timeout S`, `--timeout-action kill|pause`, `--idle-timeout S`, `--idle-action kill|pause`, `--rm` (request cleanup of a sandbox created by this invocation). Creation flags together with `--sandbox` are an error. `--rm` does not remove a sandbox supplied through `--sandbox`. Cleanup errors are currently ignored; verify remote state when cleanup must be confirmed.

Numeric creation flags are forwarded only when positive. In particular,
`--timeout 0` does not disable the default lifetime; use the SDK or HTTP API
with an explicit zero to disable it.

## Status lines (stderr)

These formats are stable; end-to-end tests match them.

```text
sandbox: sbx_123 (created; remove with: sandboxctl kill sbx_123)
execution: exe_456
execution exe_456: completed (exited), exit 0
```

The first line reads `sandbox: sbx_123` when `--sandbox` was given. The last line is printed whenever the final execution is known, including after Ctrl+C. If streaming fails without a final result, the CLI prints the initial running snapshot instead. Without `--rm`, a sandbox created by `run` is kept for inspection, except when the start itself was canceled: then the CLI kills the sandbox it just created.

## Exit codes

| Situation | Exit code |
|---|---|
| success | 0 |
| command exited with code 1–125 | that code |
| cancellation reported as `ErrCanceled` | 130 |
| invalid usage or flag parsing | 2 |
| anything else (API errors, OOM, timeout, eviction, exit codes 126 and above) | 1 |

Exit codes 126 and 127 are not passed through. In shell convention they mean
"command found but not executable" and "command not found"; both describe
failures of the sandbox environment rather than of the command, and a caller
scripting around `sandboxctl` would misread them. They collapse to 1 like every
other non-command failure.

## Hints

| Condition | Hint |
|---|---|
| connection refused | `start sandboxd or set SANDBOX_DOMAIN` |
| 401 | `set a valid SANDBOX_API_KEY or --api-key` |
| 404 | `check the sandbox and execution IDs` |
| `oom` | `increase --memory or reduce the command's memory use` |
| `timeout` | `increase --timeout` |
| `evicted` | `the container was stopped outside sandboxd` |
| `busy` | `wait for the running execution or cancel it` |

## doctor

Prints checks to stdout as `<check>: ok`, `<check>: ok (<detail>)`, or
`<check>: fail (<detail>)`. Remote Docker checks print `docker: skipped (remote domain)`. The summary is `Ready` or `not ready`; failure hints go to stderr.
Exit 0 only if all applicable checks passed.

1. `config`: domain and whether an API key is set (never print the key).
2. `health`: `GET /healthz` within 3 seconds.
3. `auth`: `GET` the fixed probe ID `sbx_doctor_probe`; `404` means authorized, `401` means the key is wrong or missing.
4. `docker`: only for a local domain (loopback or `localhost`); `docker info` reachable.
5. `sandbox`: create a sandbox, run `echo doctor-ok`, check the output, kill the sandbox.

## Implementation references

- [Global flags, output, and exit codes](../cmd/sandboxctl/main.go).
- [Run flags and cleanup](../cmd/sandboxctl/run.go).
- [Attach, logs, and lifecycle commands](../cmd/sandboxctl/ops.go).
- [Doctor checks](../cmd/sandboxctl/doctor.go).
