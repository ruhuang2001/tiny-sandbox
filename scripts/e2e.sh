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

# An execution deadline ends the command but keeps the sandbox, so the same
# number of seconds has to be told apart from a lifetime deadline by the state
# of the sandbox afterwards, not by the exit status.
"$BIN/sandboxctl" run --timeout 0 --exec-timeout 3 -- sleep 30 >"$OUT/exec-timeout.out" 2>"$OUT/exec-timeout.err"
code=$?
check "exec-timeout exit" "an execution timeout exits 1" "$code" "1"
contains "exec-timeout reason" "execution timeout status line" "$OUT/exec-timeout.err" "(timeout)"
exec_timeout_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/exec-timeout.err" | head -1)
"$BIN/sandboxctl" inspect "$exec_timeout_sandbox" >"$OUT/exec-timeout-inspect.txt" 2>&1
check "exec-timeout survives" "the sandbox is still running after the command timed out" "$?" "0"
contains "exec-timeout state" "the sandbox was not killed" "$OUT/exec-timeout-inspect.txt" "state: running"
"$BIN/sandboxctl" kill "$exec_timeout_sandbox" >/dev/null 2>&1

# ---------------------------------------------------------------- hardening

note "hardening"
# These limits are not in the HTTP contract: the backend fixes them. The check
# therefore reads the container's own cgroup, which is the only evidence that
# the limit reached the runtime and not just the request struct.
#
# The pids controller lives at a different path on cgroup v1 hosts, and a
# missing file must not look like a successful probe: `cat` is the last command
# that could fail, so a missing limit has to be reported as such.
"$BIN/sandboxctl" run --image python:3.12-alpine -- \
  sh -c 'if [ -r /sys/fs/cgroup/pids.max ]; then cat /sys/fs/cgroup/pids.max;
         elif [ -r /sys/fs/cgroup/pids/pids.max ]; then cat /sys/fs/cgroup/pids/pids.max;
         else echo no-pids-controller; fi;
         grep -E "^NoNewPrivs:" /proc/self/status' \
  >"$OUT/hardening.out" 2>"$OUT/hardening.err"
check "hardening exit" "the probe command succeeds" "$?" "0"
contains "hardening pids" "pids limit reaches the cgroup" "$OUT/hardening.out" "1024"
contains "hardening nonewprivs" "no-new-privileges is set" "$OUT/hardening.out" "NoNewPrivs:	1"

# A caller-set pids limit must win over the default, in the cgroup and not
# only in the response body. The request is the only place the value comes
# from, so a mismatch here means the spec never reached Docker.
"$BIN/sandboxctl" run --image python:3.12-alpine --pids-limit 128 -- \
  cat /sys/fs/cgroup/pids.max >"$OUT/pids-request.out" 2>"$OUT/pids-request.err"
check "pids-request exit" "the probe command succeeds" "$?" "0"
check "pids-request applied" "a requested pids limit reaches the cgroup" \
  "$(cat "$OUT/pids-request.out")" "128"

# Network access is the default, so it is the absence that has to be proven.
# The evidence is the interface list, not a reachability probe: a connect to
# the internet would make this suite depend on CI egress, and what the mode
# controls is whether a non-loopback interface exists at all.
"$BIN/sandboxctl" run --image python:3.12-alpine --network none -- \
  sh -c 'awk -F: "NR>2 {gsub(/ /,\"\",\$1); print \$1}" /proc/net/dev' \
  >"$OUT/network-none.out" 2>"$OUT/network-none.err"
check "network-none exit" "the network probe succeeds" "$?" "0"
check "network-none blocked" "--network none leaves only loopback" \
  "$(tr '\n' ' ' <"$OUT/network-none.out" | tr -s ' ' | sed 's/ $//')" "lo"

# The default must be the opposite, or the isolation check above proves nothing
# about the mode it was asked for.
"$BIN/sandboxctl" run --image python:3.12-alpine -- \
  sh -c 'awk -F: "NR>2 {gsub(/ /,\"\",\$1); print \$1}" /proc/net/dev' \
  >"$OUT/network-bridge.out" 2>"$OUT/network-bridge.err"
check "network-bridge exit" "the network probe succeeds" "$?" "0"
if grep -qv '^lo$' "$OUT/network-bridge.out"; then
  pass "network-bridge open"
else
  fail "network-bridge open" "the default network has no non-loopback interface" \
    "$(tr '\n' ' ' <"$OUT/network-bridge.out")" "eth0 or similar"
fi

# The fork bomb is what the pids limit exists for. Memory alone does not stop
# it: a sleeping child costs a task slot, not a page.
#
# It runs only when the effective limit was just verified. On a host where
# Docker ignored the limit (rootless Docker is documented to do this when its
# cgroup requirements are unmet) the probe would otherwise fork thousands of
# children on the host before reporting the failure it already knows about.
if grep -qx '1024' "$OUT/hardening.out"; then
  "$BIN/sandboxctl" run --image python:3.12-alpine -- python3 -c '
import os, time
kids = []
try:
    for _ in range(5000):
        pid = os.fork()
        if pid == 0:
            time.sleep(3)
            os._exit(0)
        kids.append(pid)
except OSError:
    pass
print(len(kids))
' >"$OUT/fork.out" 2>"$OUT/fork.err"
  code=$?
  forks=$(tr -dc '0-9' <"$OUT/fork.out")
  check "fork exit" "a fork bomb does not kill the command" "$code" "0"
  if [ -n "$forks" ] && [ "$forks" -lt 2000 ]; then
    pass "fork bounded"
  else
    fail "fork bounded" "fork bomb was not bounded by the pids limit" "${forks:-none}" "<2000"
  fi

  # The fork probe leaves up to a thousand children behind. With a reaping PID
  # 1 they are collected, so the slots come back; without one they stay as
  # zombies and this next command cannot fork at all. Asserting a fresh
  # multi-process command after the bomb is what tests the reaper.
  "$BIN/sandboxctl" run --image python:3.12-alpine -- \
    sh -c 'for i in $(seq 1 20); do (true) & done; wait; echo slots-ok' \
    >"$OUT/after-fork.out" 2>"$OUT/after-fork.err"
  check "after-fork exit" "the sandbox still forks after the fork bomb" "$?" "0"
  check "after-fork reaped" "orphaned children were reaped, slots released" \
    "$(cat "$OUT/after-fork.out")" "slots-ok"
else
  fail "fork setup" "the pids limit was not observed, the destructive probe is skipped" \
    "$(head -1 "$OUT/hardening.out")" "1024"
fi

# Raw sockets need CAP_NET_RAW. Without it Python raises PermissionError; if
# the capability were present the probe would print raw-ok and pass silently.
"$BIN/sandboxctl" run --image python:3.12-alpine -- python3 -c '
import socket
try:
    socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)
    print("raw-ok")
except PermissionError:
    print("raw-denied")
' >"$OUT/capraw.out" 2>"$OUT/capraw.err"
check "cap_net_raw exit" "the capability probe succeeds" "$?" "0"
check "cap_net_raw dropped" "CAP_NET_RAW is dropped" "$(cat "$OUT/capraw.out")" "raw-denied"

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
