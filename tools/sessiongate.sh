#!/bin/sh
# Session gate. A Claude Code PreToolUse hook on the Bash tool. It reads the
# tool call as JSON on stdin and refuses, with exit 2 and a reason, the
# commands CLAUDE.md says are never used: skipping the commit gate, moving or
# disabling the hooks, and force pushing. Everything else passes untouched.
set -u
cmd=$(python3 -c 'import json, sys
try:
    call = json.load(sys.stdin)
except Exception:
    sys.exit(0)
print(call.get("tool_input", {}).get("command", ""))' 2>/dev/null) || exit 0
[ -z "$cmd" ] && exit 0

refuse() { printf 'sessiongate: %s\n' "$1" >&2; exit 2; }
has() { printf '%s' "$cmd" | grep -q -E -- "$1"; }

has '--no-verify' && refuse "--no-verify skips the commit gate. Fix what the gate reports and commit again."
has 'git[[:space:]]+commit\b[^|;&]*[[:space:]]-[a-zA-Z]*n[a-zA-Z]*([[:space:]]|$)' && refuse "-n on git commit skips the commit gate. Fix what the gate reports and commit again."
has 'core\.hooksPath' && refuse "the hook path is bound once with make hooks, and a session never moves it."
has '(chmod|rm|mv|git[[:space:]]+rm)[[:space:]][^|;&]*\.githooks' && refuse "the hooks in .githooks are the gate. A session never removes, moves or disables them."
has 'git[[:space:]]+push\b[^|;&]*([[:space:]]--force(-with-lease)?\b|[[:space:]]-f\b|[[:space:]]\+[^[:space:]]+)' && refuse "a force push rewrites shared history. Push a new commit instead."
exit 0
