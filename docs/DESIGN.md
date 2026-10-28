# Design

This is the design of v1, as the owner approved it on 2026-10-28. The
decisions behind it are in docs/PLAN.md section 2.

## 1. What it is

An MCP server for PostgreSQL whose every data call names its database. The
agent gets typed tools, an organization's own SQL as named tools, and free SQL
under the grants of the login each call runs on. It runs on its own with an
access file, or behind the Straza gateway, which then decides per agent.

## 2. Scope of v1

One static Go binary (CGO_ENABLED=0) built on the MCP go-sdk and pgx. It
serves Streamable HTTP and stdio. Its commands are `serve`, `check` and
`token`. It runs standalone, or behind Straza, and the mode is set in the
config. Postgres 14 to 18 are supported.

The code lives under `internal/`, so no outside module imports it before v1
settles. `internal/catalog` reads the organization's statements and checks
them on their databases. `internal/core` turns catalog entries into tools and
holds the built-in tools and the call path. They meet at `catalog.Entry`: the
catalog fills it with what Postgres reports, such as each parameter's type
name, and the core decides what the agent sees, such as the JSON Schema, the
hints and the marks. The catalog never imports the go-sdk.
`cmd/adaptive-pg-mcp` holds the commands, `internal/config` reads
`server.yaml` and `access.yaml`, and `internal/pg` arrives with the pools.

Over HTTP the endpoint is `/mcp`. Every request carries
`Authorization: Bearer` and a token, and the server checks it on every
request, not only the first. A request with an `Origin` header is refused,
because the server serves no web page. A request body is capped at 1 MiB.
Revisions before 2026-07-28 keep protocol sessions, and each session is bound
to the caller that opened it. Revision 2026-07-28 has no sessions, so each of
its requests stands alone. On stdio the caller's token comes from the
environment variable `ADAPTIVE_PG_MCP_TOKEN`, and `serve` checks it once and
does not start when it is missing or matches nothing.

## 3. Configuration

`server.yaml` holds every key below. Only the two `allow_` keys have a
default, false, which is the refusing side. Any other missing key fails the
start and names the key, and a key that the file's version does not have
fails it and names its line. `serve` reads the file that `-config` names,
./server.yaml by default. examples/ holds a server.yaml and an access.yaml
that a test loads.

```yaml
version: 1                       # the format version of this file
mode: straza                     # or standalone
listen: 127.0.0.1:8080           # or stdio
free_sql: read-write             # off, read or read-write
limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }
gateway_tokens_sha256: [ "..." ] # mode straza only: the call and listing tokens
access: ./access.yaml            # mode standalone only
catalog: ./catalog               # a folder, or none
databases:
  sales_eu:
    login: { dsn_env: SALES_EU_DSN }   # one login for reads and writes
  helpdesk:
    logins:                            # or separate read and write logins
      read:  { dsn_env: HELPDESK_READ_DSN }
      write: { dsn_env: HELPDESK_WRITE_DSN }
    allow_cross_database: false        # true allows dblink or postgres_fdw
    allow_superuser: false             # true allows a superuser with free SQL on
```

A database names one login, or separate `read` and `write` logins. One
powerful login is the common setup in practice, and the server works on it:
read tools and `query_sql` still run in read-only transactions, and Straza
still decides the tools, the databases and the approval. Separate logins add
the database's own grants as one more lock. Logins are always real logins,
never roles to switch to, because a role switch is no boundary against free
SQL. A catalog tool may name any extra login of its database, such as
`finance_reader`, which `logins` lists next to `read` and `write`.

`access` is required in mode standalone and refused in mode straza, and
`gateway_tokens_sha256` the other way round. Relative `access` and `catalog`
paths resolve against the folder of `server.yaml`. Every `dsn_env` variable
must be set and not empty, and the load names the variable, never its value.

`access.yaml` is used in standalone mode only:

```yaml
version: 1
callers:
  - name: support_bot
    token_sha256: "<64 lowercase hex characters>"
    tools: [list_tables, read_rows]
    databases: [helpdesk]
    tables: [public.tickets]   # optional
```

Each caller has a name, a SHA-256 of its token, and the tools and databases
it may use. Tables may narrow it further. Names and hashes are unique, and
every database a caller names must be in `server.yaml`. `adaptive-pg-mcp
token` prints a new token on a `token:` line and its hash on a
`token_sha256:` line. A token is `apm_` and 43 base64url characters made from
32 random bytes.

