#!/bin/sh
# State gate for docs/STATE.md. One keyed line per item:
#   - <ID> <status> <area> | <one sentence> | <anchor> | [<tag> <MM-DD>]
# ID is a section letter (P Q B C E S), a dash and three digits. Status is
# open, blocked, waiting-owner or standing. Tag is live, doc or suspect.
# A line is at most 400 characters. IDs are unique. The file stays under 80 KB.
set -u
file="${1:-docs/STATE.md}"
[ -f "$file" ] || { echo "stategate: $file is missing. Create it with the sections P Q B C E S." >&2; exit 1; }

size=$(wc -c < "$file" | tr -d ' ')
if [ "$size" -gt 81920 ]; then
  echo "stategate: $file is $size bytes, over 80 KB. Close items or move narrative to CHANGELOG." >&2
  exit 1
fi

awk -v file="$file" '
BEGIN { fail = 0 }
/^- / {
  line = $0
  if (length(line) > 400) {
    printf "%s:%d: item line is %d characters, over 400; one sentence, then anchor and tag\n", file, NR, length(line) > "/dev/stderr"; fail = 1
  }
  if (line !~ /^- [PQBCES]-[0-9][0-9][0-9] (open|blocked|waiting-owner|standing) [a-z0-9-]+ \| [^|]+ \| [^|]+ \| \[(live|doc|suspect) [0-9][0-9]-[0-9][0-9]\]$/) {
    printf "%s:%d: item line is off-grammar; the form is - <ID> <status> <area> | <one sentence> | <anchor> | [<tag> <MM-DD>]\n", file, NR > "/dev/stderr"; fail = 1
  }
  split(line, parts, " ")
  id = parts[2]
  if (id in seen) {
    printf "%s:%d: duplicate id %s; a new item takes max+1 in its section\n", file, NR, id > "/dev/stderr"; fail = 1
  }
  seen[id] = NR
}
END { if (fail) { print "stategate: refused; the grammar is in CLAUDE.md (Where things live)" > "/dev/stderr"; exit 1 } }
' "$file"
