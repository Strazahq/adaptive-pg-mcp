# adaptive-pg-mcp

An MCP server for PostgreSQL. Agents get typed tools on named databases, an
organization's reviewed SQL as named tools with typed parameters, and free
SQL that runs under the grants of the login each call uses. It is one static
Go binary that serves Streamable HTTP and stdio to any MCP client. Behind the
Straza gateway, each agent gets only the tools and databases its policy
allows, with approval holds and an audit record per call, and the server
needs no change for it.

Postgres 14 to 18 are supported. The server targets MCP revision 2026-07-28
and also serves the earlier revisions that the official Go SDK serves.

No release is tagged yet, and the configuration format may still change
within version 1 until the first one. The design is
[docs/DESIGN.md](docs/DESIGN.md).

- [What the server enforces](#what-the-server-enforces)
- [Quick start](#quick-start)
- [Install](#install)
- [Connect a client](#connect-a-client)
- [Configuration](#configuration)
- [Tools](#tools)
- [Start checks](#start-checks)
- [TLS](#tls)
- [Behind the Straza gateway](#behind-the-straza-gateway)
- [Docker and Kubernetes](#docker-and-kubernetes)
- [Development](#development)

## What the server enforces

On every call:

- A call names one database from server.yaml and runs on a login of that
  database.
- Each statement goes to Postgres as one unnamed prepared statement on the
  extended protocol, and each call runs in one transaction, with
  `statement_timeout` set from the configured caps. A text that holds two
  statements is refused.
- Read tools and `query_sql` run in a `READ ONLY` transaction. A read stops
  at the row cap or the byte cap and returns `truncated: true` when more rows
  follow.
- A write that changes more rows than `write_rows` is rolled back. The count
  includes rows changed by triggers, cascades, functions and `WITH` queries.
  With `track_counts` off, every write is refused, because the count cannot
  be read.
- `execute_sql` and `execute_ddl` tell row changes from structure changes by
  what Postgres reports: the command tag and the rows changed in the system
  catalogs. The server never parses the SQL text, so a `CREATE TABLE` inside
  a `DO` block is rolled back in `execute_sql` like a plain one.
- Free SQL and catalog tools close their connection after each call, so
  session state such as a `SET` or an advisory lock never reaches the next
  call.
- The table tools quote every identifier and send every value as a bind
  parameter.
- A cancelled call cancels its statement in Postgres.

On every request:

- Over HTTP, the server compares the SHA-256 of each request's bearer token
  with the configured hashes, in constant time. A protocol session id is
  never taken as proof of who calls.
- A request with an `Origin` header is refused, because the server serves no
  web page. A request body over 1 MiB is refused.
- In standalone mode, access.yaml decides which tools, databases and tables
  each caller may use, and `tools/list` shows each caller only its own tools.
- Without `tls`, HTTP listens on a loopback address only, unless
  `allow_plain_http: true` says that something in front encrypts the
  traffic.
- No token or connection string appears in a log line or an error. The
  caller's token is never sent to Postgres or anywhere else.

Before it serves, the [start checks](#start-checks) refuse a login that is a
superuser while free SQL is on, a database with dblink or postgres_fdw
installed, logins of one database that reach different databases, and a
catalog statement that does not prepare or whose login lacks a grant it
needs.

What it leaves to Postgres:

- The grants of each login decide what it may read and change. Free SQL
  runs with every grant the login holds on its database. A `READ ONLY`
  transaction stops writes to tables, but a function the login may execute
  still runs. For a hard boundary, give reads a login with `SELECT` only,
  and give the write login only the grants its writes need.
- Row-level security and column privileges apply to every call, because
  every statement runs as the login. The server does not filter rows per
  person.

## Quick start

The compose example starts Postgres 18 with a `helpdesk` database, a read
login and a write login, and the server in front of it, built from this
repository.

```sh
git clone https://github.com/strazahq/adaptive-pg-mcp
cd adaptive-pg-mcp/examples/compose
docker compose up --build
```

The server listens on `http://127.0.0.1:8080/mcp` and accepts the bearer
token `demo-token`. Add it to Claude Code:

```sh
claude mcp add --transport http helpdesk-demo http://127.0.0.1:8080/mcp \
  --header "Authorization: Bearer demo-token"
```

Or check that it responds:

```sh
token=demo-token
curl -s http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
```

The demo serves every tool, with `free_sql: read-write`, and the two catalog
tools of [examples/catalog](examples/catalog). It serves plain HTTP inside
the compose network and publishes the port on 127.0.0.1 only. The passwords
and the token are for the demo only. `docker compose down -v` removes the
containers and the data.

## Install

### Container image

```sh
docker pull ghcr.io/strazahq/adaptive-pg-mcp:0.1.0
```

The image holds the static binary on distroless static and runs as the
nonroot user, uid 65532. Its default command is
`serve -config /etc/adaptive-pg-mcp/server.yaml`. It is built for
linux/amd64 and linux/arm64. A release tag `vX.Y.Z` publishes `X.Y.Z`, and
each push to main publishes `main` and `sha-<commit>`. Each image published
from the public repository carries a signed build provenance attestation:

```sh
gh attestation verify oci://ghcr.io/strazahq/adaptive-pg-mcp:0.1.0 --owner strazahq
```

### go install

```sh
GOTOOLCHAIN=go1.27.3 go install github.com/strazahq/adaptive-pg-mcp/cmd/adaptive-pg-mcp@latest
```

`go install` builds with the Go you run and ignores the toolchain that
go.mod pins. Earlier releases, such as Go 1.26.5, carry standard library
vulnerabilities that govulncheck finds on the server's code paths, so set
`GOTOOLCHAIN` as above, or run Go 1.27.3 or newer.

### From source

```sh
git clone https://github.com/strazahq/adaptive-pg-mcp
cd adaptive-pg-mcp
CGO_ENABLED=0 go build -trimpath -o adaptive-pg-mcp ./cmd/adaptive-pg-mcp
```

Inside the module, the go command fetches the toolchain that go.mod pins
when the installed Go is older. `docker build -t adaptive-pg-mcp .` builds
the image.

## Connect a client

Over Streamable HTTP, the endpoint is `/mcp`, and each request sends
`Authorization: Bearer <token>`. Over stdio, the client starts
`adaptive-pg-mcp serve` itself, with a server.yaml whose `listen` is
`stdio`, the token in `ADAPTIVE_PG_MCP_TOKEN` and each connection string in
the variable that server.yaml names. `serve` checks the token once at start
and does not start without a match.

A server.yaml for stdio:

```yaml
version: 1
mode: standalone
listen: stdio
free_sql: read
limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }
access: ./access.yaml
catalog: none
databases:
  helpdesk:
    logins:
      read:  { dsn_env: HELPDESK_READ_DSN }
      write: { dsn_env: HELPDESK_WRITE_DSN }
```

The examples below use `https://mcp.example.internal:8443/mcp` for HTTP and
`/etc/adaptive-pg-mcp/stdio.yaml` for stdio. The client machine must trust
the CA that signed the server's certificate.

<details>
<summary>Claude Code</summary>

HTTP:

```sh
claude mcp add --transport http adaptive-pg-mcp https://mcp.example.internal:8443/mcp \
  --header "Authorization: Bearer $ADAPTIVE_PG_MCP_TOKEN"
```

stdio:

```sh
claude mcp add --env ADAPTIVE_PG_MCP_TOKEN="$ADAPTIVE_PG_MCP_TOKEN" \
  --env HELPDESK_READ_DSN="$HELPDESK_READ_DSN" \
  --env HELPDESK_WRITE_DSN="$HELPDESK_WRITE_DSN" \
  --transport stdio adaptive-pg-mcp \
  -- adaptive-pg-mcp serve -config /etc/adaptive-pg-mcp/stdio.yaml
```

Both commands write the values into the Claude Code config. A project
`.mcp.json` can read them from the environment instead:

```json
{
  "mcpServers": {
    "adaptive-pg-mcp": {
      "type": "http",
      "url": "https://mcp.example.internal:8443/mcp",
      "headers": {
        "Authorization": "Bearer ${ADAPTIVE_PG_MCP_TOKEN}"
      }
    }
  }
}
```

</details>

<details>
<summary>Claude Desktop</summary>

Claude Desktop connects to remote servers from Anthropic's cloud, which
cannot reach a server on your machine or a private network. Run the server
over stdio from `claude_desktop_config.json`, which is
`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS
and `%APPDATA%\Claude\claude_desktop_config.json` on Windows:

```json
{
  "mcpServers": {
    "adaptive-pg-mcp": {
      "command": "/usr/local/bin/adaptive-pg-mcp",
      "args": ["serve", "-config", "/etc/adaptive-pg-mcp/stdio.yaml"],
      "env": {
        "ADAPTIVE_PG_MCP_TOKEN": "apm_...",
        "HELPDESK_READ_DSN": "postgres://helpdesk_read:...@pg.example.internal:5432/helpdesk?sslmode=verify-full",
        "HELPDESK_WRITE_DSN": "postgres://helpdesk_write:...@pg.example.internal:5432/helpdesk?sslmode=verify-full"
      }
    }
  }
}
```

The file then holds the token and the passwords, so keep it readable by you
alone. Restart Claude Desktop after a change.

</details>

<details>
<summary>VS Code</summary>

Put this in `.vscode/mcp.json`, or in the user configuration that
**MCP: Open User Configuration** opens. VS Code prompts for the token when
the server first starts.

```json
{
  "inputs": [
    {
      "type": "promptString",
      "id": "adaptive-pg-mcp-token",
      "description": "adaptive-pg-mcp token",
      "password": true
    }
  ],
  "servers": {
    "adaptive-pg-mcp": {
      "type": "http",
      "url": "https://mcp.example.internal:8443/mcp",
      "headers": {
        "Authorization": "Bearer ${input:adaptive-pg-mcp-token}"
      }
    },
    "adaptive-pg-mcp-stdio": {
      "type": "stdio",
      "command": "adaptive-pg-mcp",
      "args": ["serve", "-config", "/etc/adaptive-pg-mcp/stdio.yaml"],
      "env": {
        "ADAPTIVE_PG_MCP_TOKEN": "${input:adaptive-pg-mcp-token}"
      },
      "envFile": "/etc/adaptive-pg-mcp/dsn.env"
    }
  }
}
```

`envFile` names a file of `NAME=value` lines that holds the connection
strings.

</details>

<details>
<summary>Cursor</summary>

Put this in `.cursor/mcp.json` of the project, or in `~/.cursor/mcp.json`
for every project. `${env:NAME}` reads a variable from the environment Cursor runs in.

```json
{
  "mcpServers": {
    "adaptive-pg-mcp": {
      "url": "https://mcp.example.internal:8443/mcp",
      "headers": {
        "Authorization": "Bearer ${env:ADAPTIVE_PG_MCP_TOKEN}"
      }
    },
    "adaptive-pg-mcp-stdio": {
      "type": "stdio",
      "command": "adaptive-pg-mcp",
      "args": ["serve", "-config", "/etc/adaptive-pg-mcp/stdio.yaml"],
      "env": {
        "ADAPTIVE_PG_MCP_TOKEN": "${env:ADAPTIVE_PG_MCP_TOKEN}",
        "HELPDESK_READ_DSN": "${env:HELPDESK_READ_DSN}",
        "HELPDESK_WRITE_DSN": "${env:HELPDESK_WRITE_DSN}"
      }
    }
  }
}
```

</details>

## Configuration

The server reads server.yaml, access.yaml in standalone mode, and the
catalog. Each YAML file has `version: 1`. An unknown key, a missing required
key, a value the server cannot use or a second YAML document fails the load
with a sentence that names the file and what to fix.
[docs/DESIGN.md](docs/DESIGN.md) section 3 holds every rule, and
[examples/](examples) holds files that the tests load.

### server.yaml

```yaml
version: 1
mode: standalone
listen: 127.0.0.1:8080
free_sql: read
limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }
access: ./access.yaml
catalog: ./catalog
databases:
  sales_eu:
    login: { dsn_env: SALES_EU_DSN }
  helpdesk:
    logins:
      read:  { dsn_env: HELPDESK_READ_DSN }
      write: { dsn_env: HELPDESK_WRITE_DSN }
```

| Key | Required | What it sets |
|---|---|---|
| `version` | yes | The format version, 1. |
| `mode` | yes | `standalone` reads its callers from access.yaml. `straza` accepts only the tokens the Straza gateway holds. |
| `listen` | yes | `stdio`, or the host and port of Streamable HTTP, such as `127.0.0.1:8080`. |
| `tls` | no | `cert_file` and `key_file`, the PEM files that HTTPS serves. See [TLS](#tls). |
| `allow_plain_http` | no | `true` accepts plain HTTP on an address beyond this machine. Defaults to false. |
| `free_sql` | yes | `off`, `read` for `query_sql`, or `read-write` for `query_sql`, `execute_sql` and `execute_ddl`. |
| `limits` | yes | `rows` and `bytes` cap what a read returns, `timeout` is the `statement_timeout` of each call, and `write_rows` caps the rows a write may change. |
| `access` | standalone mode | The path of access.yaml. Refused in straza mode. |
| `gateway_tokens_sha256` | straza mode | The SHA-256 of each token Straza calls with. Refused in standalone mode. |
| `listing_tokens_sha256` | no | The SHA-256 of each token Straza sends only to `list_databases`, which is all such a token can call. Refused in standalone mode. |
| `catalog` | yes | The catalog folder, or `none`. |
| `databases` | yes | Each database under the name that calls use, with its logins. |

Relative paths resolve against the folder of server.yaml. `serve` and
`check` read `./server.yaml` unless `-config` names another file.

A database has one `login`, or `logins` with `read`, `write` and any extra
login that a catalog tool names, such as `finance_reader`. Each login names
the environment variable that holds its connection string in `dsn_env`, so
no password sits in the file. A connection string takes either libpq form,
a URL or `key=value` pairs, such as
`postgres://helpdesk_read@pg.example.internal:5432/helpdesk?sslmode=verify-full`.
Logins are real logins, never roles to switch to, because free SQL can
switch roles. One login for reads and writes works: reads still run in
read-only transactions. Separate logins add the database's own grants as a
second boundary.

Two keys per database relax a start check:

- `allow_cross_database: true` accepts dblink or postgres_fdw in the
  database.
- `allow_superuser: true` accepts a superuser login while free SQL is on.

### access.yaml

In standalone mode, access.yaml lists the callers:

```yaml
version: 1
callers:
  - name: support_bot
    token_sha256: "<64 lowercase hex characters>"
    tools: [list_databases, list_tables, describe_table, read_rows, insert_rows]
    databases: [helpdesk]
    tables: [public.tickets]   # optional
```

`tools` and `databases` name what the caller may use. `tables` narrows the
table tools to those tables, and a caller with `tables` may not use free
SQL, because free SQL can reach any table. A catalog tool is listed in
`tools` by its name.

`adaptive-pg-mcp token` prints a new token and its hash:

```text
token: apm_...
token_sha256: 3f0c...
```

The token goes to the caller, and the hash goes into `token_sha256`. The
server keeps neither, so the token shows only once.

### The catalog

The catalog turns reviewed SQL into tools. It is a folder of `.sql` files,
with one `tools.yaml` index per folder, at any depth. A statement starts at
a `-- name:` line:

```sql
-- name: close_stale_tickets
UPDATE public.tickets
SET status = 'closed'
WHERE status = 'pending'
  AND updated_on < current_date - $1::int4
RETURNING id;
```

The index of its folder makes it a tool:

```yaml
version: 1
tools:
  close_stale_tickets:
    description: Closes the tickets that have waited in pending for more than a number of days. Returns the id of each ticket it closed.
    mode: write                    # or read
    databases: [helpdesk]
    login: write                   # read, write, or an extra login
    params:                        # in $1 order
      - name: days
        description: How many days a ticket waits in pending before it closes, from 7 to 90.
        required: true             # also enum, pattern, minimum, maximum, maxLength
        minimum: 7
        maximum: 90
    limits: { write_rows: 50 }     # optional: rows, bytes, timeout, write_rows
```

Postgres reports the type of each parameter when the server starts, and the
tool's input schema follows it. A parameter may be of type bool, int2, int4,
int8, float4, float8, numeric, text, varchar, bpchar, uuid, date, timestamp,
timestamptz, json or jsonb, or of a domain over one of them. A parameter
that a call leaves out binds NULL. A read tool runs in a read-only
transaction, and a write tool is rolled back when it changes more rows than
its cap or changes structure. Each field of an entry's `limits` replaces
the same field of server.yaml for that tool, and the start reports each cap
an entry raises.

The catalog skips files and folders whose name starts with a dot, so it
loads from a Kubernetes ConfigMap mount. It follows a link only when the
link is relative and stays inside the catalog folder.

#### Writing catalog tools

Agents never read this README. When a client connects, it fetches the tool
list, and the agent picks a tool by its name, its `description`, the
`description`, type and limits of each parameter, and whether it reads or
writes. The `description` in tools.yaml is the agent's documentation, so
say what the tool does, what it returns and its caps, in words an agent can
act on. The start refuses a tool or a parameter without a description.

Each tool is exactly one SQL statement, because the server prepares it as
one statement. A script of several statements, such as one wrapped in
`BEGIN` and `COMMIT`, or a psql command such as `\copy` does not prepare,
and the start refuses it. Parameters are `$1`, `$2` and so on, in the order
of `params`. psql variables such as `:month` do not work. A parameter of a
type outside the list above needs a cast in the statement, such as
`$1::text`.

One `.sql` file may hold many statements, each from its `-- name:` line to
the next one. A statement that no tools.yaml lists is not a tool, and the
start reports it. A tools.yaml entry without a statement of its name
refuses the start.

The server reads the catalog once, at start, so a changed file takes effect
after a restart. `adaptive-pg-mcp check` prepares every statement on its
databases and runs the grants check without serving, so CI can run it
against a staging database before a deploy.

## Tools

Every tool except `list_databases` takes a required `database` argument,
whose schema lists the databases the caller may use. Where a tool takes a
table, `schema` defaults to `public`. Results carry `structuredContent` that
matches the tool's output schema, and a short text. A bad argument or a
refusal returns a tool result with `isError: true` and a sentence that says
what to change.

| Tool | What it does | Login | Kind |
|---|---|---|---|
| `list_databases` | Lists the configured databases with their ids, 25 per page. | read | read |
| `list_schemas` | Lists the schemas the login may use, up to 100 per page. | read | read |
| `list_tables` | Lists the tables, views and foreign tables the login may use, in one schema or all, up to 100 per page. | read | read |
| `describe_table` | Returns the columns of a table or view: name, type, nullable, default and primary key. | read | read |
| `read_rows` | Returns rows, with a column list, `where` filters, `order_by` and `limit`. | read | read |
| `aggregate` | Returns `count`, `sum`, `avg`, `min` or `max` of a column, with filters and `group_by`. | read | read |
| `insert_rows` | Inserts rows and returns `rowsAffected` and the `returning` columns. | write | write |
| `update_rows` | Updates the rows a required `where` matches. | write | write |
| `delete_rows` | Deletes the rows a required `where` matches. | write | write |
| `query_sql` | Runs one SQL statement in a read-only transaction. Needs `free_sql: read` or `read-write`. | read | read |
| `execute_sql` | Runs one statement that changes rows, such as `INSERT`, `UPDATE`, `MERGE` or `CALL`, under the write cap. Refuses `TRUNCATE` and changes of structure. Needs `free_sql: read-write`. | write | write |
| `execute_ddl` | Runs `TRUNCATE` or one statement that changes structure. Refuses a statement that changes rows only. Needs `free_sql: read-write`. | write | write |
| one per catalog statement | Runs the statement with the call's arguments as its parameters. | its entry's | its entry's |

A `where` filter is a list of `{column, op, value}`, with `op` one of `=`,
`<>`, `<`, `<=`, `>`, `>=`, `in`, `not in`, `is null`, `is not null`,
`like` and `ilike`. A read returns `columns` with their Postgres types,
`rows` as arrays in column order, and `truncated`. Numbers and booleans are
JSON numbers and booleans, json is JSON, dates and timestamps are ISO 8601
with timestamptz in UTC, and other types are their Postgres text.

Read tools set `readOnlyHint`, and write tools set `destructiveHint`.

## Start checks

`serve` runs the start checks before it serves, and `check` runs them
without serving. They connect every login, and these findings refuse the
start:

- A login is a superuser, or a member of a superuser role, while free SQL
  is on, and its database does not set `allow_superuser: true`.
- A database that one of its logins reaches has dblink or postgres_fdw
  installed, and the database does not set `allow_cross_database: true`.
- A login of a database reaches another database than its read login,
  compared by the cluster's system identifier and the database OID, or the
  two cannot be compared.
- A catalog statement does not prepare on one of its databases, takes a
  different number of parameters than its index lists, needs a grant that
  its login lacks, fails to plan for a reason that its NULL parameters
  cannot cause, or has a parameter whose type differs between two of its
  databases.
- A login cannot connect, the catalog does not load, or the TLS certificate
  and key do not load or the certificate has expired.

The grant check prepares each catalog statement and plans it with
`EXPLAIN (ANALYZE false)` and NULL for each parameter, on its entry's login
in a read-only transaction that is rolled back. It runs no statement. A few
grants show only on the first call, such as `USAGE` on a sequence that a
column default calls.

Everything else is reported: what each login can do, such as writing to
every table or holding `BYPASSRLS`, a statement whose grants EXPLAIN cannot
check, each cap a catalog entry raises, and each tool in access.yaml that
the server does not serve.

```text
$ adaptive-pg-mcp check -config /etc/adaptive-pg-mcp/server.yaml
report: ...
check: no refusals, so adaptive-pg-mcp would start.
```

`check` prints one `refusal:` or `report:` line per finding and a closing
`check:` line, and exits 1 when `serve` would refuse to start. It needs the
same connection strings as `serve`, so a catalog repository can run it in
its CI against the real databases. `serve` logs refusals at ERROR and
reports at WARN, on stderr.

## TLS

With `tls`, Streamable HTTP serves HTTPS only:

```yaml
listen: 0.0.0.0:8443
tls:
  cert_file: /etc/adaptive-pg-mcp/tls/tls.crt   # the certificate, then any intermediates
  key_file: /etc/adaptive-pg-mcp/tls/tls.key
```

It accepts TLS 1.2 and later with Go's default cipher suites, offers HTTP/2
and HTTP/1.1, and asks for no client certificate. A pair that does not load,
or a certificate that has expired, refuses the start. While it serves, the
server reads both files again at a handshake once a minute has passed since
the last read. A tool such as cert-manager that replaces them renews the
pair without a restart, and a new pair that does not load leaves the old
one in service with a warning.

Without `tls`, `listen` must be localhost or a loopback address. Any other
address, `0.0.0.0` included, fails the load unless `allow_plain_http: true`
says that something in front of the server encrypts the traffic, such as a
service mesh. The start then reports that bearer tokens cross the network
in cleartext.

## Behind the Straza gateway

In straza mode the server trusts the gateway for identity and policy:

```yaml
mode: straza
gateway_tokens_sha256:
  - "<SHA-256 of the token Straza calls with>"
listing_tokens_sha256:
  - "<SHA-256 of the token Straza lists with>"
```

The server accepts only those tokens. A gateway token reaches every tool,
and every request with it has the caller `straza`. A listing token sees and
calls `list_databases` only, so a leaked one cannot read or change data. A
hash in both lists fails the load, and a listing token whose hash is in
`gateway_tokens_sha256` alone would reach every tool, so keep it in
`listing_tokens_sha256`. Straza decides which tools and which databases each agent may
use, holds a call for approval when its policy says so, and writes the
audit record. Each data tool carries the mark
`ai.straza/object: {type: database, argument: database}`, and
`list_databases` lists the databases for Straza with an id that joins the
cluster's system identifier, the database OID and the config name. When a
call's `_meta` carries `ai.straza/object-id`, the server compares it with
the id read on the call's own connection and refuses a mismatch, so a call
on a database dropped and created again under the same name is refused
until Straza lists the databases again. The configuration of Straza itself
is in its own documentation.

## Docker and Kubernetes

The image reads `/etc/adaptive-pg-mcp/server.yaml`. Mount a folder that
holds server.yaml, access.yaml and the catalog there, and pass each
connection string as an environment variable. Inside a container, `listen`
needs an address that the host can reach, such as `0.0.0.0:8443`, and with
it `tls` or `allow_plain_http: true`.

```sh
docker run --rm -p 127.0.0.1:8443:8443 \
  -v /srv/adaptive-pg-mcp:/etc/adaptive-pg-mcp:ro \
  -e HELPDESK_READ_DSN -e HELPDESK_WRITE_DSN \
  ghcr.io/strazahq/adaptive-pg-mcp:0.1.0
```

The same image makes tokens, runs the checks, and serves stdio for a client
that starts it:

```sh
docker run --rm ghcr.io/strazahq/adaptive-pg-mcp:0.1.0 token
docker run --rm -v /srv/adaptive-pg-mcp:/etc/adaptive-pg-mcp:ro \
  -e HELPDESK_READ_DSN -e HELPDESK_WRITE_DSN \
  ghcr.io/strazahq/adaptive-pg-mcp:0.1.0 check -config /etc/adaptive-pg-mcp/server.yaml
docker run -i --rm -v /srv/adaptive-pg-mcp:/etc/adaptive-pg-mcp:ro \
  -e ADAPTIVE_PG_MCP_TOKEN -e HELPDESK_READ_DSN -e HELPDESK_WRITE_DSN \
  ghcr.io/strazahq/adaptive-pg-mcp:0.1.0 serve -config /etc/adaptive-pg-mcp/stdio.yaml
```

[examples/kubernetes](examples/kubernetes) holds manifests for
`kubectl apply -k`. They need cert-manager and an issuer.

- The Deployment runs one replica as uid 65532, with a read-only root file
  system, no privilege escalation, every capability dropped and the
  RuntimeDefault seccomp profile, which the restricted Pod Security Standard
  of its namespace enforces.
- server.yaml and access.yaml, and each catalog folder, become ConfigMaps
  whose names carry a hash of their content, so a change rolls the
  Deployment. The server reads them only at start.
- A projected volume puts the ConfigMaps and the cert-manager Secret into
  one folder at the paths server.yaml names. cert-manager renews the pair in
  place, and the server serves the new pair without a restart.
- The connection strings come from a Secret through the environment. The
  Secret, and the pull secret while the image is private, are created
  outside the manifests, as kustomization.yaml shows.
- The probes open a TCP connection, because every HTTP request needs a
  token.
- Clients on revisions before 2026-07-28 keep a protocol session in the
  memory of one replica, so more replicas need session affinity in front.

A test loads the example's server.yaml, access.yaml and catalog, and checks
that the manifests deliver each connection string and catalog file.

## Development

```sh
make hooks    # bind the git hooks in .githooks, once per clone
make check    # the commit gate: text, state and Go gates, gofmt, go vet, go test
make pgtest   # the database tests on Postgres 14 to 18 in local Docker
make security # gitleaks, govulncheck, osv-scanner and the spec gate
```

`go test ./...` needs no database. Tests that touch one carry the build tag
`pg`, and `make pgtest` runs them against one container per major version,
which `tools/pg.sh` starts and removes. The push gate runs the security
scans and the database tests. The workflow in
[.github/workflows/image.yml](.github/workflows/image.yml) runs `go vet`,
`go test` and govulncheck, and then builds and publishes the image.

[docs/DESIGN.md](docs/DESIGN.md) is the design, [docs/PLAN.md](docs/PLAN.md)
holds the decisions and invariants, and [CHANGELOG.md](CHANGELOG.md) records
each change.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
