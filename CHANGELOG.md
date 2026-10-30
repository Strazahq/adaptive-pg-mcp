# Changelog

All notable changes to this project are documented in this file.

The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- The `serve`, `check` and `token` commands. `check` runs the start checks
  without serving, so a catalog repository can run it in CI.
- Streamable HTTP at `/mcp` with a bearer token on every request, and stdio
  with the token in `ADAPTIVE_PG_MCP_TOKEN`, checked once at start.
- HTTPS with `tls`, which serves a renewed certificate without a restart.
  Plain HTTP beyond loopback needs `allow_plain_http: true`, for a proxy or
  service mesh that encrypts the traffic.
- Standalone mode, where access.yaml names the tools, databases and tables
  of each caller, and straza mode, where the Straza gateway decides per
  agent.
- `listing_tokens_sha256` in straza mode, for the token Straza sends only to
  `list_databases`. A listing token sees and calls that tool alone, so a
  leaked one cannot reach data, as Straza's object marks reference asks of
  a server.
- The built-in tools `list_schemas`, `list_tables`, `describe_table`,
  `read_rows`, `aggregate`, `insert_rows`, `update_rows` and `delete_rows`.
- Free SQL as `query_sql`, `execute_sql` and `execute_ddl`, split by what
  Postgres reports about the statement, never by its text, so a policy can
  hold changes of structure for approval on their own.
- Caps on the rows, bytes and time of each call and on the rows a write
  changes. A write over its cap rolls back, with triggers and cascades
  counted.
- The catalog, which turns reviewed SQL in `.sql` files into named tools
  with typed parameters through a tools.yaml per folder.
- Start checks that refuse to serve on a superuser login while free SQL is
  on, dblink or postgres_fdw, logins of one database that reach different
  databases, a login that cannot connect, or a TLS pair that does not load.
- A grants check of each catalog statement at start, through EXPLAIN on
  its own login, so a missing grant stops the start instead of failing the
  first call.
- The database id check, which refuses a call on a database dropped and
  created again under the same name until Straza lists it again.
- `list_databases`, which lists the configured databases with an id that
  joins the cluster's system identifier, the database OID and the config
  name.
- A container image at `ghcr.io/strazahq/adaptive-pg-mcp` for linux/amd64
  and linux/arm64, on distroless static as a non-root user.
- Docker Compose and Kubernetes examples. The Kubernetes one takes its TLS
  pair from cert-manager.
- The direct dependencies github.com/modelcontextprotocol/go-sdk v1.8.0
  (MIT and Apache-2.0), github.com/google/jsonschema-go v0.4.3 (MIT),
  github.com/jackc/pgx/v5 v5.11.0 (MIT) and go.yaml.in/yaml/v3 v3.0.5 (MIT
  and Apache-2.0). govulncheck and osv-scanner find no known vulnerability
  in them.