The catalog folder holds the organization's `.sql` files, with a
`-- name: <tool>` line before each statement, and a `tools.yaml` index per
folder. Each index entry has `description`, `mode`, `databases`, `login`,
`params` (in `$1` order, with name, description and optional `enum`,
`pattern`, `minimum`, `maximum`, `maxLength`, `required`) and optional
`limits`. A statement without an entry is not a tool, and two statements with
one name fail the load. An entry the core cannot turn into a tool fails the
load too, such as one whose name is not snake_case or whose parameter has a
Postgres type outside the supported list.

## 4. Tools

Every tool except `list_databases` takes a required top-level `database`
string and carries the mark
`ai.straza/object: {type: database, argument: database}`. Read tools set
`readOnlyHint`, and write tools set `destructiveHint`.

| Tool | Arguments besides `database` | Runs on |
|---|---|---|
| `list_databases` | optional `cursor` only, no `database`. Reach `all` plus `ai.straza/lists-objects` | the server's config |
| `list_schemas`, `list_tables`, `describe_table` | `schema`, `table`. Only objects the login may use | read |
| `read_rows` | `schema`, `table`, `columns`, `where`, `order_by`, `limit` | read |
| `aggregate` | `schema`, `table`, `function`, `column`, `where`, `group_by` | read |
| `insert_rows` | `schema`, `table`, `rows`, `returning` | write |
| `update_rows` | `schema`, `table`, `set`, `where` (required), `returning` | write |
| `delete_rows` | `schema`, `table`, `where` (required), `returning` | write |
| `query_sql` | `sql`, with `free_sql` at read or read-write | read |
| `execute_sql` | `sql`, with `free_sql` at read-write | write |
| `execute_ddl` | `sql`, with `free_sql` at read-write | write |
| one per catalog entry | its params. `database` is an enum of the entry's databases | the entry's login |

A `where` is a list of `{column, op, value}` with `op` from `=`, `<>`, `<`,
`<=`, `>`, `>=`, `in`, `not in`, `is null`, `is not null`, `like` and
`ilike`. Identifiers are quoted by the server, and values are bind
parameters. An `in` or `not in` filter takes at most 1,000 values. A value of
JSON null binds as SQL NULL, a json or jsonb column takes the value's JSON
text, and any other column takes the value's text, which Postgres parses as
the column's type. `schema` is optional and defaults to public, and names
compare exactly. `order_by` entries are `{column, direction}` with `asc` or
`desc`, `returning` is a list of columns, and `aggregate` takes `count`,
`sum`, `avg`, `min` or `max`, with `column` optional for `count`.

Answers write numbers and booleans as JSON numbers and booleans, json as
itself, dates and timestamps in ISO 8601 with timestamptz in UTC, and every
other type as its Postgres text. `list_schemas` and `list_tables` page like
`list_databases`, at most 100 objects or the row cap when it is lower, and
answer the read shape plus `nextCursor`.

In standalone mode, `tables` entries are `schema.table`, split at the first
dot. They narrow `describe_table`, `read_rows`, `aggregate` and the write
tools, `list_schemas` and `list_tables` answer only those tables and their
schemas, and a caller whose entry lists tables is refused `query_sql`,
`execute_sql` and `execute_ddl`, because free SQL can reach any table. In
standalone mode `tools/list` shows a caller only the tools its entry lists,
with each `database` enum cut to its databases.

`execute_sql` and `execute_ddl` split free SQL by what Postgres reports
about the statement, never by the SQL text. The statement runs in its
transaction, and the server reads its command tag and the rows it changed in
the system catalogs, from `pg_stat_xact_sys_tables` without the TOAST
schemas. Plain row writes leave that count at 0, and TRUNCATE and changes of
structure move it, also inside a DO block, a procedure or a function.

- `execute_sql` runs a statement whose tag is INSERT, UPDATE, DELETE, MERGE,
  SELECT, CALL, DO, EXPLAIN or COPY and that changed no catalog row, under
  the write cap. Anything else is rolled back and refused.
- `execute_ddl` runs TRUNCATE and changes of structure, CREATE TABLE AS,
  SELECT INTO and CREATE MATERIALIZED VIEW AS included. It refuses a
  row-tagged statement that changed no catalog row, because that is row work
  for `execute_sql`. Its write cap counts only the rows changed in user tables
  that existed before the statement, so rows written into a table it creates
  do not count. REFRESH MATERIALIZED VIEW stays uncapped like TRUNCATE.
- `insert_rows`, `update_rows` and `delete_rows` refuse a statement that
  changed catalog rows, which only a trigger on the table can cause.

A refused statement has run until the rollback, so it held its locks for
that long. Large object functions write a system catalog, so `execute_sql`
refuses them.

