# adaptive-pg-mcp

An MCP server for PostgreSQL that is safe by construction. Every data call
names its database, the agent gets typed tools, and an organization's own
SQL becomes named tools with typed parameters. Free SQL runs under the grants
of the login each call uses. It works with any MCP client, and behind the
Straza gateway each agent gets only the tools and databases its roles allow.

Status: in design, nothing to run yet. The design is docs/DESIGN.md and the
plan is docs/PLAN.md.

Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
