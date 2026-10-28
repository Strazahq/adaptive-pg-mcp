#!/bin/sh
# Text gate. Deterministic checks over every tracked text file so that nothing
# sloppy or internal lands. Each finding prints file:line and what to do.
# This file is the single source of the word lists; CLAUDE.md points here.
set -u
root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root" || exit 1

files=$( { git ls-files; git diff --cached --name-only --diff-filter=A; } 2>/dev/null | sort -u \
  | grep -E '\.(md|go|yaml|yml|sh|txt|toml|json)$|(^|/)Makefile$' \
  | grep -v -E '^tools/textgate\.sh$' \
  | while read -r f; do [ -f "$f" ] && printf '%s\n' "$f"; done)
[ -z "$files" ] && exit 0

fail=0
report() { printf '%s\n' "$*" >&2; fail=1; }

# 1. Em dashes, anywhere.
hits=$(printf '%s\n' "$files" | xargs grep -n '—' 2>/dev/null)
[ -n "$hits" ] && report "textgate: em dash. Use a comma, a colon or a period.
$hits"

# 2. Filler and marketing words that mark generated text. Case-insensitive.
tells='\b(delv(e|es|ing)|leverag(e|es|ing)|seamless(ly)?|robust(ly|ness)?|cutting-edge|game[- ]changer|unlock(s|ing)?|empower(s|ing)?|streamlin(e|es|ing)|tapestry|elevat(e|es|ing)|supercharge|effortless(ly)?|state-of-the-art|best-in-class|world-class|revolutioni[sz]e|paradigm|synergy|holistic)\b'
phrases="it'?s worth noting|it is worth noting|in today'?s|let'?s dive|dive into|harness the power|the landscape of|navigate the|great question|as an ai|in conclusion|comprehensive suite|a testament to"
hits=$(printf '%s\n' "$files" | xargs grep -n -i -E "$tells|$phrases" 2>/dev/null)
# In Go code the mutex methods Unlock and RUnlock are names, not filler, so a
# Go line counts only when it still matches with those calls taken out.
hits=$(printf '%s\n' "$hits" | while IFS= read -r h; do
  [ -z "$h" ] && continue
  case "${h%%:*}" in
    *.go) printf '%s\n' "$h" | sed -E 's/\bR?Unlock\(//g' | grep -q -i -E "$tells|$phrases" || continue ;;
  esac
  printf '%s\n' "$h"
done)
[ -n "$hits" ] && report "textgate: filler or marketing words. Say the plain thing instead.
$hits"

# 3. Bold-label bullets as a form. A bullet is a sentence.
hits=$(printf '%s\n' "$files" | grep -E '\.md$' | xargs grep -n -E '^[[:space:]]*[-*] \*\*[^*]+:\*\*' 2>/dev/null)
[ -n "$hits" ] && report "textgate: bold-label bullet. Write the bullet as a sentence.
$hits"

# 4. Internal names that must never reach a public repository. The list is
# not in the tree: it lives in tools/internal-names.local, ignored by git, one
# extended regular expression per line. Without the file the check is skipped
# with a notice, so a fresh clone can still commit.
list="tools/internal-names.local"
if [ -f "$list" ]; then
  hits=$(printf '%s\n' "$files" | xargs grep -n -i -E -f "$list" 2>/dev/null)
  [ -n "$hits" ] && report "textgate: an internal name, address or sibling reference. This repository is public; replace it with a documentation example.
$hits"
else
  printf '%s\n' "textgate: $list is absent, so the internal-name check did not run." >&2
fi

# 5. Real-looking private keys or tokens in text files.
hits=$(printf '%s\n' "$files" | xargs grep -n -E 'BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY|ghp_[A-Za-z0-9]{20,}|xox[bp]-[A-Za-z0-9-]{10,}' 2>/dev/null)
[ -n "$hits" ] && report "textgate: something that looks like a private key or a token. Secrets never live in the tree.
$hits"

if [ "$fail" -ne 0 ]; then
  printf '%s\n' "textgate: refused. The list above says where and what to change." >&2
  exit 1
fi
exit 0