`list_databases` returns `{objects: [{value, id, title}], nextCursor}` in
`structuredContent`. The value is the config name. The id is the cluster's
system identifier and the database OID joined by a colon, because OIDs repeat
across clusters and Straza fails a listing that holds one id under two
values. A plain login reads the system identifier on 14 to 18, because
`pg_control_system()` keeps its default EXECUTE grant to PUBLIC. The id is
read on the database's read login, and a page with an id that cannot be read
is a tool error, because an item without an id would hide a database that was
dropped and created again. The title is the config name. A page holds at most
25 databases, and `nextCursor` is opaque. A standalone caller may call it
only when its access.yaml tools list it, and it sees only the databases of
its entry. The tool carries
`ai.straza/object: {type: database, reach: all}`,
`ai.straza/lists-objects: {type: database}` and `readOnlyHint`.

## 5. Every call, in order

1. The bearer token matches a configured hash, compared in constant time.
2. In standalone mode, the caller may use this tool, then this database.
3. The arguments match the input schema, and in standalone mode the table is
   one the caller's entry allows.
4. When `_meta` carries `ai.straza/object-id`, it must equal the database's
   id read on the connection the call uses. A mismatch refuses the call.
5. The call runs as one prepared statement in its own transaction, with
   `statement_timeout` from the limits. A read runs in `BEGIN READ ONLY`.
6. A write that touches more rows than its cap is rolled back and refused.
   The count is the larger of the command tag and the rows the statement
   inserted, updated and deleted in user tables, with cascades, triggers,
   functions and WITH queries included, and with `track_counts` off a write
   is refused. A read stops at the row or byte cap and says where it stopped,
   and `truncated` is true whenever more rows follow.
7. Free SQL closes its connection after the call, because a statement can
   change the session that the pool would hand to the next call.
8. The answer carries `structuredContent` with the column names and types,
   the rows, and `truncated`. A write answers `rowsAffected` and the
   `returning` rows.

## 6. Checks before it serves

`serve` refuses to start, and `check` fails, in four cases only:

- A login is a superuser while `free_sql` is on, and its database does not
  set `allow_superuser: true`. A superuser's free SQL is not limited to the
  database. A login that is a member of a superuser role counts, because free
  SQL can switch to that role.
- A database that one of its logins reaches has dblink or postgres_fdw
  installed, and the database does not set `allow_cross_database: true`.
  Either lets a call reach another database, which breaks the promise that a
  call touches only the database it names.
- A login of a database reaches another database than the database's read
  login, by the id of section 4, or the ids cannot be compared because one
  cannot be read. A call would then touch a database other than the one it
  names.
- A catalog statement does not prepare on one of its databases, or its
  parameter count differs from its index.

The catalog check arrives with the catalog loader in M2. A login that cannot
connect, or whose connection string does not parse, also refuses the start,
under invariant 8, because a check that cannot run never happened.

Everything else is reported, not refused. `check` and the start log say what
each login can do, such as that it can write to every table or has BYPASSRLS,
so the operator sees the posture. `check` runs without serving, so a catalog
repo runs it in its CI. It prints one `refusal:` or `report:` line per finding
and a closing `check:` line, and it exits 1 when the start would be refused.
`serve` logs refusals at ERROR and reports at WARN.

## 7. Errors, in plain words

Every refusal says what failed, why, and what to do. The build quotes these
word for word:

- `sales_eu was dropped and created again after Straza listed it, so the
  call is refused. Try again once Straza lists the databases again, or ask an
  administrator to refresh the list.`
- `update_rows on helpdesk would change 1,240 rows and the cap is 100, so
  nothing was changed. Narrow the where filter, or ask an administrator to
  raise write_rows.`
- `delete_rows on helpdesk would change 501 rows and the cap is 100, so
  nothing was changed. The statement changes rows in other tables through
  triggers or cascades, so narrow the call, or ask an administrator to raise
  write_rows.`
- `execute_sql on helpdesk ran a statement whose command tag is TRUNCATE
  TABLE, which the write cap cannot count, so it was rolled back and nothing
  was changed. Use execute_ddl for TRUNCATE and for statements that change
  structure.`
- `execute_ddl on helpdesk ran a statement whose command tag is DELETE 1,
  which works on rows and changed no structure, so it was rolled back and
  nothing was changed. Use execute_sql to change rows under the write cap, or
  query_sql to read them.`
- `execute_sql on helpdesk ran a statement whose command tag is DO and that
  changed structure, as the system catalogs show, so it was rolled back and
  nothing was changed. Use execute_ddl for TRUNCATE and for statements that
  change structure.`
