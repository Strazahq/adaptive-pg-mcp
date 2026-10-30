# Design

This is the design of v1. The decisions behind it are in docs/PLAN.md
section 2.

## 1. What it is

An MCP server for PostgreSQL whose every data call names its database. The
agent gets typed tools, an organization's own SQL as named tools, and free
SQL under the grants of the login each call runs on. It runs in standalone
mode with an access file, or in straza mode behind the Straza gateway, which
then decides per agent.

## 2. Scope of v1

One static Go binary (CGO_ENABLED=0) built on the MCP go-sdk and pgx. It
serves Streamable HTTP and stdio. Its commands are `serve`, `check` and
`token`. `mode` in server.yaml selects standalone mode or straza mode.
Postgres 14 to 18 are supported.

The code lives under `internal/`, so no outside module imports it before v1
settles. `cmd/adaptive-pg-mcp` holds the commands, and `internal/config`
reads server.yaml and access.yaml. `internal/pg` holds a pool per login and
is the only path by which SQL reaches Postgres. `internal/catalog` reads the
organization's statements and checks them on their databases.
`internal/core` holds the built-in tools, the call path and the start
checks, and turns catalog statements into tools. The catalog and the core
meet at `catalog.Entry`: the catalog fills it with what Postgres reports,
such as each parameter's type name, and the core decides what the agent
sees, such as the JSON Schema, the hints and the marks. The catalog never
imports the go-sdk.

Over HTTP the endpoint is `/mcp`. Every request carries
`Authorization: Bearer` and a token, and the server checks it on every
request, not only the first. A request with an `Origin` header is refused,
because the server serves no web page. A request body over 1 MiB is
refused. Revisions before 2026-07-28 keep protocol sessions, and each
session is bound to the caller that opened it, while the caller of each
request still comes from that request's own token. Revision 2026-07-28 has
no sessions, so each of its requests stands alone. On stdio the caller's
token comes from the environment variable `ADAPTIVE_PG_MCP_TOKEN`, and
`serve` checks it once and does not start when it is missing or matches
nothing.

With `tls` in server.yaml, HTTP serves HTTPS only. It accepts TLS 1.2 and
later with Go's default cipher suites and curves, offers HTTP/2 and HTTP/1.1
by ALPN, and asks for no client certificate. A certificate and key that do
not load, or a certificate that has expired, refuse the start. While it
serves, the server reads both files again at a handshake once a minute has
passed since the last read, so a tool such as cert-manager that replaces
them in place renews the pair without a restart. A new pair serves from
that handshake on once it loads, and the log says so. A new pair that does
not load leaves the old one in service, and the log warns once per change
of the files. Without `tls`, the server serves plain HTTP on an address of
this machine only. Any other address, `0.0.0.0` and `::` included, fails the
load unless `allow_plain_http: true` says that something in front of the
server encrypts the traffic, such as a service mesh. The start then reports
that the bearer tokens cross the network in cleartext.

## 3. Configuration

`server.yaml` holds every key below. `tls`, `listing_tokens_sha256` and
the three `allow_` keys may be left out, and each mode takes one of `access`
and `gateway_tokens_sha256`. The `allow_` keys default to false, which is the
refusing side, and without `tls` HTTP stays plain. Any other missing key
fails the load and names the key, and an unknown key fails it and names its
line. A file that holds a second YAML document fails the load too, in
server.yaml, access.yaml and tools.yaml alike, because the rest would be
ignored. `serve` and `check` read the file that `-config` names,
./server.yaml by default. examples/ holds a server.yaml and an access.yaml
that a test loads.

