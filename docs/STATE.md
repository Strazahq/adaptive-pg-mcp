# State

One keyed line per item, read by grep. The grammar is in CLAUDE.md and
tools/stategate.sh enforces it. Close an item by deleting its line.

## P owner actions


## Q queue


## B suspected bugs


## C standing constraints

- C-001 standing spec | The server targets the newest MCP revision and the newest go-sdk release, and the push gate refuses when either falls behind. | tools/specgate.sh | [live 10-28]
- C-002 standing tests | GitHub Actions runs vet, the unit tests and govulncheck before it builds the image, but no database test, so the push gate runs every test built with -tags pg against Postgres 14 to 18 in local Docker through tools/pg.sh. | .githooks/pre-push | [doc 10-29]
- C-003 standing sql | pgx sends an Exec without arguments on the simple protocol, which can carry several statements, so the server never calls Exec: every statement goes through pg.Tx.Query or pg.Tx.Describe, and tools/gogate.sh does not catch a direct Exec yet. | internal/pg/pg.go | [live 10-28]
- C-004 standing sql | Every write cap counts the larger of the command tag and the rows changed in user tables across the statement, so cascades, triggers, functions and WITH queries count, and a write with track_counts off is refused. | internal/core/freesql.go | [live 10-28]
- C-005 standing sql | Free SQL and catalog tools close their connection after each call, because a statement that is not the server's own can change the session that the pool would hand to the next call, and a test shows the leak without it for each. | internal/core/entry.go | [live 10-28]
- C-006 standing sql | execute_sql and execute_ddl split by the command tag and the rows changed in the system catalogs, never by the SQL text, so a structure change inside a DO block, procedure or function cannot pass through execute_sql. | internal/core/freesql.go | [live 10-28]

## E environment

## S siblings
