# State

One keyed line per item, read by grep. The grammar is in CLAUDE.md and
tools/stategate.sh enforces it. Close an item by deleting its line.

## P owner actions


## Q queue

- Q-002 open core | M1 builds the config, tokens, a pool per login, the start checks, the id check, the built-in tools and free SQL, and moves the tool lint in internal/core onto a served tools/list. | docs/DESIGN.md sections 2 to 7 | [doc 10-28]
- Q-003 open catalog | M2 builds the .sql reader, the index and the check command, and fills each parameter's Postgres type from the prepare so that core.Tool builds the input schema from it. | docs/DESIGN.md section 3 | [doc 10-28]
- Q-004 open tests | M3 builds the Postgres 14 to 18 harness in local Docker that every database test runs on. | docs/PLAN.md section 4 | [doc 10-28]
- Q-005 open straza | M4 fires the server behind Straza once M1 to M3 land, with the database scope, approval and the id check through the gateway. | docs/DESIGN.md section 8 | [doc 10-28]

## B suspected bugs

## C standing constraints

- C-001 standing spec | The server targets the newest MCP revision and the newest go-sdk release, and the push gate refuses when either falls behind. | tools/specgate.sh | [live 10-28]
- C-002 standing tests | No CI runs yet, so every database test runs against Postgres 14 to 18 in local Docker before a push. | docs/PLAN.md section 4 | [doc 10-28]

## E environment

## S siblings