```yaml
version: 1                       # the format version of this file
mode: straza                     # or standalone
listen: 127.0.0.1:8080           # or stdio
tls:                             # optional: serve HTTPS with this pair
  cert_file: /etc/adaptive-pg-mcp/server.crt
  key_file: /etc/adaptive-pg-mcp/server.key
allow_plain_http: false          # true only when something else encrypts
free_sql: read-write             # off, read or read-write
limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }
gateway_tokens_sha256: [ "..." ] # straza mode only: the Straza tokens
listing_tokens_sha256: [ "..." ] # straza mode only: for list_databases
access: ./access.yaml            # standalone mode only
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

A database names one login, or separate `read` and `write` logins. One login
with broad grants is the common setup, and the server works on it: read
tools and `query_sql` still run in read-only transactions, and Straza still
decides the tools, the databases and the approval. Separate logins add the
database's own grants as a second boundary. Logins are always real logins,
never roles to switch to, because a role switch is no boundary against free
SQL. A catalog tool may name any extra login of its database, such as
`finance_reader`, which `logins` lists next to `read` and `write`.

`access` is required in standalone mode and refused in straza mode, and
`gateway_tokens_sha256` the other way round. `listing_tokens_sha256` is
refused in standalone mode, and a hash in both token lists fails the load. Relative `access`, `catalog`,
`tls.cert_file` and `tls.key_file` paths resolve against the folder of
server.yaml. Every `dsn_env` variable must be set and not empty, and the
load names the variable, never its value.

`tls` needs both `cert_file` and `key_file`: the PEM file of the server's
certificate, followed by any intermediate certificates, and the PEM file of
its private key. Without `tls`, `listen` must be localhost or a loopback
address, such as 127.0.0.1 or [::1], unless `allow_plain_http: true` accepts
plain HTTP beyond this machine. A host name other than localhost counts as
beyond this machine, because the load does not resolve names. A `listen`
without a host, such as `:8080`, always fails the load, because it would
serve on every interface. `tls` and `allow_plain_http: true` each fail the
load with `listen: stdio`, where they do nothing, and they fail it together,
because they contradict each other. `allow_plain_http: false` is always
accepted. The system resolves localhost when `serve` binds the address, so
without `tls` and `allow_plain_http: true`, `serve` refuses to start before
the start checks when the bound address is not a loopback address. `check`
binds no address, so it does not see this case.

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

Each caller has a name, the SHA-256 of its token, and the tools and
databases it may use. `tables` may narrow it further. Names and hashes are
unique, and every database a caller names must be in server.yaml.
`adaptive-pg-mcp token` prints a new token on a `token:` line and its hash
on a `token_sha256:` line. A token is `apm_` and 43 base64url characters
made from 32 random bytes.

The catalog folder holds the organization's `.sql` files and one
`tools.yaml` per folder, at any depth. A statement starts at a
`-- name: <tool>` line and runs to the next such line or to the end of its
file, and before its first name line a file holds only comments and blank
lines. A tool in tools.yaml takes the statement of its name from the `.sql`
files of its own folder.

```yaml
version: 1
tools:
  revenue_by_region:
    description: Sums net revenue per region for one month. One row per region.
    mode: read                     # or write
    databases: [sales_eu, sales_us]
    login: read                    # or write, or an extra login such as finance_reader
    params:                        # in $1 order
      - name: month
        description: The first day of the month, such as 2026-09-01.
        required: true             # also optional enum, pattern, minimum, maximum, maxLength
    limits: { rows: 100 }          # optional: rows, bytes, timeout, write_rows
