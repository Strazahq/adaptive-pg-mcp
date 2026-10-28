# State

One keyed line per item, read by grep. The grammar is in CLAUDE.md and
tools/stategate.sh enforces it. Close an item by deleting its line.

## P owner actions

- P-003 waiting-owner core | Confirm the names the M1 lane chose: the HTTP endpoint /mcp and the token command's output lines token: and token_sha256:. | internal/core/http.go | [doc 10-28]
- P-004 waiting-owner core | Decide whether a browser-based client may call over HTTP, since the server refuses every request with an Origin header and allowing one needs a config key the owner names. | internal/core/http.go | [live 10-28]
- P-005 waiting-owner core | Decide how HTTP gets TLS, since the server has none and a listen address beyond loopback sends bearer tokens in cleartext: TLS keys in server.yaml, or a documented TLS proxy in front. | internal/core/http.go | [doc 10-28]

## Q queue

- Q-002 open core | M1 has the config, tokens, the token check and both transports, and still needs internal/pg with a pool per login, the start checks, the id check, the check command, the built-in tools, free SQL, cancellation and the tool lint on a served tools/list. | internal/core/server.go | [live 10-28]
- Q-003 open catalog | M2 builds the .sql reader, the index and the check command, and fills each parameter's Postgres type from the prepare so that core.Tool builds the input schema from it. | docs/DESIGN.md section 3 | [doc 10-28]
- Q-005 open straza | M4 fires the server behind Straza once M1 to M3 land, with the database scope, approval and the id check through the gateway. | docs/DESIGN.md section 8 | [doc 10-28]

## B suspected bugs

- B-001 open gates | tools/textgate.sh takes the sync.Mutex method that releases a lock for a filler word from its list, so Go code that releases a mutex cannot pass the commit gate. | tools/textgate.sh | [live 10-28]

## C standing constraints

- C-001 standing spec | The server targets the newest MCP revision and the newest go-sdk release, and the push gate refuses when either falls behind. | tools/specgate.sh | [live 10-28]
- C-002 standing tests | No CI runs yet, so the push gate runs every database test, built with -tags pg, against Postgres 14 to 18 in local Docker through tools/pg.sh. | .githooks/pre-push | [live 10-28]
- C-003 standing sql | pgx sends an Exec or Query without arguments on the simple query protocol, which can carry several statements, so server code sends SQL through pgconn ExecParams or a prepared statement, and tools/gogate.sh does not catch this yet. | internal/pgtest/pgtest.go | [doc 10-28]

## E environment

## S siblings
