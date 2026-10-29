#!/bin/sh
# Go gate. Deterministic checks over tracked Go files for the shortcuts that
# break the server's boundaries. Each finding prints file:line and what to do.
# The invariants behind the checks are docs/PLAN.md section 3.
set -u
root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root" || exit 1

files=$( { git ls-files '*.go'; git diff --cached --name-only --diff-filter=A -- '*.go'; } 2>/dev/null | sort -u \
  | while read -r f; do [ -f "$f" ] && printf '%s\n' "$f"; done)
[ -z "$files" ] && exit 0
src=$(printf '%s\n' "$files" | grep -v '_test\.go$')

fail=0
report() { printf '%s\n' "$*" >&2; fail=1; }
grep_src() { [ -n "$src" ] && printf '%s\n' "$src" | xargs grep -n -E "$@" 2>/dev/null; }

# 1. The simple query protocol runs several statements in one call. Invariant 2.
hits=$(grep_src 'QueryExecModeSimpleProtocol|PreferSimpleProtocol')
[ -n "$hits" ] && report "gogate: the simple query protocol. Every call is one prepared statement; keep pgx on the extended protocol.
$hits"

# 2. SQL text built with a format verb. Invariant 3.
hits=$(grep_src -i 'fmt\.(Sprintf|Fprintf)\([^"]*"[^"]*\b(select|insert|update|delete|where|from|into|set role|truncate)\b')
[ -n "$hits" ] && report "gogate: SQL built with fmt. Values travel as bind parameters, and identifiers go through the one quoting helper.
$hits"

# 3. The server never starts a program. Invariant 6.
hits=$(grep_src '"os/exec"')
[ -n "$hits" ] && report "gogate: os/exec in server code. The server never starts a program; a test may.
$hits"

# 4. Certificate checks switched off. Invariant 7.
hits=$(grep_src 'InsecureSkipVerify:[[:space:]]*true')
[ -n "$hits" ] && report "gogate: InsecureSkipVerify. A connection that cannot verify its peer is refused; fix the trust store instead.
$hits"

# 5. Tokens from a predictable source. Invariant 5.
hits=$(grep_src '"math/rand(/v2)?"')
[ -n "$hits" ] && report "gogate: math/rand in server code. Tokens and anything secret come from crypto/rand.
$hits"

# 6. Writes to stdout outside cmd/. On stdio, stdout is the protocol channel.
hits=$( [ -n "$src" ] && printf '%s\n' "$src" | grep -v '^cmd/' | xargs grep -n -E 'fmt\.Print(f|ln)?\(|os\.Stdout' 2>/dev/null)
[ -n "$hits" ] && report "gogate: a write to stdout outside cmd/. Logs go to stderr, because on stdio stdout carries the protocol.
$hits"

# 7. Half-done code committed as done.
hits=$(printf '%s\n' "$files" | xargs grep -n -E '\b(TODO|FIXME|XXX|HACK)\b|panic\("(not implemented|unimplemented)' 2>/dev/null)
[ -n "$hits" ] && report "gogate: a stub or a marker for later. Finish it in this unit or record it as a STATE line and leave the code out.
$hits"

if [ "$fail" -ne 0 ]; then
  printf '%s\n' "gogate: refused. The list above says where and what to change." >&2
  exit 1
fi
exit 0