```

`description`, `mode`, `databases` and `login` are required, and `params`
and `limits` are optional. A parameter is optional unless it sets
`required: true`, and a call that leaves it out binds NULL. A field of
`limits` replaces the same field of server.yaml for that tool, and may raise
or lower it. The start reports each cap that a catalog tool raises. An
unknown key fails the load and names it, and so does a version other
than 1. A statement that no tools.yaml lists is not a tool, and the start
reports it.

These fail the load: a tool without a statement, two statements with one
name anywhere in the catalog, a tool with the name of a built-in tool, a
tool that names a database that server.yaml does not define or a login that
its database does not have, and a statement that holds no SQL or starts
with a semicolon, which would cut it off from the EXPLAIN of section 6. A
catalog that fails to load refuses the start.

The catalog reads only files inside its folder. It skips files and folders
whose name starts with a dot, so a Kubernetes ConfigMap mount loads each
statement once, and it reads each real folder once. It follows a link to a
folder, a `.sql` file or a `tools.yaml` only when the link is relative and
stays inside the folder at every step, and such a link that does not fails
the load. Other links are ignored, as other files are. A catalog tool that
the core cannot build refuses the start once Postgres has reported its
types, such as one whose name is not snake_case or whose parameter has a
type outside the list of section 4. examples/catalog holds a catalog that a
test loads and prepares.

## 4. Tools

Every tool except `list_databases` takes a required top-level `database`
string and carries the mark
`ai.straza/object: {type: database, argument: database}`. Every tool sets
the four hints: read tools are read-only and idempotent, write tools are
destructive, and no tool is open-world.

| Tool | Arguments besides `database` | Runs on |
|---|---|---|
| `list_databases` | `cursor`, and no `database` | the read login of each database it lists |
| `list_schemas` | `cursor` | read |
| `list_tables` | `schema`, `cursor` | read |
| `describe_table` | `schema`, `table` | read |
| `read_rows` | `schema`, `table`, `columns`, `where`, `order_by`, `limit` | read |
| `aggregate` | `schema`, `table`, `function`, `column`, `where`, `group_by` | read |
| `insert_rows` | `schema`, `table`, `rows`, `returning` | write |
| `update_rows` | `schema`, `table`, `set`, `where`, `returning` | write |
| `delete_rows` | `schema`, `table`, `where`, `returning` | write |
| `query_sql` | `sql`, with `free_sql` at read or read-write | read |
| `execute_sql` | `sql`, with `free_sql` at read-write | write |
| `execute_ddl` | `sql`, with `free_sql` at read-write | write |
| one per catalog tool | its params, with `database` an enum of its databases | its login |

`table` is required wherever a tool takes it, and so are `rows` of
`insert_rows`, `set` and `where` of `update_rows`, `where` of `delete_rows`,
`function` of `aggregate`, and `sql`. Every other argument is optional. The
listing tools and `describe_table` show only the objects and columns that
the login may use.

A `where` is a list of `{column, op, value}` conditions joined by AND, with
`op` from `=`, `<>`, `<`, `<=`, `>`, `>=`, `in`, `not in`, `is null`, `is not
null`, `like` and `ilike`. Identifiers are quoted by the server, and values
are bind parameters. `in` and `not in` take a list of 1 to 1,000 values, `is
null` and `is not null` take none, and every other op takes one value that is
not null. In `rows` and `set`, a JSON null binds as SQL NULL. A json or jsonb
column takes the value's JSON text, and any other column takes the value's
text, which Postgres parses as the column's type. Where a tool names a table,
`schema` defaults to public, and names compare exactly. In `list_tables`,
`schema` narrows the list to one schema, and leaving it out lists every
schema. `order_by` entries are `{column, direction}` with `asc` or `desc`,
`returning` is a list of columns, and `aggregate` takes `count`, `sum`, `avg`,
`min` or `max`, with `column` optional for `count`. Groups come ordered by the
`group_by` columns.

Results write numbers and booleans as JSON numbers and booleans, json as
itself, dates and timestamps in ISO 8601 with timestamptz in UTC, and every
other type as its Postgres text. `list_schemas` and `list_tables` page with
an opaque cursor, at most 100 objects per page or the row cap when it is
lower, and return the read shape plus `nextCursor`.

In standalone mode, `tables` entries are `schema.table`, split at the first
dot. They narrow `describe_table`, `read_rows`, `aggregate` and the write
tools, `list_schemas` and `list_tables` return only those tables and their
schemas, and a caller with `tables` is refused `query_sql`, `execute_sql`
and `execute_ddl`, because free SQL can reach any table. In standalone mode
`tools/list` shows a caller only the tools access.yaml lists for it, without
free SQL for a caller with `tables`, and with each `database` enum cut to
its databases.

`execute_sql` and `execute_ddl` split free SQL by what Postgres reports
about the statement, never by the SQL text. The statement runs in its
transaction, and the server reads its command tag and the rows it changed
in the system catalogs, from `pg_stat_xact_sys_tables` without the TOAST
schemas. Plain row writes leave that count at 0, and TRUNCATE and changes
of structure move it, also inside a DO block, a procedure or a function.

- `execute_sql` runs a statement whose tag is INSERT, UPDATE, DELETE,
  MERGE, SELECT, CALL, DO, EXPLAIN or COPY and that changed no catalog row,
  under the write cap. Anything else is rolled back and refused.
- `execute_ddl` runs TRUNCATE and changes of structure, CREATE TABLE AS,
  SELECT INTO and CREATE MATERIALIZED VIEW AS included. It refuses a
  statement with a row tag that changed no catalog row, because that is row
  work for `execute_sql`. Its write cap counts only the rows changed in
  user tables that existed before the statement. Rows written into a table
  it creates do not count, and neither do the rows that TRUNCATE removes or
  that REFRESH MATERIALIZED VIEW writes into the view.
- `insert_rows`, `update_rows` and `delete_rows` refuse a statement that
  changed catalog rows, which only a trigger on the table can cause.

A refused statement has run until the rollback, so it held its locks for
that long. Large object functions write a system catalog, so `execute_sql`
refuses them.

A catalog tool takes one argument per parameter, with the JSON Schema of
the type that Postgres reports for it: bool, int2, int4, int8, float4,
float8, numeric, text, varchar, bpchar, uuid, date, timestamp, timestamptz,
json or jsonb. A parameter of a domain takes the schema of its base type.
Each argument travels as text that Postgres reads as the parameter's type.
A catalog tool with `mode: read` runs in a read-only transaction and returns
the read shape with `truncated`, and one with `mode: write` runs read-write
and returns the write shape. A write is rolled back and refused when it
changes more rows than its write cap, counted as `execute_sql` counts them,
or when it changes the system catalogs. A catalog tool closes its
connection after each call, as free SQL does. In standalone mode the start
reports each tool that access.yaml lists for a caller and the server does
not serve.

`list_databases` returns `{objects: [{value, id, title}], nextCursor}` in
`structuredContent`. The value is the config name. The id is the cluster's
system identifier, the database OID and the config name, joined by colons.
OIDs repeat across clusters, and a standby or a restored copy of a database
keeps the system identifier and the OID of its source, while Straza fails a
listing that holds one id under two values. With the config name, a
database and its copy listed under two names get two ids, and a rename in
server.yaml gives the database a new id. A plain login reads the system
identifier on 14 to 18, because `pg_control_system()` keeps its default
EXECUTE grant to PUBLIC. The id is read on the database's read login, and a
page with an id that cannot be read is a tool error, because an item
without an id would hide a database that was dropped and created again.
The title is the config name. A page holds at most 25 databases, and
`nextCursor` is opaque. A standalone caller may call it only when
access.yaml lists it for the caller, and it sees only the databases
access.yaml lists for it. The tool carries
`ai.straza/object: {type: database, reach: all}` and
`ai.straza/lists-objects: {type: database}`.

## 5. Every call, in order

1. The bearer token matches a configured hash, compared in constant time.
   Over stdio this check runs once, at start.
2. In standalone mode, access.yaml lists the tool for the caller, then the
   database. In straza mode, a listing token may call `list_databases`
   only.
3. The arguments match the input schema, and in standalone mode the table
   is one that access.yaml allows the caller.
4. When `_meta` carries `ai.straza/object-id`, it must equal the database's
   id, read in the transaction of step 5. A mismatch refuses the call.
5. The call runs in a transaction of its own on its tool's login, with
   `statement_timeout` from its caps, set through a bind parameter. A read
   runs in `BEGIN READ ONLY`. Every statement of the call, the server's own
   included, goes to Postgres as one unnamed prepared statement on the
   extended protocol, which carries exactly one statement.
6. A write that changes more rows than its cap is rolled back and refused.
   The count is the larger of the command tag and the rows the statement
   inserted, updated and deleted in user tables, with cascades, triggers,
   functions and WITH queries included, and with `track_counts` off a write
   is refused. A read stops at the row or byte cap and says where it
   stopped, and `truncated` is true whenever more rows follow.
7. Free SQL and catalog tools close their connection after the call,
   because a statement that is not the server's own can change the session
   that the pool would hand to the next call. A read that stops at its cap
   closes its connection too, because that is how pgx stops the rest of the
   statement.
8. A read returns `structuredContent` with `columns`, each with its name
   and Postgres type, `rows` as arrays in column order, and `truncated`,
   plus a short text for people. A write returns `rowsAffected` with the
   `columns` and `rows` that its statement returned, such as its
   `returning` columns.

A call that the client cancels stops its statement in Postgres.

## 6. Checks before it serves

`serve` runs the start checks before it serves, and `check` runs them
without serving. They check TLS first, then every login of every database,
then the catalog. `serve` refuses to start, and `check` exits 1, when a
check finds one of these:

- A login is a superuser while `free_sql` is on, and its database does not
  set `allow_superuser: true`. A superuser's free SQL is not confined to the
  database. A login that is a member of a superuser role counts, because
  free SQL can switch to that role.
- A database that one of its logins reaches has dblink or postgres_fdw
  installed, and the database does not set `allow_cross_database: true`.
  Either lets a call reach another database.
- A login of a database reaches another database than the database's read
  login, by the id of section 4. A call would then touch a database other
  than the one it names.
- A catalog statement does not prepare on one of its databases, its
  parameter count differs from tools.yaml, its plan on one of its databases
  needs a grant that its login lacks, or a parameter's type differs between
  two of its databases. A catalog tool that the core cannot build refuses
  too, such as one with a parameter type outside the list of section 4.

Under invariant 8, the start is also refused when a check cannot run,
because a check that cannot run never happened, and when a file that the
server needs does not load:

- A connection string does not parse, or a login cannot connect.
- The checks of a login cannot run, such as when it cannot read pg_roles,
  pg_extension or pg_class, which every login can by default.
- A login of a database with more than one login cannot read the id, so it
  cannot be compared with the read login.
- The catalog cannot be read or does not load.
- The plan of a catalog statement fails for a reason that a NULL parameter
  cannot cause, as the next paragraphs say.
- A `tls` certificate and key do not load, or the certificate has expired,
  so the server could not serve HTTPS with them.

Some refusals come before the checks, and `check` does not see them. On
stdio, `serve` refuses to start when `ADAPTIVE_PG_MCP_TOKEN` is missing or
matches no configured hash. Over HTTP, it refuses when it cannot bind the
address in `listen`, or when, without `tls` and `allow_plain_http: true`,
the bound address is not a loopback address. A server.yaml or access.yaml
that does not load stops both commands before the checks.

The catalog check prepares each statement with Parse and Describe, on each
of its databases and on its login, in a read-only transaction. In the same
transaction it then plans the statement with `EXPLAIN (ANALYZE false)` and
NULL for each parameter, and Postgres checks the grants of the plan: SELECT
on each table or column it reads, EXECUTE on each function it calls, and
INSERT, UPDATE or DELETE on the table it writes. EXPLAIN without ANALYZE
does not run the statement, so it changes no row and fires no trigger, also
for a write. While it plans, Postgres may evaluate a STABLE or IMMUTABLE
function whose arguments are all constants, a NULL parameter included, on
the tool's login and in the read-only transaction. Such a function can
leave an effect on the session, such as an advisory lock, so the check
closes the connection afterwards, and the effect ends with it. The check
also rolls its transaction back, so an effect that a commit would deliver,
such as a notification, never leaves it.

Some missing grants show on the first call instead. Postgres checks a few
grants only while a statement runs, such as USAGE on a sequence that a
column default calls nextval on. The plan also leaves out a part whose
result a NULL parameter decides, such as a STRICT function of a parameter,
and with it the grants of that part. A statement that EXPLAIN cannot plan,
such as CALL, is reported, not refused, and its tool is served. So is a
statement whose plan fails with an error that a NULL parameter can cause: a
data exception, an integrity constraint violation, a PL/pgSQL error such as
a function that raises on NULL, or an exception of a SQL or external
routine, which are the SQLSTATE classes 22, 23, P0, 2F, 38 and 39. Any
other failure of the plan refuses the start, because the check did not run:
a statement timeout, such as a plan that waits for a lock or for a slow
function, a dropped connection, a terminated backend, or the check's own
deadline of 30 seconds.

Everything else is reported, not refused. `check` and the start log say
what each login can do, such as that it can write to every table, has
BYPASSRLS or CREATEROLE, or is a member of a role that reaches the files or
programs of the database host, so the operator sees the posture. They also
report a superuser or an extension that an `allow_` key accepts, a single
login that cannot read the id, each statement that no tools.yaml lists,
each cap that a catalog tool raises, each statement whose grants the plan
could not check, each tool that access.yaml lists and the server does not
serve, and plain HTTP beyond this machine. `check` prints one `refusal:` or
`report:` line per finding and a closing `check:` line, and exits 1 when
the start would be refused, so a catalog repository can run it in CI.
`serve` logs refusals at ERROR and reports at WARN.

## 7. Refusals and reports

Every refusal and report says what failed and what to do. The tests quote
each sentence below word for word. A refused call returns its sentence in
a tool result with `isError: true`, and a refused HTTP request in the body
of its 401 or 403 response. `check` prints the findings of the start checks
on `refusal:` and `report:` lines, and `serve` logs them.

Calls:

- `the id of sales_eu differs from the one Straza listed: the database was
  recreated, or server.yaml now gives its name to another database. Try
  again after Straza lists the databases again, or ask an administrator to
  refresh the list.`
- `access.yaml does not list update_rows for support_bot. Use a listed
  tool, or ask an administrator to add it.`
- `access.yaml does not list sales_eu for support_bot. Use a database that
  list_databases returns, or ask an administrator to add it.`
- `access.yaml narrows support_bot to the tables public.tickets and
  audit.log, so it may not use public.customers on helpdesk. Name one of
  those tables, or ask an administrator to add public.customers.`
- `access.yaml narrows support_bot to tables, and query_sql can reach any
  table. Use a table tool such as read_rows, or ask an administrator to
  remove the tables of support_bot.`
- `list_schemas refused the call, because it came with a listing token,
  which works on list_databases only. Send a token whose SHA-256 is in
  gateway_tokens_sha256 instead.`
- `update_rows on helpdesk would change 1,240 rows, over the cap of 100,
  and changed nothing. Narrow the where filter, or ask an administrator to
  raise limits.write_rows in server.yaml.`
- `delete_rows on helpdesk would change 501 rows, over the cap of 100, and
  changed nothing. Triggers or cascades change rows in other tables, so
  narrow the call, or ask an administrator to raise limits.write_rows in
  server.yaml.`
- `execute_sql on helpdesk rolled back the statement with command tag
  TRUNCATE TABLE, which the write cap cannot count. Use execute_ddl for
  TRUNCATE and changes of structure.`
- `execute_sql on helpdesk rolled back the statement with command tag DO,
  because it changed the system catalogs. Use execute_ddl for TRUNCATE and
  changes of structure.`
- `execute_ddl on helpdesk rolled back the statement with command tag
  DELETE 1, because it changed no structure. Use execute_sql to change
  rows, or query_sql to read them.`
- `execute_ddl on helpdesk would change 150 rows, over the cap of 100, and
  changed nothing. Run row changes through execute_sql, or ask an
  administrator to raise limits.write_rows in server.yaml.`
- `close_tickets on helpdesk would change 50 rows, over the cap of 10, and
  changed nothing. Narrow the call through its arguments, or ask an
  administrator to raise limits.write_rows of close_tickets in tools.yaml.`
- `wipe_ledger on helpdesk rolled back the statement with command tag
  SELECT 1, because it changed the system catalogs. A catalog tool may
  change rows only. Ask an administrator to change the statement.`
- `the cursor of list_databases is not from this listing. Pass the
  nextCursor of the previous page unchanged, or leave cursor out for the
  first page.`

HTTP requests:

- `the request has no bearer token. Send the header Authorization: Bearer
  followed by the token issued for this server.`
- `the bearer token is malformed or unknown to this server. Send the token
  issued for this server, once, after Bearer.`
- `the request has an Origin header, so it comes from a web page, and web
  pages may not call this server. Use an MCP client that sends no Origin
  header.`

Loading server.yaml:

- `/etc/adaptive-pg-mcp/server.yaml does not set listen, a required key.
  Add listen: stdio, or an address such as 127.0.0.1:8080.`
- `/etc/adaptive-pg-mcp/server.yaml has listen 0.0.0.0:8080 without tls, so
  bearer tokens would cross the network in cleartext. Add tls, set listen
  to 127.0.0.1:8080, or set allow_plain_http: true if a proxy or service
  mesh encrypts the traffic.`
- `/etc/adaptive-pg-mcp/server.yaml sets tls, but listen is stdio. Remove
  tls, or set listen to the address to serve HTTPS on.`
- `/etc/adaptive-pg-mcp/server.yaml sets allow_plain_http: true, but listen
  is stdio. Remove allow_plain_http, or set listen to the address to serve
  HTTP on.`
- `/etc/adaptive-pg-mcp/server.yaml sets both tls and allow_plain_http:
  true, but tls serves HTTPS only. Remove allow_plain_http to serve HTTPS,
  or tls to serve plain HTTP.`
- `/etc/adaptive-pg-mcp/server.yaml sets tls without cert_file. Add
  tls.cert_file: the path to the PEM file of the certificate.`
- `/etc/adaptive-pg-mcp/server.yaml sets tls without key_file. Add
  tls.key_file: the path to the PEM file of the private key.`

Starting, before the checks:

- `ADAPTIVE_PG_MCP_TOKEN is not set, so adaptive-pg-mcp cannot tell who calls
  on stdio and will not start. Set ADAPTIVE_PG_MCP_TOKEN to the token of a
  caller in access.yaml,, in the env of the client's MCP server entry.`
