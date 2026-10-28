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
hints and the marks. The catalog never imports the go-sdk. `cmd/adaptive-pg-mcp`,
`internal/config` and `internal/pg` arrive with M1.

## 3. Configuration

`server.yaml` holds every key below. Only the two `allow_` keys have a
default, false, which is the refusing side. Any other missing key fails the
start and names the key.

```yaml
mode: straza                     # or standalone
listen: 127.0.0.1:8080           # or stdio
free_sql: read-write             # off, read or read-write
limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }
gateway_tokens_sha256: [ "..." ] # mode straza: the call and listing tokens
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
`finance_reader`.

`access.yaml` is used in standalone mode only. Each caller has a name, a
SHA-256 of its token, and the tools and databases it may use. Tables may
narrow it further. `adaptive-pg-mcp token` prints a new token and its hash.

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
| one per catalog entry | its params. `database` is an enum of the entry's databases | the entry's login |

A `where` is a list of `{column, op, value}` with `op` from `=`, `<>`, `<`,
`<=`, `>`, `>=`, `in`, `not in`, `is null`, `is not null`, `like` and
`ilike`. Identifiers are quoted by the server, and values are bind
parameters.

`list_databases` returns `{objects: [{value, id, title}], nextCursor}` in
`structuredContent`. The value is the config name. The id is the cluster's
system identifier and the database OID joined by a colon, because OIDs repeat
across clusters and Straza fails a listing that holds one id under two
values. The core lane confirms which grant reading the system identifier
needs on 14 to 18. A standalone caller sees only its own databases.

## 5. Every call, in order

1. The bearer token matches a configured hash, compared in constant time.
2. In standalone mode, the caller may use this tool on this database.
3. The arguments match the input schema.
4. When `_meta` carries `ai.straza/object-id`, it must equal the database's
   id read on the connection the call uses. A mismatch refuses the call.
5. The call runs as one prepared statement in its own transaction, with
   `statement_timeout` from the limits. A read runs in `BEGIN READ ONLY`.
6. A write that touches more rows than its cap is rolled back and refused. A
   read stops at the row or byte cap and says where it stopped.
7. The answer carries `structuredContent` with the column names and types,
   the rows, and `truncated`. A write answers `rowsAffected` and the
   `returning` rows.

## 6. Checks before it serves

`serve` refuses to start, and `check` fails, in three cases only:

- A login is a superuser while `free_sql` is on, and its database does not
  set `allow_superuser: true`. A superuser's free SQL is not limited to the
  database.
- A database has dblink or postgres_fdw installed and does not set
  `allow_cross_database: true`. Either lets a call reach another database,
  which breaks the promise that a call touches only the database it names.
- A catalog statement does not prepare on one of its databases, or its
  parameter count differs from its index.

Everything else is reported, not refused. `check` and the start log say what
each login can do, such as that it can write to every table or has BYPASSRLS,
so the operator sees the posture. `check` runs without serving, so a catalog
repo runs it in its CI.

## 7. Errors, in plain words

Every refusal says what failed, why, and what to do. The build quotes these
word for word:

- `sales_eu was dropped and created again after Straza listed it, so the
  call is refused. Try again once Straza lists the databases again, or ask an
  administrator to refresh the list.`
- `update_rows on helpdesk would change 1,240 rows and the cap is 100, so
  nothing was changed. Narrow the where filter, or ask an administrator to
  raise write_rows.`
- `the login of sales_eu is a superuser and free SQL is on, so
  adaptive-pg-mcp will not start. Use a login without superuser, turn
  free_sql off, or set allow_superuser: true for sales_eu.`
- A report line, not a refusal: `the login of sales_eu can write to all 212
  tables, so its read tools rely on read-only transactions alone. A separate
  read login with SELECT only adds the database's own lock.`
- `finance/revenue.sql statement revenue_by_region does not prepare on
  sales_us: column "net" does not exist. Fix the statement or remove sales_us
  from its databases.`

## 8. Behind Straza

Straza needs no change to govern this server. The marks, the listing shape,
the object id and the required string argument follow Straza's object marks
reference. In `mode: straza`, the server accepts only the tokens Straza holds,
one for calls and one for the listing. Straza decides which tools and which
databases each agent may use, holds a call for approval when its policy says
so, and writes the audit record. The server checks the id Straza sends, so a
database dropped and created again under the same name is refused.

## 9. Tests

Table-driven, test-first for the token check, the id check and the start
checks. Every package test that touches a database runs against Postgres 14
to 18 in local Docker. The authorization test runs on
every protocol revision the SDK serves.

## 10. Not in v1

Rows filtered per person, which needs a caller identity from Straza. Long
exports through MCP tasks. Write confirmation through elicitation in
standalone mode. Catalog reload without a restart. dbt models. Read replicas.
Databases other than Postgres.
