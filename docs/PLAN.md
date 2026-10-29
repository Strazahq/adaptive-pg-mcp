# Plan

## 1. What it is

Most Postgres MCP servers get one connection fixed at start and hand the
agent free SQL, so every agent holds the same rights and a gateway sees only
text. adaptive-pg-mcp names the database in every call, gives the agent
typed tools, and turns an organization's own reviewed SQL into named tools.
Behind Straza, a role decides which tools and which databases each agent
may use, a person can approve a write, and every call is audited with its
database. Without Straza, the server reads its own access file.

## 2. Decisions

1. The project is adaptive-pg-mcp, at github.com/strazahq/adaptive-pg-mcp,
   under the Apache-2.0 license.
2. Write tools and free SQL are in v1. Free SQL is split by what a statement
   does: `query_sql` reads, `execute_sql` changes rows under the write cap,
   and `execute_ddl` runs TRUNCATE and changes of structure. Behind Straza,
   a policy can then hold `execute_sql` or `execute_ddl` alone for a
   person's approval.
3. An organization's SQL stays in its own `.sql` files, and a small index,
   tools.yaml, says which statements become tools.
4. How an operator configures Straza, approval binding included, is up to
   the operator. The server does not check Straza settings.
5. One login per database with broad grants is the common setup and works.
   Separate read and write logins are the hardened option.
6. The server refuses to start when a login breaks the promise of one
   database per call, such as a superuser while free SQL is on, when a
   catalog statement would fail, and when a check cannot run. Everything
   else is reported. docs/DESIGN.md section 6 holds the list.
7. Go on the official MCP go-sdk and pgx, Postgres 14 to 18, and the newest
   MCP revision (docs/MCP-REVISION, enforced by tools/specgate.sh).

## 3. Invariants

The gates cite these numbers.

1. Every data tool names its database in a required top-level string
   argument, and a call reaches only that database.
2. Every statement goes to Postgres as one unnamed prepared statement on
   the extended protocol, never on the simple query protocol. Each call runs
   in a transaction of its own, and reads run in read-only transactions.
3. Values travel as bind parameters, and identifiers go through one quoting
   helper. Free SQL is the agent's own statement, sent as it is.
4. A call can do only what the grants of its login allow. Logins are real
   logins, never roles to switch to.
5. Every request carries a bearer token that matches a configured hash,
   compared in constant time. Tokens come from crypto/rand. A caller's token
   never goes anywhere else.
6. The server never starts a program, and it reads only its config, its
   access file, its catalog, and its TLS certificate and key.
7. Every connection verifies its peer as its config asks, and code never
   switches a check off.
8. Fail closed. An unreadable config or catalog, or a check that cannot
   run, refuses the start, an id mismatch refuses the call, and a write over
   its cap rolls back.
9. server.yaml, access.yaml and tools.yaml are at version 1 and may still
   change until the first release. After it, a format changes only by
   adding, and each change lands with a version bump, an example and a
   CHANGELOG line in the same commit.

## 4. Milestones

- M0: the Go module on the newest go-sdk, the package skeleton, and the
  seam between the catalog and the core. The catalog package loads
  statements and checks them against the databases, and the core turns
  them into tools.
- M1: the core. Config, tokens, a pool per login, the start checks, the id
  check, the built-in tools and free SQL.
- M2: the catalog. The `.sql` reader, tools.yaml, `check`, and the input
  schema built from the parameter types Postgres reports.
- M3: the Postgres 14 to 18 harness in local Docker that every database
  test runs on.
- M4: a run behind Straza, with the database scope, approval and the id
  check exercised through the gateway.