- `the token in ADAPTIVE_PG_MCP_TOKEN matches no configured hash, so
  adaptive-pg-mcp will not start. Set ADAPTIVE_PG_MCP_TOKEN to the token of
  a caller in access.yaml.`
- `adaptive-pg-mcp will not start, because listen localhost:8080 resolved to
  192.0.2.10:8080, not a loopback address, and without tls the tokens would
  cross the network in cleartext. Make localhost resolve to 127.0.0.1, set
  listen to 127.0.0.1:8080, or set tls.cert_file and tls.key_file.`

Start checks of the logins:

- `the login of sales_eu is a superuser and free SQL is on. Use a login
  without superuser, turn free_sql off, or set allow_superuser: true for
  sales_eu.`
- `the login of sales_eu can switch to the superuser role postgres and free
  SQL is on. Use a login that is not a member of a superuser role, turn
  free_sql off, or set allow_superuser: true for sales_eu.`
- `sales_eu has dblink installed, which lets a call reach another database.
  Drop the extension from sales_eu, or set allow_cross_database: true for
  sales_eu.`
- `the write login of helpdesk reaches a database that has dblink
  installed, which lets a call reach another database. Drop the extension
  from that database, or set allow_cross_database: true for helpdesk.`
- `the write login of helpdesk reaches a different database than the read
  login of helpdesk. Point HELPDESK_WRITE_DSN and HELPDESK_READ_DSN at the
  same database.`
