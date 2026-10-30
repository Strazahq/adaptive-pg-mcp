# State

One keyed line per item, read by grep. The grammar is in CLAUDE.md and
tools/stategate.sh enforces it. Close an item by deleting its line.

## P maintainer actions


## Q queue


## B suspected bugs


## C standing constraints

- C-001 standing spec | The server targets the newest MCP revision and the newest go-sdk release, and the push gate refuses when either falls behind. | tools/specgate.sh | [live 10-28]
- C-002 standing tests | The database tests built with -tags pg run on Postgres 14 to 18 through tools/pg.sh, both in the local push gate and in the ci workflow on every pull request and push to main, while the image and release workflows run only the unit tests. | .github/workflows/ci.yml | [live 10-30]
- C-003 standing sql | pgx sends an Exec without arguments on the simple protocol, which can carry several statements, so the server never calls Exec: every statement goes through pg.Tx.Query or pg.Tx.Describe, and tools/gogate.sh does not catch a direct Exec yet. | internal/pg/pg.go | [live 10-28]
- C-004 standing sql | Every write cap counts the larger of the command tag and the rows changed in user tables across the statement, so cascades, triggers, functions and WITH queries count, and a write with track_counts off is refused. | internal/core/freesql.go | [live 10-28]
- C-005 standing sql | Free SQL and catalog tools close their connection after each call, because a statement that is not the server's own can change the session that the pool would hand to the next call, and a test shows the leak without it for each. | internal/core/entry.go | [live 10-28]
- C-006 standing sql | execute_sql and execute_ddl split by the command tag and the rows changed in the system catalogs, never by the SQL text, so a structure change inside a DO block, procedure or function cannot pass through execute_sql. | internal/core/freesql.go | [live 10-28]

