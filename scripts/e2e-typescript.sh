#!/usr/bin/env bash
# Repeatable real-Docker E2E suite. No fake-only unit tests.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/artifacts/typescript-e2e}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
PORT=${E2E_TS_PORT:-$(node -e 'const s=require("net").createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')}
OWNER="127.0.0.1:$PORT"
SERVER_PID=''
cleanup() {
  local status=$?
  trap - EXIT
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER" >"$OUT/containers-before-cleanup.txt"
  while IFS= read -r id; do
    [ -z "$id" ] || docker rm -f "$id" >>"$OUT/cleanup.log" 2>&1 || true
  done <"$OUT/containers-before-cleanup.txt"
  docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER" >"$OUT/containers-after-cleanup.txt"
  if [ -s "$OUT/containers-after-cleanup.txt" ]; then status=1; fi
  printf '{"owner":"%s","remainingContainers":%s,"exitCode":%s}\n' "$OWNER" "$(wc -l <"$OUT/containers-after-cleanup.txt" | tr -d ' ')" "$status" >"$OUT/cleanup.json"
  exit "$status"
}
trap cleanup EXIT
docker info >"$OUT/docker-info.txt" 2>&1
DOCKER_ENDPOINT=${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}
if [[ "$DOCKER_ENDPOINT" != unix://* ]]; then
  echo 'sandboxd E2E requires a unix Docker endpoint' >&2
  exit 1
fi
export DOCKER_HOST="$DOCKER_ENDPOINT"
(cd "$ROOT/sandbox-ts" && npm ci && npm run build && npm run check) >"$OUT/sdk-build.log" 2>&1
(cd "$ROOT" && go build -o "$OUT/sandboxd" ./cmd/sandboxd) >"$OUT/server-build.log" 2>&1
API_KEY="typescript-e2e-$(node -e 'console.log(require("crypto").randomBytes(16).toString("hex"))')"
SANDBOX_API_KEY="$API_KEY" "$OUT/sandboxd" -addr "$OWNER" >"$OUT/sandboxd.log" 2>&1 &
SERVER_PID=$!
READY=0
for _ in $(seq 1 100); do
  if curl -fsS "http://$OWNER/healthz" >"$OUT/healthz.json" 2>/dev/null; then READY=1; break; fi
  kill -0 "$SERVER_PID" 2>/dev/null || break
  sleep 0.1
done
if [ "$READY" -ne 1 ]; then cat "$OUT/sandboxd.log" >&2; exit 1; fi
SANDBOX_DOMAIN="http://$OWNER" SANDBOX_API_KEY="$API_KEY" E2E_ARTIFACT_DIR="$OUT" \
  node "$ROOT/sandbox-ts/e2e/run.mjs" 2>&1 | tee "$OUT/runner.log"
SANDBOX_DOMAIN="http://$OWNER" SANDBOX_API_KEY="$API_KEY" E2E_ARTIFACT_DIR="$OUT" \
  node "$ROOT/sandbox-ts/e2e/package-smoke.mjs" 2>&1 | tee "$OUT/package-smoke.log"