- `the write login of helpdesk cannot read the id of helpdesk, because
  Postgres said: permission denied for function pg_control_system (SQLSTATE
  42501). Without it, adaptive-pg-mcp cannot check that this login reaches
  the same database as the read login. Grant EXECUTE on pg_control_system()
  to the login, or restore its default grant to PUBLIC.`
- `the login of sales_eu cannot connect, because Postgres said: password
  authentication failed for user "sales" (SQLSTATE 28P01). Check the
  connection string in SALES_EU_DSN, and that Postgres is up and accepts
  the login.`
- `the connection string in HELPDESK_WRITE_DSN for the write login of
  helpdesk cannot be used, because it does not parse as a postgres:// URL
  or as keyword=value pairs. Correct it in the server's environment.`

Start checks of the catalog:

- `finance/revenue.sql statement revenue_by_region does not prepare on
  sales_us: Postgres said: column "net" does not exist (SQLSTATE 42703).
  Fix the statement, or remove sales_us from its databases.`
- `finance/revenue.sql statement revenue_of_region takes 2 parameters on
  sales_eu, but tools.yaml lists 1. List each parameter in params, in $1
  order.`
- `finance/revenue.sql statement tickets_since has parameter $1 (after) as
  int4 on sales_eu and as int8 on sales_us. Make the types match, or cast it
  in the statement, such as $1::int4.`
