# Plan

## 1. What it is

Most Postgres MCP servers get one connection fixed at start and hand the
agent free SQL, so every agent holds the same rights and a gateway sees only
text. adaptive-pg-mcp names the database in every call, gives the agent typed
tools, and turns an organization's own reviewed SQL into named tools. Behind
Straza, a role decides which tools and which databases each agent may use, a
person can approve a write, and every call is audited with its database.
Without Straza, the server reads its own access file.

## 2. Decisions

Made by the owner on 2026-10-28:

1. The name is adaptive-pg-mcp, in the Strazahq organization, under
   Apache-2.0. The repository is private for now.
2. Write tools and free SQL are in v1. Behind Straza, a policy can hold
   `execute_sql` for a person's approval. Free SQL that changes rows goes
   through `execute_sql` under the write cap, and TRUNCATE and changes of
   structure go through `execute_ddl`, which has no row cap, so a policy can
   hold structure changes alone.
3. An organization's SQL stays in its own `.sql` files, and a small index
   says which statements become tools.
4. How an operator configures Straza, approval binding included, is the
   operator's call. The server does not check Straza settings.
5. One powerful login per database is the common setup and works. Separate
   read and write logins are the hardened option.
6. The server refuses to start in four cases only: a superuser with free SQL
   on, dblink or postgres_fdw installed, a login that reaches another
   database than its database's read login, and a catalog statement that
   fails. A member of a superuser role counts as a superuser. The first two
   each have one config key that accepts them. A login that cannot connect
   refuses under invariant 8. Everything else is reported.
7. Go on the official MCP go-sdk and pgx, Postgres 14 to 18, the newest MCP
   revision always (docs/MCP-REVISION, enforced by tools/specgate.sh).

## 3. Invariants

The gates cite these numbers.

1. Every data tool names its database in a required top-level string
   argument, and a call reaches only that database.
2. Every call is one prepared statement on the extended protocol, in its own
   transaction. Reads run in read-only transactions.
3. Values travel as bind parameters, and identifiers go through one quoting
   helper. Free SQL is the agent's own statement, sent as it is.
4. The limit of every call is the grants of the login it runs on. Logins are
   real logins, never roles to switch to.
5. Every request carries a bearer token that matches a configured hash,
   compared in constant time. Tokens come from crypto/rand. A caller's token
   never goes anywhere else.
6. The server never starts a program, and it reads only its config, its
   access file, its catalog, and its TLS certificate and key.
7. Every connection verifies its peer as its config asks, and code never
   switches a check off.
8. Fail closed. An unreadable config, catalog or check refuses to start, an
   id mismatch refuses the call, and a write over its cap rolls back.
9. Formats are additive. A change to `server.yaml`, `access.yaml` or the
   catalog index lands with a version bump, an example and a CHANGELOG line
   in the same commit.

## 4. Milestones

- M0: the Go module on the newest go-sdk, the package skeleton, and the seam
  between the catalog and the core. The catalog package loads entries and
  checks them against the databases, and the core turns entries into tools.
  The milestone session proposes the package names to the owner.
- M1: the core. Config, tokens, a pool per login, the start checks, the id
  check, the built-in tools and free SQL.
- M2: the catalog. The `.sql` reader, the index, `check`, and the input
  schema built from the parameter types Postgres reports.
- M3: the Postgres 14 to 18 harness in local Docker that every database test
  runs on.
- M4: the fire behind Straza, with the database scope, approval and the id
  check exercised through the gateway.
