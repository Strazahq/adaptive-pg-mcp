# adaptive-pg-mcp session rules

An MCP server for PostgreSQL that is safe by construction. Every call names
its database, the agent gets typed tools, and an organization's own SQL
becomes named tools with typed parameters. Free SQL is there too, under the
grants of the login it runs on. It is a plain MCP server that any client can
use. Behind the Straza gateway it gains identity, per-database roles, approval
holds and an audit record per call, with no change to the server. One static
Go binary, fail closed everywhere. Single owner; Claude sessions do most of
the typing; the git hooks are the gate, not the model.

The repository is private for now and will be public later, so everything in
it is written for a stranger who runs databases for a living.

## Session start (read these, nothing more, before code)
1. docs/STATE.md by grep: the P lines waiting on the owner, the Q lines of the
   area in play, the C lines.
2. docs/PLAN.md section 2 (decisions) and section 3 (invariants), plus the
   milestone in play.
3. docs/DESIGN.md for the section the unit touches. Not the whole document.
4. `make spec`. When it refuses, the unit is the revision update first.
5. Open with the STATE task menu; the owner picks the unit. One unit per session.

## Constraint checks (owner rule: the newest MCP revision, security, MCP best practices)

Gated means a hook refuses. Tested means a test in the tree fails. Session
means the session does it and says so in the unit report.

### The newest MCP revision
- Gated at push: tools/specgate.sh refuses when go.mod is behind the newest
  go-sdk release, when a newer dated revision is out and the go-sdk serves
  it, or when docs/MCP-REVISION names a revision the SDK does not serve.
- Session: when the revision changes, read its changelog and its security
  best practices page before changing code, then record in the CHANGELOG line
  what the revision changed for this server.
- Tested: a test fails when the server does not serve the revision in
  docs/MCP-REVISION, and the authorization test runs on every revision the
  SDK serves, because a check that only one protocol path runs is a bypass.

### Security measures
- Gated at commit by tools/gogate.sh: no simple query protocol, no SQL built
  with fmt, no os/exec in server code, no InsecureSkipVerify, no math/rand in
  server code, no stdout outside cmd/, no stubs.
- Gated at push: gitleaks, govulncheck and osv-scanner. A missing scanner
  refuses the push.
- Tested, written first: the token check, the id check, the start checks and
  every refusal in docs/DESIGN.md section 7, each with its sentence quoted
  word for word.
- Tested: no connection string, password or token appears in a log line or an
  error. A test runs with a known password and greps all output for it.
- Session: a new dependency gets a CHANGELOG line with the why, its license
  and a vulnerability check, in the same commit.
- Session: a change to the token check, the SQL path, the id check or the
  start checks gets an independent Opus review lane before the commit.

### MCP best practices
- Tested by a tool lint test that walks `tools/list` and fails on any tool
  that breaks one of these. The name is 1 to 128 of `A-Z a-z 0-9 _ - .`,
  snake_case and unique. The description says what the tool does, what it
  returns and its limits. The input schema is an object with every property
  described, `required` listed and `additionalProperties: false`. The
  annotations `readOnlyHint`, `destructiveHint`, `idempotentHint` and
  `openWorldHint` are set on every tool. An output schema is declared, and
  every answer carries `structuredContent` that matches it plus a short text
  for people. The Straza marks follow docs/DESIGN.md section 4.
- Tested: a bad argument or a refusal is a tool result with `isError: true`
  and a plain sentence. A protocol error is only for a protocol fault.
- Tested: listings and large results page with an opaque cursor, and a
  cancelled call cancels its query in Postgres.
- Tested: Streamable HTTP checks `Origin`, listens on localhost unless the
  config names another address, and never treats a session id as proof of
  who calls.
- Never: the server forwards a caller's token to Postgres or anywhere else.
- Sources: the specification at
  `https://modelcontextprotocol.io/specification/<revision>` and its security
  best practices at
  `https://modelcontextprotocol.io/docs/<revision>/tutorials/security/security_best_practices`,
  for the revision in docs/MCP-REVISION.

## The gate (.githooks, bound with `make hooks`)
- pre-commit: gofmt, go vet and the tests when Go code exists, the text gate
  (tools/textgate.sh), the Go gate (tools/gogate.sh), the state gate
  (tools/stategate.sh) and the 600-line file cap. It reads the whole tree, so
  commit with explicit paths when another session shares the tree.
- commit-msg: a subject of at most 72 characters, imperative, saying what
  changed, and no AI attribution trailer of any kind.
- pre-push: gitleaks, govulncheck, osv-scanner and the spec gate.
- `--no-verify`, `-n`, hook path changes and force pushes are never used. A
  red gate is fixed, not bypassed. The session gate (tools/sessiongate.sh,
  wired in .claude/settings.json) refuses those commands before they run.
- Lanes that share the tree run every go command under one `flock` file, so
  two test runs never fight over the box.

## Writing: for a reader who was not in the session
- Full sentences, one idea per sentence, plain words. This holds for chat,
  commits, CHANGELOG, STATE, docs, code comments and error messages.
- No em dashes anywhere. Restructure with a comma, a colon or a period.
- No filler and no marketing words. The text gate holds the list in
  tools/textgate.sh.
- Bold-label bullets are not the default form. A bullet is a sentence.
- Code comments are godoc: one sentence that starts with the name. No
  session narrative, dates or owner quotes in code. History lives in
  CHANGELOG and PLAN.
- Every operator-facing message says what failed, why, and what to do next,
  in plain words. Never a bare code.
- Provenance on durable facts: [live] fired against a real system, [doc]
  derived from documents, [suspect] doubted. "Verified" is never claimed from
  documents.

## Repository rules
- Nothing internal: no private hostnames, addresses, sibling repository
  names, session codenames or organization names outside this project's own.
  The text gate refuses the known ones from tools/internal-names.local, which
  stays outside git.
- Examples use documentation addresses, `example.internal` names and made-up
  databases such as `sales_eu` and `helpdesk`.

## Definition of done
Acceptance criteria met. Tests added, table-driven, on Postgres 14 to 18 in
local Docker. The constraint checks above pass. `make check` green. One
CHANGELOG line with the why. Every STATE line the session touched
reconciled: closed, re-anchored or re-tagged. Affected docs updated in the
same session.

## Owner rules
- Show-then-change: what will change, why this design over the alternatives,
  the risk and what it can break, in chat, then stop for an explicit go.
- Names are the owner's: the tools, the config keys, the catalog keys.
  Propose options with plain context, never decide alone.
- No adjacent cleanups in a diff. Every changed line traces to the request.
  No setting, interface or layer that no current caller or test needs.
- New scope waits for the owner.

## Where things live
- docs/DESIGN.md is the design. docs/PLAN.md holds the decisions, the
  invariants and the milestones. docs/MCP-REVISION names the MCP revision the
  server targets.
- docs/STATE.md is live state, one keyed line per item, read by grep:
  `- <ID> <status> <area> | <one sentence> | <anchor> | [<tag> <MM-DD>]`,
  sections P owner actions, Q queue, B suspected bugs, C standing
  constraints, E environment, S siblings. Status: open, blocked,
  waiting-owner, standing. Tag: live, doc, suspect. Close an item by deleting
  its line. tools/stategate.sh enforces the grammar.
- CHANGELOG.md is the change record.
