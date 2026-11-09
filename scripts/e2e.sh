#!/usr/bin/env bash
# End-to-end suite for tiny-sandbox: a real sandboxd, a real Docker daemon, and
# the real CLI. Every assertion in docs/cli.md is exercised here.
#
#   bash scripts/e2e.sh [artifact-dir]
#
# The script starts its own sandboxd on a free loopback port, runs the CLI
# against it, records evidence, and removes its containers on exit.
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/artifacts/e2e}
mkdir -p "$OUT"

PORT=${E2E_PORT:-18081}
export SANDBOX_DOMAIN="http://127.0.0.1:$PORT"
# The server labels every container with its listen address. Scoping the leak
# check to this exact value means it asserts what this run created, not what
# happens to carry the label's key from anything else on the host.
OWNER="127.0.0.1:$PORT"
unset SANDBOX_API_KEY || true

BIN=$ROOT/bin
COVER_DIR=$OUT/coverage
SERVER_LOG=$OUT/sandboxd.log
RESULTS=$OUT/results.txt
: >"$RESULTS"
failures=0
server_pid=""

cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null
    wait "$server_pid" 2>/dev/null
  fi
  ids=$(docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER")
  [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
}
trap cleanup EXIT

# check NAME CONDITION-DESCRIPTION ACTUAL EXPECTED
pass() { printf 'ok   %s\n' "$1" | tee -a "$RESULTS"; }
fail() {
  printf 'FAIL %s: %s (got %s, want %s)\n' "$1" "$2" "$3" "$4" | tee -a "$RESULTS"
  failures=$((failures + 1))
}
check() { # name, description, actual, expected
  if [ "$3" = "$4" ]; then pass "$1"; else fail "$1" "$2" "$3" "$4"; fi
}
contains() { # name, description, haystack-file, needle
  if grep -qF -- "$4" "$3"; then pass "$1"; else fail "$1" "$2" "no '$4'" "present"; fi
}
lacks() { # name, description, haystack-file, needle
  if grep -qF -- "$4" "$3"; then fail "$1" "$2" "found '$4'" "absent"; else pass "$1"; fi
}

note() { printf '\n== %s ==\n' "$1" | tee -a "$RESULTS"; }

# ---------------------------------------------------------------- build

note "build"
if [ "${E2E_COVERAGE:-0}" = "1" ]; then
  # Instrument the binaries so the end-to-end run contributes coverage for
  # the packages only it reaches: the Docker backend's real paths and the
  # CLI's process-level behaviour.
  mkdir -p "$COVER_DIR"
  BIN=$OUT/bin
  mkdir -p "$BIN"
  {
    # -covermode=atomic matches the unit run, so both profiles merge into
    # one covdata set; a mode clash is a merge error, not a warning.
    (cd "$ROOT" && go build -cover -covermode=atomic -coverpkg=./... -o "$BIN/sandboxd" ./cmd/sandboxd) &&
    (cd "$ROOT" && go build -cover -covermode=atomic -coverpkg=./... -o "$BIN/sandboxctl" ./cmd/sandboxctl)
  } >"$OUT/build.txt" 2>&1 || {
    echo "instrumented build failed; see $OUT/build.txt" | tee -a "$RESULTS"
    exit 1
  }
  export GOCOVERDIR="$COVER_DIR"
  pass "instrumented binaries built"
else
  (cd "$ROOT" && make build) >"$OUT/build.txt" 2>&1 || {
    echo "build failed; see $OUT/build.txt" | tee -a "$RESULTS"
    exit 1
  }
  pass "binaries built"
fi

# ---------------------------------------------------------------- server

note "server startup"
"$BIN/sandboxd" -addr "127.0.0.1:$PORT" >"$SERVER_LOG" 2>&1 &
server_pid=$!
for _ in $(seq 1 50); do
  if curl -fsS "$SANDBOX_DOMAIN/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -fsS "$SANDBOX_DOMAIN/healthz" >"$OUT/healthz.json" 2>&1
check "healthz" "health endpoint answers" "$(grep -c '"status":"ok"' "$OUT/healthz.json")" "1"

# ---------------------------------------------------------------- doctor

note "doctor"
"$BIN/sandboxctl" doctor >"$OUT/doctor.txt" 2>"$OUT/doctor.err"
code=$?
check "doctor exit" "doctor succeeds against a local server" "$code" "0"
contains "doctor config" "config line printed" "$OUT/doctor.txt" "config: ok"
contains "doctor health" "health line printed" "$OUT/doctor.txt" "health: ok"
contains "doctor auth" "auth line printed" "$OUT/doctor.txt" "auth: ok"
contains "doctor docker" "docker line printed" "$OUT/doctor.txt" "docker: ok"
contains "doctor sandbox" "sandbox line printed" "$OUT/doctor.txt" "sandbox: ok"
contains "doctor ready" "prints Ready" "$OUT/doctor.txt" "Ready"

# ---------------------------------------------------------------- run

note "run: success"
"$BIN/sandboxctl" run -- sh -c 'echo out; echo err >&2' >"$OUT/run.out" 2>"$OUT/run.err"
code=$?
check "run exit" "a clean command exits 0" "$code" "0"
check "run stdout" "stdout carries only command output" "$(cat "$OUT/run.out")" "out"
contains "run sandbox line" "created sandbox is announced" "$OUT/run.err" "sandbox: sbx_"
contains "run created hint" "remove hint is printed" "$OUT/run.err" "remove with: sandboxctl kill sbx_"
contains "run execution line" "execution id is printed" "$OUT/run.err" "execution: exe_"
contains "run status line" "final status line" "$OUT/run.err" "completed (exited), exit 0"
sandbox_id=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/run.err" | head -1)

note "run: exit codes"
for pair in "1:1" "42:42" "125:125" "126:1" "127:1"; do
  want=${pair##*:}
  given=${pair%%:*}
  "$BIN/sandboxctl" run -- sh -c "exit $given" >/dev/null 2>"$OUT/exit-$given.err"
  code=$?
  check "run exit $given" "command exit code mapping" "$code" "$want"
done

note "run: nonzero status line"
contains "run nonzero status" "nonzero exit is reported" "$OUT/exit-42.err" "exit 42"

# ---------------------------------------------------------------- pause/resume

note "pause and resume"
"$BIN/sandboxctl" pause "$sandbox_id" >"$OUT/pause.txt" 2>&1
check "pause exit" "pause succeeds" "$?" "0"
contains "pause state" "sandbox is paused" "$OUT/pause.txt" "paused"
"$BIN/sandboxctl" resume "$sandbox_id" >"$OUT/resume.txt" 2>&1
check "resume exit" "resume succeeds" "$?" "0"
contains "resume state" "sandbox is running" "$OUT/resume.txt" "running"

# ---------------------------------------------------------------- logs/inspect/list

note "logs, inspect, list"
"$BIN/sandboxctl" run --sandbox "$sandbox_id" -- echo second >"$OUT/second.out" 2>"$OUT/second.err"
check "run existing" "run on an existing sandbox" "$?" "0"
contains "run existing line" "existing sandbox line has no created hint" "$OUT/second.err" "sandbox: $sandbox_id"
lacks "run existing no hint" "no remove hint for an existing sandbox" "$OUT/second.err" "(created"

exec_id=$(sed -n 's/^execution: \(exe_[a-z0-9]*\).*/\1/p' "$OUT/second.err" | head -1)
"$BIN/sandboxctl" logs "$sandbox_id" "$exec_id" >"$OUT/logs.out" 2>"$OUT/logs.err"
check "logs exit" "logs succeeds" "$?" "0"
check "logs stdout" "logs prints retained stdout" "$(cat "$OUT/logs.out")" "second"

"$BIN/sandboxctl" inspect "$sandbox_id" >"$OUT/inspect.txt" 2>&1
check "inspect exit" "inspect succeeds" "$?" "0"
contains "inspect id" "inspect prints the id" "$OUT/inspect.txt" "id: $sandbox_id"

"$BIN/sandboxctl" list >"$OUT/list.txt" 2>&1
check "list exit" "list succeeds" "$?" "0"
contains "list line" "list prints ID STATE IMAGE CREATED" "$OUT/list.txt" "$sandbox_id running"

# ---------------------------------------------------------------- attach

note "attach"
"$BIN/sandboxctl" attach "$sandbox_id" "$exec_id" >"$OUT/attach.out" 2>"$OUT/attach.err"
check "attach exit" "attach to a finished execution" "$?" "0"
check "attach stdout" "attach replays retained output" "$(cat "$OUT/attach.out")" "second"

# ---------------------------------------------------------------- Ctrl+C

note "ctrl+c"
# SIGINT is what Ctrl+C delivers; the CLI traps it and cancels the execution
# through the SDK. The CLI is the only local process here, so signalling its
# PID is equivalent to the terminal signalling the foreground group.
"$BIN/sandboxctl" run -- sleep 60 >"$OUT/cancel.out" 2>"$OUT/cancel.err" &
cli_pid=$!
sleep 2
kill -INT "$cli_pid"
wait "$cli_pid"
code=$?
check "ctrl+c exit" "Ctrl+C returns 130" "$code" "130"
contains "ctrl+c status" "final status line is printed after Ctrl+C" "$OUT/cancel.err" "(canceled)"

# ---------------------------------------------------------------- OOM

note "oom"
"$BIN/sandboxctl" run --image python:3.12-alpine --memory 64 -- \
  python3 -c 'x = bytearray(256*1024*1024)' >"$OUT/oom.out" 2>"$OUT/oom.err"
code=$?
check "oom exit" "OOM exits 1" "$code" "1"
contains "oom status" "OOM status line" "$OUT/oom.err" "(oom)"
contains "oom hint" "OOM hint" "$OUT/oom.err" "hint: increase --memory"

# ---------------------------------------------------------------- timeout

note "timeout"
"$BIN/sandboxctl" run --timeout 5 -- sleep 30 >"$OUT/timeout.out" 2>"$OUT/timeout.err"
code=$?
check "timeout exit" "timeout exits 1" "$code" "1"
contains "timeout reason" "timeout status line" "$OUT/timeout.err" "(timeout)"
contains "timeout hint" "timeout hint" "$OUT/timeout.err" "hint: increase --timeout"

# ---------------------------------------------------------------- 404 and busy hints

note "404 hint"
"$BIN/sandboxctl" inspect sbx_does_not_exist >"$OUT/notfound.out" 2>"$OUT/notfound.err"
check "404 exit" "a missing sandbox exits 1" "$?" "1"
contains "404 hint" "404 hint" "$OUT/notfound.err" "hint: check the sandbox and execution IDs"

note "busy hint"
# One long execution holds the sandbox; a second start through the SDK's own
# path must report busy. `run --sandbox` starts an execution, so it is the
# honest way to produce a busy from the CLI.
"$BIN/sandboxctl" run -- sleep 20 >"$OUT/busy-holder.out" 2>"$OUT/busy-holder.err" &
holder_pid=$!
sleep 2
holder_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/busy-holder.err" | head -1)
"$BIN/sandboxctl" run --sandbox "$holder_sandbox" -- true >"$OUT/busy.out" 2>"$OUT/busy.err"
check "busy exit" "a second execution exits 1" "$?" "1"
contains "busy hint" "busy hint" "$OUT/busy.err" "hint: wait for the running execution or cancel it"
kill -INT "$holder_pid" 2>/dev/null
wait "$holder_pid" 2>/dev/null

note "evicted hint"
"$BIN/sandboxctl" run -- sleep 30 >"$OUT/evict.out" 2>"$OUT/evict.err" &
evict_pid=$!
# Wait for the CLI to report its sandbox and execution rather than guessing a
# fixed delay: the container must exist before it can be killed.
for _ in $(seq 1 100); do
  grep -q '^execution: exe_' "$OUT/evict.err" 2>/dev/null && break
  sleep 0.1
done
evict_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/evict.err" | head -1)
# Select the exact container by the name the server gives it. A bare
# `docker ps | head -1` would pick any running sandbox, including ones earlier
# checks left behind, and kill the wrong one.
evict_container=$(docker ps -q --filter "name=^tiny-sandbox-${evict_sandbox}$" | head -1)
if [ -n "$evict_container" ]; then
  docker kill "$evict_container" >/dev/null 2>&1
  wait "$evict_pid"
  code=$?
  check "evicted exit" "an externally killed container exits 1" "$code" "1"
  contains "evicted status" "evicted status line" "$OUT/evict.err" "(evicted)"
  contains "evicted hint" "evicted hint" "$OUT/evict.err" "hint: the container was stopped outside sandboxd"
else
  fail "evicted setup" "container tiny-sandbox-$evict_sandbox was not found" "none" "one"
  kill "$evict_pid" 2>/dev/null
fi

# ---------------------------------------------------------------- auth

note "auth"
kill "$server_pid" 2>/dev/null
wait "$server_pid" 2>/dev/null
SANDBOX_API_KEY=e2e-secret "$BIN/sandboxd" -addr "127.0.0.1:$PORT" >"$SERVER_LOG.auth" 2>&1 &
server_pid=$!
for _ in $(seq 1 50); do
  if curl -fsS "$SANDBOX_DOMAIN/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
"$BIN/sandboxctl" list >"$OUT/auth.out" 2>"$OUT/auth.err"
check "auth exit" "missing key exits 1" "$?" "1"
contains "auth hint" "auth hint" "$OUT/auth.err" "hint: set a valid SANDBOX_API_KEY or --api-key"
SANDBOX_API_KEY=e2e-secret "$BIN/sandboxctl" list >"$OUT/auth-ok.out" 2>&1
check "auth ok" "a valid key works" "$?" "0"

# ---------------------------------------------------------------- leak check

note "cleanup"
kill "$server_pid" 2>/dev/null
wait "$server_pid" 2>/dev/null
server_pid=""
docker ps -a --filter "label=tiny-sandbox.owner=$OWNER" --format '{{.ID}} {{.Names}}' >"$OUT/containers.txt"
check "containers" "no sandbox containers remain" "$(wc -l <"$OUT/containers.txt" | tr -d ' ')" "0"

# ---------------------------------------------------------------- summary

echo | tee -a "$RESULTS"
if [ "$failures" -eq 0 ]; then
  echo "PASS: all end-to-end checks passed" | tee -a "$RESULTS"
  exit 0
fi
echo "FAIL: $failures check(s) failed" | tee -a "$RESULTS"
exit 1