- `execute_ddl on helpdesk would change 150 rows and the cap is 100, so
  nothing was changed. Run row changes through execute_sql, or ask an
  administrator to raise write_rows.`
- `support_bot may not use update_rows, because its entry in access.yaml
  does not list that tool, so the call is refused. Use a tool that its entry
  lists, or ask an administrator to add update_rows to its tools.`
- `support_bot may not use sales_eu, because its entry in access.yaml does
  not list that database, so the call is refused. Name a database that
  list_databases answers, or ask an administrator to add sales_eu to its
  databases.`
- `support_bot may not use public.customers on helpdesk, because its entry
  in access.yaml narrows it to the table public.tickets, so the call is
  refused. Name that table, or ask an administrator to add public.customers
  to its tables.`
- `support_bot may not use query_sql, because its entry in access.yaml
  narrows it to tables, and free SQL can reach any table, so the call is
  refused. Use the table tools, such as read_rows, on its tables, or ask an
  administrator to remove tables from its entry.`
- `the write login of helpdesk reaches another database than the read login
  of helpdesk, and its calls would change a database that list_databases does
  not name, so adaptive-pg-mcp will not start. Point the connection strings
  in HELPDESK_WRITE_DSN and HELPDESK_READ_DSN at the same database.`
- `the login of sales_eu is a superuser and free SQL is on, so
  adaptive-pg-mcp will not start. Use a login without superuser, turn
  free_sql off, or set allow_superuser: true for sales_eu.`
- A report line, not a refusal: `the login of sales_eu can write to all 212
  tables, so its read tools rely on read-only transactions alone. A separate
  read login with SELECT only adds the database's own lock.`
- `finance/revenue.sql statement revenue_by_region does not prepare on
  sales_us: column "net" does not exist. Fix the statement or remove sales_us
  from its databases.`
- `the request carries no bearer token, so adaptive-pg-mcp refused it. Send
  the header Authorization: Bearer followed by the token issued for this
  server.`
- `the bearer token of the request is malformed or not one this server
  knows, so adaptive-pg-mcp refused it. Send the token issued for this
  server, once, after Bearer.`
- `ADAPTIVE_PG_MCP_TOKEN is not set, and on stdio adaptive-pg-mcp needs it to
  know who calls, so it will not start. Set ADAPTIVE_PG_MCP_TOKEN to the token
  of a caller in access.yaml in the environment the client starts the server
  with.`
- `the write login of helpdesk is a superuser and free SQL is on, so
  adaptive-pg-mcp will not start. Use a login without superuser, turn
  free_sql off, or set allow_superuser: true for helpdesk.`
- `sales_eu has dblink installed, which lets a call reach another database,
  so adaptive-pg-mcp will not start. Drop the extension from sales_eu, or set
  allow_cross_database: true for sales_eu.`
- `the write login of helpdesk reaches a database that has dblink installed,
  which lets a call reach another database, so adaptive-pg-mcp will not
  start. Drop the extension from that database, or set allow_cross_database:
  true for helpdesk.`
- `the login of sales_eu cannot connect, because the server answered:
  password authentication failed for user "sales" (SQLSTATE 28P01), so
  adaptive-pg-mcp will not start. Check the connection string in SALES_EU_DSN,
  and that the database server is up and accepts the login.`
- `the cursor of list_databases does not decode to a page of this listing,
  so the call is refused. Pass the nextCursor of the previous page exactly as
  it came, or leave cursor out to start from the first page.`

## 8. Behind Straza

Straza needs no change to govern this server. The marks, the listing shape,
the object id and the required string argument follow Straza's object marks
reference. In `mode: straza`, the server accepts only the tokens Straza holds,
one for calls and one for the listing. Straza decides which tools and which
databases each agent may use, holds a call for approval when its policy says
so, and writes the audit record. The server checks the id Straza sends, so a
database dropped and created again under the same name is refused. In mode
straza every request has the caller `straza`, because Straza holds the
identity of the agent.

## 9. Tests

Table-driven, test-first for the token check, the id check and the start
checks. Every package test that touches a database runs against Postgres 14
to 18 in local Docker. Such a test carries the build constraint
`//go:build pg` and gets its servers from internal/pgtest. Plain
`go test ./...` in the commit gate needs no Docker, and `make pgtest` and the
push gate run `go test -tags pg ./...` against the five servers that
`sh tools/pg.sh up` starts. The authorization test runs on every protocol
revision the SDK serves.

## 10. Not in v1

Rows filtered per person, which needs a caller identity from Straza. Long
exports through MCP tasks. Write confirmation through elicitation in
standalone mode. Catalog reload without a restart. dbt models. Read replicas.
Databases other than Postgres.
