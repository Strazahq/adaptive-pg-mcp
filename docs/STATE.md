# State

One keyed line per item, read by grep. The grammar is in CLAUDE.md and
tools/stategate.sh enforces it. Close an item by deleting its line.

## P owner actions

- P-003 waiting-owner core | Confirm the names the M1 lane chose: the HTTP endpoint /mcp and the token command's output lines token: and token_sha256:. | internal/core/http.go | [doc 10-28]
- P-004 waiting-owner core | Decide whether a browser-based client may call over HTTP, since the server refuses every request with an Origin header and allowing one needs a config key the owner names. | internal/core/http.go | [live 10-28]
- P-005 waiting-owner core | Decide how HTTP gets TLS, since the server has none and a listen address beyond loopback sends bearer tokens in cleartext: TLS keys in server.yaml, or a documented TLS proxy in front. | internal/core/http.go | [doc 10-28]
- P-006 waiting-owner core | Confirm the names the unit 3a lane chose: the check output prefixes refusal:, report: and check:, a list_databases page of 25 with the config name as title, and the report sentences in internal/core/start.go. | internal/core/start.go | [doc 10-28]
- P-007 waiting-owner core | Decide whether a login that can switch to a superuser role counts as a superuser for the start refusal, as unit 3a builds it, since free SQL can switch roles and decision 6 names superuser only. | internal/core/start.go | [live 10-28]
- P-008 waiting-owner core | Decide whether a write login that reaches another database than its read login refuses the start instead of being reported, since its writes then miss the database that list_databases names. | internal/core/start.go | [live 10-28]
- P-009 waiting-owner core | Decide whether a standalone caller must list list_databases in its tools in access.yaml, since unit 3a lets every caller list its own databases and the examples do not name the tool. | internal/core/listdb.go | [doc 10-28]
- P-010 waiting-owner core | Decide how the database id stays unique across physical copies, since a standby or a restored base backup has the same system identifier and OIDs, so a config that lists a primary and its copy gives one id under two values and Straza fails the listing. | internal/core/objectid.go | [live 10-28]

## Q queue

- Q-002 open core | M1 has the config, tokens, the token check, both transports, internal/pg with a pool per login, the start checks, the id check, the check command and list_databases, and still needs the data tools, free SQL, the limits, cancellation, the id check in the data tools and the tool lint on a served tools/list. | internal/core/start.go | [live 10-28]
- Q-003 open catalog | M2 builds the .sql reader, the index and the check command, and fills each parameter's Postgres type from the prepare so that core.Tool builds the input schema from it. | docs/DESIGN.md section 3 | [doc 10-28]
- Q-005 open straza | M4 fires the server behind Straza once M1 to M3 land, with the database scope, approval and the id check through the gateway. | docs/DESIGN.md section 8 | [doc 10-28]

## B suspected bugs

- B-001 open gates | tools/textgate.sh takes the sync.Mutex method that releases a lock for a filler word from its list, so Go code that releases a mutex cannot pass the commit gate. | tools/textgate.sh | [live 10-28]

## C standing constraints

- C-001 standing spec | The server targets the newest MCP revision and the newest go-sdk release, and the push gate refuses when either falls behind. | tools/specgate.sh | [live 10-28]
- C-002 standing tests | No CI runs yet, so the push gate runs every database test, built with -tags pg, against Postgres 14 to 18 in local Docker through tools/pg.sh. | .githooks/pre-push | [live 10-28]
- C-003 standing sql | pgx sends an Exec without arguments on the simple query protocol, which can carry several statements, so every statement of the server goes through pg.Tx.Query, which forces QueryExecModeDescribeExec, and tools/gogate.sh does not catch a direct Exec yet. | internal/pg/pg.go | [live 10-28]

## E environment

## S siblings
