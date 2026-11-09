#!/usr/bin/env bash
# Real Docker + sandboxd E2E. PYTHON selects the interpreter. SDK_INSTALLED=1
# tests a previously installed wheel (CI); default builds into an isolated venv.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/artifacts/python-e2e}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
PYTHON=${PYTHON:-python3}
command -v "$PYTHON" >/dev/null
docker info >"$OUT/docker-info.txt" 2>&1
(cd "$ROOT" && go build -o "$OUT/sandboxd" ./cmd/sandboxd) >"$OUT/build.log" 2>&1
if [ "${SDK_INSTALLED:-0}" != "1" ]; then
  "$PYTHON" -m venv "$OUT/venv"
  mkdir -p "$OUT/wheels"
  rm -f "$OUT"/wheels/tiny_sandbox-*.whl
  "$OUT/venv/bin/python" -m pip wheel --no-deps --wheel-dir "$OUT/wheels" "$ROOT/sandbox-python" >"$OUT/install.log" 2>&1
  "$OUT/venv/bin/python" -m pip install --force-reinstall --no-deps "$OUT"/wheels/tiny_sandbox-*.whl >>"$OUT/install.log" 2>&1
  PYTHON="$OUT/venv/bin/python"
fi
"$PYTHON" -u "$ROOT/sandbox-python/e2e/run.py" --server "$OUT/sandboxd" --artifacts "$OUT"
