# Changelog

## Unreleased

- Start adaptive-pg-mcp with its guardrails, design and plan. The design is the v1 contract the owner approved on 2026-10-28. The gates hold the server to the newest MCP revision and go-sdk release (tools/specgate.sh, revision 2026-07-28 and go-sdk v1.8.0 at the start), to its SQL and process boundaries (tools/gogate.sh), and to the secret and vulnerability scans at push.
- Add the Go module, internal/catalog and internal/core, with the seam between them: core.Tool turns a catalog.Entry into an MCP tool that passes the tool lint, or names what to fix in the entry. The seam is fixed first so that the core (M1) and the catalog (M2) can be built in separate folders against one type. The package names and the seam are the ones the owner approved on 2026-10-28.
- Add github.com/modelcontextprotocol/go-sdk v1.8.0, the official MCP SDK, which serves revision 2026-07-28. Its license is moving from MIT to Apache-2.0, and both are compatible with this project. govulncheck and osv-scanner find nothing in it.
- Add github.com/google/jsonschema-go v0.4.3 as a direct dependency, because core builds input and output schemas with it, and the go-sdk already requires it. It is MIT licensed. govulncheck and osv-scanner find nothing in it.