- `finance/revenue.sql statement tickets_by_ids has parameter $1 ids of
  Postgres type "_int4", which a tool argument cannot carry. Cast it in the
  statement to one of bool, bpchar, date, float4, float8, int2, int4, int8,
  json, jsonb, numeric, text, timestamp, timestamptz, uuid, varchar, such
  as $1::text.`
- `helpdesk/tickets.sql statement close_tickets needs a grant that the
  write login of helpdesk lacks: Postgres said: permission denied for table
  tickets (SQLSTATE 42501). Grant it, or remove helpdesk from its
  databases.`
- `helpdesk/tickets.sql statement ticket_count could not be planned on
  helpdesk: Postgres said: terminating connection due to administrator
  command (SQLSTATE 57P01). The grants of the read login of helpdesk cannot
  be checked without a plan. Fix the cause, and try again.`
- `finance/revenue.sql statement revenue_by_region on line 1 starts with a
  semicolon, which would split it from the EXPLAIN that checks its grants.
  Remove the semicolon.`
- `finance/tools.yaml has a second YAML document at line 11. Remove it,
  because adaptive-pg-mcp reads only the first.`

Start checks of TLS:

- `the certificate file /etc/adaptive-pg-mcp/server.crt (tls.cert_file)
  cannot be read: permission denied. Check that it exists and that the user
  the server runs as can read it.`
