#!/usr/bin/env sh
# Validates one commit message file, or every commit in a revision range.
#
#   check-commit-msg.sh <file>              validate a single message file
#   check-commit-msg.sh <range>             validate commits in a git range
#
# Rule: type(scope): summary
#   - type is one of feat, fix, style, docs, refactor, test, chore, build, ci
#   - scope is required, lowercase kebab-case
#   - summary starts lowercase and the subject stays under 72 characters
#   - merge commits and reverts are skipped
set -eu

types='feat|fix|style|docs|refactor|test|chore|build|ci'
pattern="^($types)\([a-z0-9][a-z0-9.-]*\)!?: [a-z][^ ]*( .*)?\$"

check_subject() {
  subject=$1
  id=$2
  case "$subject" in
    "Merge "*|"Revert "*|"fixup! "*|"squash! "*) return 0 ;;
  esac
  if [ ${#subject} -gt 72 ]; then
    printf 'commit-msg: %s: subject is %d characters, limit is 72\n' "$id" "${#subject}" >&2
    return 1
  fi
  if ! printf '%s\n' "$subject" | grep -Eq "$pattern"; then
    printf 'commit-msg: %s: bad subject: %s\n' "$id" "$subject" >&2
    printf 'commit-msg: expected "type(scope): summary", e.g. "feat(sdk): add stream reconnect"\n' >&2
    printf 'commit-msg: types: %s\n' "$(printf '%s' "$types" | tr '|' ' ')" >&2
    return 1
  fi
  return 0
}

if [ $# -ne 1 ]; then
  printf 'usage: %s <message-file|revision-range>\n' "$0" >&2
  exit 2
fi

target=$1
if [ -f "$target" ]; then
  subject=$(grep -m1 -v '^#' "$target" | sed -e 's/[[:space:]]*$//')
  [ -z "$subject" ] && exit 0
  check_subject "$subject" "$(basename "$target")"
  exit $?
fi

status=0
for sha in $(git rev-list --no-merges "$target"); do
  subject=$(git log -1 --format=%s "$sha")
  check_subject "$subject" "$(git log -1 --format=%h "$sha")" || status=1
done
exit $status
