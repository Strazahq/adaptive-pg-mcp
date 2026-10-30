# Security policy

## Supported versions

Security fixes go into the newest release. Versions before 1.0.0 get no
backports, so upgrade to the newest 0.x release to get a fix.

## Reporting a vulnerability

Report it privately through GitHub: open the Security tab of this
repository and choose Report a vulnerability. Do not open a public issue
for it.

Say what an attacker can do, which version and configuration it needs, and
the steps to reproduce it. A configuration with made-up databases and
tokens is enough. Never send a real connection string or token.

These count as vulnerabilities in this server:

- A request that passes the token check without a configured token, or a
  caller that reaches a tool, database or table that access.yaml does not
  give it.
- A listing token that reaches any tool other than `list_databases`.
- A read tool or `query_sql` that changes data, or a write over its cap
  that is not rolled back.
- A catalog tool whose arguments change its statement.
- A token, password or connection string in a log line, an error or a
  result.
- A call on a database that was dropped and created again under the same
  name that passes the id check.

What a login's own grants allow through free SQL is decided by Postgres,
not by this server. The section "What the server enforces" in README.md
draws that line.