- `the key file /etc/adaptive-pg-mcp/server.key (tls.key_file) cannot be
  read: no such file or directory. Check that it exists and that the user
  the server runs as can read it.`
- `the certificate file /etc/adaptive-pg-mcp/server.crt holds no PEM
  certificate. Put the server certificate in it as PEM, followed by any
  intermediate certificates.`
- `the key file /etc/adaptive-pg-mcp/server.key holds no PEM private key.
  Put the certificate's private key in it as PEM.`
- `the first certificate in /etc/adaptive-pg-mcp/server.crt does not parse
  as an X.509 certificate. Replace it with the server certificate as
  issued.`
- `the private key in /etc/adaptive-pg-mcp/server.key does not parse as an
  RSA, ECDSA or Ed25519 key. Replace it with the PEM private key of the
  certificate in /etc/adaptive-pg-mcp/server.crt.`
- `the private key in /etc/adaptive-pg-mcp/server.key does not match the
  certificate in /etc/adaptive-pg-mcp/server.crt. Put a certificate and its
  own private key in the two files.`
- `the certificate in /etc/adaptive-pg-mcp/server.crt expired at
  2026-10-01T12:00:00Z. Renew it, and put the new certificate and key in
  /etc/adaptive-pg-mcp/server.crt and /etc/adaptive-pg-mcp/server.key.`

