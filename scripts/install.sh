#!/bin/sh
set -eu

main() {
  for command in curl tar uname mktemp install; do
    command -v "$command" >/dev/null 2>&1 || { echo "Required command not found: $command" >&2; exit 1; }
  done
  case "$(uname -s)" in
    Darwin) platform=darwin ;;
    Linux) platform=linux ;;
    *) echo "Use macOS, Linux, or WSL2." >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac

  repo=https://github.com/ruhuang2001/tiny-sandbox
  release=$(curl -fsSL --retry 3 -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
  case "$release" in
    "$repo/releases/tag/v"*) tag=${release##*/} ;;
    *) echo "Could not find the latest release." >&2; exit 1 ;;
  esac
  destination=${1:-"$HOME/tiny-sandbox-quickstart"}
  work=$(mktemp -d "${TMPDIR:-/tmp}/tiny-sandbox-install.XXXXXX")
  trap 'rm -rf "$work"' 0
  trap 'exit 130' INT
  trap 'exit 143' TERM

  echo "Downloading tiny-sandbox $tag ($platform/$arch)..."
  curl -fL --retry 3 "$repo/releases/download/$tag/tiny-sandbox_${tag#v}_${platform}_${arch}.tar.gz" -o "$work/release.tar.gz"
  tar -xzf "$work/release.tar.gz" -C "$work" sandboxd sandboxctl
  mkdir -p "$destination"
  install -m 755 "$work/sandboxd" "$destination/sandboxd"
  install -m 755 "$work/sandboxctl" "$destination/sandboxctl"
  echo "Installed sandboxd and sandboxctl in $destination"
  echo "Next: https://github.com/ruhuang2001/tiny-sandbox/blob/main/docs/quickstart.md"
}

main "$@"
