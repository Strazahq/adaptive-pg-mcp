# State

One keyed line per item, read by grep. The grammar is in CLAUDE.md and
tools/stategate.sh enforces it. Close an item by deleting its line.

## P owner actions

- P-003 waiting-owner core | Confirm the names the M1 lane chose: the HTTP endpoint /mcp and the token command's output lines token: and token_sha256:. | internal/core/http.go | [doc 10-28]
- P-004 waiting-owner core | Decide whether a browser-based client may call over HTTP, since the server refuses every request with an Origin header and allowing one needs a config key the owner names. | internal/core/http.go | [live 10-28]
- P-005 waiting-owner core | Decide how HTTP gets TLS, since the server has none and a listen address beyond loopback sends bearer tokens in cleartext: TLS keys in server.yaml, or a documented TLS proxy in front. | internal/core/http.go | [doc 10-28]
- P-006 waiting-owner core | Confirm the names the unit 3a lane chose: the check output prefixes refusal:, report: and check:, a list_databases page of 25 with the config name as title, and the report sentences in internal/core/start.go. | internal/core/start.go | [doc 10-28]
- P-010 waiting-owner core | Decide how the database id stays unique across physical copies, since a standby or a restored base backup has the same system identifier and OIDs, so a config that lists a primary and its copy gives one id under two values and Straza fails the listing. | internal/core/objectid.go | [live 10-28]
- P-011 waiting-owner core | Confirm the shapes unit 3b chose: order_by entries of column and direction asc or desc, returning as a list of columns, aggregate functions count, sum, avg, min and max, a page of 100 for list_schemas and list_tables, schema defaulting to public, and the output columns of the listing and describe tools. | internal/core/schema.go | [doc 10-28]
- P-014 waiting-owner catalog | Confirm the names the M2 lane chose: the load, start, call and report sentences in internal/catalog and internal/core/entry.go, the Go names catalog.Load, catalog.Check, pg.Tx.Describe and config.Decode, and the example tools tickets_by_status and close_stale_tickets. | internal/catalog/load.go | [doc 10-28]
- P-015 waiting-owner catalog | Decide whether a standalone caller whose access.yaml entry lists tables may use a catalog tool its tools name, as the M2 lane built it, since the statement is fixed and the entry names the tool. | internal/core/authorize.go | [live 10-28]
- P-016 waiting-owner catalog | Decide whether the catalog check also checks grants, since Parse and Describe pass a statement that the entry's login may not run, and the tool then fails at every call. | internal/catalog/check.go | [live 10-28]
- P-017 waiting-owner catalog | Decide what the loader does with hidden folders and linked folders, since a Kubernetes ConfigMap mount loads its statements twice and fails on duplicate names, and a linked folder is skipped without a word. | internal/catalog/load.go | [live 10-28]
- P-018 waiting-owner catalog | Decide whether an entry's limits may raise the caps of server.yaml, as the field-by-field override now allows, or only lower them. | internal/core/entry.go | [doc 10-28]

## Q queue

- Q-005 open straza | M4 fires the server behind Straza once M1 to M3 land, with the database scope, approval and the id check through the gateway. | docs/DESIGN.md section 8 | [doc 10-28]

## B suspected bugs

- B-001 open gates | tools/textgate.sh takes the sync.Mutex method that releases a lock for a filler word from its list, so Go code that releases a mutex cannot pass the commit gate. | tools/textgate.sh | [live 10-28]

## C standing constraints

- C-001 standing spec | The server targets the newest MCP revision and the newest go-sdk release, and the push gate refuses when either falls behind. | tools/specgate.sh | [live 10-28]
- C-002 standing tests | No CI runs yet, so the push gate runs every database test, built with -tags pg, against Postgres 14 to 18 in local Docker through tools/pg.sh. | .githooks/pre-push | [live 10-28]
- C-003 standing sql | pgx sends an Exec without arguments on the simple query protocol, which can carry several statements, so every statement of the server goes through pg.Tx.Query, which forces QueryExecModeDescribeExec, the catalog check prepares through pg.Tx.Describe, which sends Parse and Describe only, and tools/gogate.sh does not catch a direct Exec yet. | internal/pg/pg.go | [live 10-28]
- C-004 standing sql | Every write cap counts the larger of the command tag and the rows changed in user tables across the statement, so cascades, triggers, functions and WITH queries count, and a write with track_counts off is refused. | internal/core/freesql.go | [live 10-28]
- C-005 standing sql | Free SQL and catalog tools close their connection after each call, because a statement that is not the server's own can change the session that the pool would hand to the next call, and a test shows the leak without it for each. | internal/core/entry.go | [live 10-28]
- C-006 standing sql | execute_sql and execute_ddl split by the command tag and the rows changed in the system catalogs, never by the SQL text, so a structure change inside a DO block, procedure or function cannot pass through execute_sql. | internal/core/freesql.go | [live 10-28]

## E environment

## S siblings