While it serves, a renewed pair that does not load is a warning, not a
refusal: `the private key in /etc/adaptive-pg-mcp/server.key does not match
the certificate in /etc/adaptive-pg-mcp/server.crt, so the server keeps
serving the previous certificate. Put a certificate and its own private key
in the two files.` Each TLS refusal above reads the same way as such a
warning, with `, so the server keeps serving the previous certificate` at
the end of its first sentence.

Reports, which do not refuse the start:

- `the login of sales_eu can write to all 212 tables, so the read tools rely
  on read-only transactions alone. A separate read login with only SELECT adds
  Postgres grants as a second check.`
- `helpdesk/tickets.sql statement close_month cannot be planned by EXPLAIN,
  so its grants are not checked. The tool is served, and its first call
  fails if the write login of helpdesk lacks a grant. Check its grants by
  hand.`
- `helpdesk/tickets.sql statement tickets_after does not plan on helpdesk
  with NULL parameters: Postgres said: the id must be set (SQLSTATE P0001).
  The tool is served, and its first call fails if the read login of
  helpdesk lacks a grant. Fix the statement if real calls fail the same
  way.`
- `finance/tools.yaml: the catalog tool revenue_by_region raises
  limits.rows to 5,000, above the 1,000 in server.yaml. Lower it if that is
  not intended.`
- `finance/revenue.sql statement revenue_draft is not a tool, because
  finance/tools.yaml does not list it. List it there to serve it.`
- `support_bot lists open_ticket in access.yaml, but no built-in or catalog
  tool has that name. Correct the name, or remove it from the tools of
  support_bot.`
- `listen 0.0.0.0:8080 serves plain HTTP beyond this machine, which
  allow_plain_http: true accepts. Bearer tokens cross the network in
  cleartext unless a proxy or service mesh encrypts the traffic. Set
  tls.cert_file and tls.key_file to serve HTTPS.`

`check` closes with a line such as `check: 1 refusal above, so
adaptive-pg-mcp would not start. Fix it, and run adaptive-pg-mcp check
again.`

## 8. Behind Straza

Straza needs no change to govern this server. The marks, the listing shape,
the object id and the required string argument follow Straza's object
marks reference. In straza mode, the server accepts only the tokens whose
hashes `gateway_tokens_sha256` and `listing_tokens_sha256` list. A gateway
token reaches every tool. A listing token, which Straza sends only to
`list_databases`, sees that tool alone in `tools/list` and is refused on
every other tool, as the marks reference asks of a server. Straza decides
which tools and which databases each agent may use, holds a call for
approval when its policy says so, and writes the audit record. The server
checks the id Straza sends, so a call on a database dropped and created
again under the same name is refused. In straza mode every request with a
gateway token has the caller `straza`, because Straza holds the identity of
the agent, and every request with a listing token has the caller
`straza-listing`. The two callers never share a session.

## 9. Tests

Tests are table-driven. Every package test that touches a database runs
against Postgres 14 to 18 in local Docker. Such a test carries the build
constraint `//go:build pg` and gets its servers from internal/pgtest. Plain
`go test ./...` in the commit gate needs no Docker, and `make pgtest` and
the push gate run `go test -tags pg ./...` against the five servers that
`sh tools/pg.sh up` starts. The authorization test runs on every protocol
revision the SDK serves.

## 10. Not in v1

Rows filtered per person, which needs a caller identity from Straza. Long
exports through MCP tasks. Write confirmation through elicitation in
standalone mode. Catalog reload without a restart. dbt models. Read
replicas. Databases other than Postgres.
