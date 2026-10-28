#!/bin/sh
# Spec gate. Keeps the server on the newest MCP specification revision and the
# newest go-sdk release. It runs at push and with make spec. It needs the
# network, and an unreachable source refuses, because a skipped check is a
# check that never happened.
#   1. docs/MCP-REVISION names the newest dated revision in the specification
#      repository, unless the newest go-sdk does not serve it yet.
#   2. go.mod requires the newest go-sdk release.
#   3. The go-sdk in go.mod serves the revision docs/MCP-REVISION names.
set -u
root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root" || exit 1
sdk=github.com/modelcontextprotocol/go-sdk
say() { printf '%s\n' "$*" >&2; }
newest() { sort | tail -1; }

target=$(tr -d ' \n' < docs/MCP-REVISION 2>/dev/null)
[ -z "$target" ] && { say "specgate: docs/MCP-REVISION is missing or empty. Write the revision the server targets, such as 2026-07-28."; exit 1; }

spec=$(curl -fsS https://api.github.com/repos/modelcontextprotocol/modelcontextprotocol/contents/schema 2>/dev/null \
  | grep -o '"name": *"[0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\}"' | grep -o '[0-9-]\{10\}' | newest)
[ -z "$spec" ] && { say "specgate: could not read the revisions from the MCP specification repository, so the push is refused. Check the network and push again."; exit 1; }

if [ ! -f go.mod ]; then
  if [ "$target" != "$spec" ]; then
    say "specgate: MCP revision $spec is out and docs/MCP-REVISION names $target. Read https://modelcontextprotocol.io/specification/$spec/changelog, then write $spec into docs/MCP-REVISION."
    exit 1
  fi
  exit 0
fi

have=$(go list -m -f '{{.Version}}' "$sdk" 2>/dev/null)
[ -z "$have" ] && { say "specgate: go.mod does not require $sdk. The server is built on the official Go SDK; add it."; exit 1; }
latest=$(GOFLAGS=-mod=mod go list -m -f '{{.Version}}' "$sdk@latest" 2>/dev/null)
[ -z "$latest" ] && { say "specgate: could not read the newest $sdk release, so the push is refused. Check the network and push again."; exit 1; }
if [ "$have" != "$latest" ]; then
  say "specgate: go.mod requires $sdk $have and $latest is out. Run go get $sdk@$latest and go mod tidy, run the tests, then push."
  exit 1
fi

go mod download "$sdk" 2>/dev/null
dir=$(go list -m -f '{{.Dir}}' "$sdk" 2>/dev/null)
served=$(grep -ho '"[0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\}"' "$dir"/mcp/shared.go 2>/dev/null | tr -d '"' | newest)
[ -z "$served" ] && { say "specgate: could not read the protocol versions from $sdk mcp/shared.go. The SDK moved them; update this check."; exit 1; }

if [ "$target" != "$served" ] && [ "$(printf '%s\n%s\n' "$target" "$served" | newest)" = "$target" ]; then
  say "specgate: docs/MCP-REVISION names $target, but $sdk $have serves revisions up to $served. Name a revision the SDK serves."
  exit 1
fi
want=$(printf '%s\n%s\n' "$spec" "$served" | sort | head -1)
if [ "$target" != "$want" ]; then
  say "specgate: MCP revision $want is out and served by $sdk $have, and docs/MCP-REVISION names $target. Read https://modelcontextprotocol.io/specification/$want/changelog, update the server and its tests, then write $want into docs/MCP-REVISION."
  exit 1
fi
if [ "$spec" != "$served" ]; then
  say "specgate: notice: MCP revision $spec is out, and $sdk $have serves up to $served. The gate passes; a STATE line tracks the revision until the SDK serves it."
fi
exit 0
