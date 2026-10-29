package core

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// The names of the built-in data tools, as docs/DESIGN.md section 4 gives them.
const (
	listSchemasName   = "list_schemas"
	listTablesName    = "list_tables"
	describeTableName = "describe_table"
	readRowsName      = "read_rows"
	aggregateName     = "aggregate"
	insertRowsName    = "insert_rows"
	updateRowsName    = "update_rows"
	deleteRowsName    = "delete_rows"
	querySQLName      = "query_sql"
	executeSQLName    = "execute_sql"
	executeDDLName    = "execute_ddl"
)

// dataTool is one data tool: what tools/list shows, and how its calls run.
type dataTool struct {
	tool *mcp.Tool
	// write is true when the tool runs read-write, and false when it runs
	// read-only.
	write bool
	// free is true for the free SQL tools, which a caller narrowed to tables
	// may not use.
	free bool
	// login is the login the tool runs on, by its name in logins, such as
	// finance_reader. Empty means read for a read tool and write for a write
	// tool.
	login string
	// limits are the caps of the tool's calls, or nil for the caps in
	// server.yaml.
	limits *config.Limits
	// fromCatalog is true for a catalog tool, whose caps are set in its limits
	// in tools.yaml.
	fromCatalog bool
	// run runs a call whose arguments match the input schema.
	run func(ctx context.Context, c *call, args map[string]any) *mcp.CallToolResult
}

// addDataTools adds the built-in data tools: the read and write tools
// always, query_sql when free_sql is read or read-write, and execute_sql
// and execute_ddl when it is read-write.
func (s *Server) addDataTools() {
	tools := []*dataTool{
		s.listSchemasTool(), s.listTablesTool(), s.describeTableTool(), s.readRowsTool(), s.aggregateTool(),
		s.insertRowsTool(), s.updateRowsTool(), s.deleteRowsTool(),
	}
	if s.cfg.FreeSQL == config.FreeSQLRead || s.cfg.FreeSQL == config.FreeSQLReadWrite {
		tools = append(tools, s.querySQLTool())
	}
	if s.cfg.FreeSQL == config.FreeSQLReadWrite {
		tools = append(tools, s.executeSQLTool(), s.executeDDLTool())
	}
	for _, d := range tools {
		s.addTool(d)
	}
}

// addTool serves a data tool, built-in or from the catalog. Every call
// passes the checks of begin before its statement runs.
func (s *Server) addTool(d *dataTool) {
	s.mcp.AddTool(d.tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, args, refused := s.begin(ctx, req, d)
		if refused != nil {
			return refused, nil
		}
		return d.run(ctx, c, args), nil
	})
}

// call is one call of a data tool that passed the checks before its statement.
type call struct {
	s        *Server
	tool     string
	caller   string
	database string
	meta     mcp.Meta
	write    bool
	// login is the login the call runs on, by its name in logins.
	login string
	// limits are the caps of the call, from server.yaml or from the catalog
	// tool.
	limits config.Limits
	// fromCatalog is true for a call of a catalog tool.
	fromCatalog bool
	// typed and untyped are the columns of a read that stopped at its cap,
	// and those whose type names are read after its transaction.
	typed   []resultColumn
	untyped []untypedColumn
}

// begin runs steps 2 and 3 of docs/DESIGN.md section 5. In standalone mode
// it checks that the caller may use the tool and then the database, and then
// it checks the arguments against the input schema. The tool is checked
// before the arguments are read, and the database before any other
// argument, so a refusal tells a caller nothing about what it may not use.
func (s *Server) begin(ctx context.Context, req *mcp.CallToolRequest, d *dataTool) (*call, map[string]any, *mcp.CallToolResult) {
	name := d.tool.Name
	caller, _ := callerFrom(ctx)
	if err := s.authorizeTool(caller, name, d.free); err != nil {
		return nil, nil, toolError(err)
	}
	args, err := argsOf(name, req.Params.Arguments)
	if err != nil {
		return nil, nil, toolError(err)
	}
	if db, ok := args[databaseArg].(string); ok {
		if err := s.authorizeDatabase(caller, db); err != nil {
			return nil, nil, toolError(err)
		}
	}
	if err := checkArgs(name, d.tool.InputSchema.(*jsonschema.Schema), args); err != nil {
		return nil, nil, toolError(err)
	}
	c := &call{s: s, tool: name, caller: caller, database: args[databaseArg].(string), meta: req.Params.Meta, write: d.write, login: d.login, limits: s.cfg.Limits, fromCatalog: d.fromCatalog}
	if c.login == "" {
		c.login = "read"
		if d.write {
			c.login = "write"
		}
	}
	if d.limits != nil {
		c.limits = *d.limits
	}
	return c, args, nil
}

// decode decodes arguments that match the input schema into dst. It fails
// only on a fault of the server.
func (c *call) decode(args map[string]any, dst any) error {
	raw, err := json.Marshal(args)
	if err == nil {
		err = json.Unmarshal(raw, dst)
	}
	if err != nil {
		return argError(c.tool, "the arguments do not decode: "+err.Error())
	}
	return nil
}

// refusedCall is a refusal that a call raises inside its transaction, so
// that Run rolls the transaction back. Its text goes to the agent.
type refusedCall struct{ text string }

// Error returns the text of the refusal.
func (r *refusedCall) Error() string { return r.text }

// refuse returns a refusal inside the transaction with text.
func refuse(text string) error { return &refusedCall{text: text} }

// run runs fn in one transaction on the call's login, after step 4 of
// docs/DESIGN.md section 5: an ai.straza/object-id in _meta must equal the
// database id read in the same transaction. A read runs read-only and a
// write read-write, both with the statement_timeout of the call's caps. A
// refusal or an error of fn rolls the transaction back and returns a tool
// error. A read that stopped at its cap returns its rows, and the names of
// types that pgx does not know are read in a second transaction, because the
// first one's connection is closed.
func (c *call) run(ctx context.Context, fn func(tx *pg.Tx) (*mcp.CallToolResult, error)) *mcp.CallToolResult {
	if c.s.pools == nil {
		return toolError(errors.New(c.tool + " on " + c.database + " cannot run, because " + errNotStarted.Error() + ". This is a bug in adaptive-pg-mcp. Report it with the log."))
	}
	login, access := pg.Login{Database: c.database, Role: c.login}, pg.ReadOnly
	if c.write {
		access = pg.ReadWrite
	}
	var res *mcp.CallToolResult
	err := c.s.pools.Run(ctx, login, access, c.limits.Timeout, func(tx *pg.Tx) error {
		if err := c.s.checkObjectID(ctx, tx, c.database, c.meta); err != nil {
			return refuse(err.Error())
		}
		var err error
		res, err = fn(tx)
		return err
	})
	var r *refusedCall
	switch {
	case err == nil:
		return res
	case errors.Is(err, errCut) && len(c.untyped) > 0:
		err = c.s.pools.Run(ctx, login, access, c.limits.Timeout, func(tx *pg.Tx) error { return nameTypes(ctx, tx, c.typed, c.untyped) })
		if err == nil {
			return res
		}
	case errors.Is(err, errCut):
		return res
	case errors.As(err, &r):
		return toolError(r)
	}
	return toolError(c.failed(err))
}

// failed returns the error of a call whose statement or connection failed.
// The reason comes from pg's Reason, which never holds a connection string
// or a password.
func (c *call) failed(err error) error {
	done := " failed, because "
	if c.write {
		done = " changed nothing, because "
	}
	advice := "Correct the call and try again."
	var e *pg.Error
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &e) && e.Connect:
		advice = "Try again, or ask an administrator to run adaptive-pg-mcp check."
	case errors.Is(err, context.Canceled):
		advice = "Call it again if the result is still needed."
	case errors.As(err, &pgErr) && pgErr.Code == "57014":
		advice = "The timeout is " + c.limits.Timeout.String() + ". Narrow the call, or ask an administrator to raise " + c.limitKey("timeout") + "."
	}
	return errors.New(c.tool + " on " + c.database + done + c.s.pools.Reason(err) + ". " + advice)
}

// writeCap is step 6 of docs/DESIGN.md section 5 for a write: it refuses, and
// so rolls back, a write that changed more rows than the call's write_rows.
func (c *call) writeCap(changed int64, advice string) error {
	limit := int64(c.limits.WriteRows)
	if changed <= limit {
		return nil
	}
	key := "limits.write_rows in server.yaml"
	if c.fromCatalog {
		key = c.limitKey("write_rows")
	}
	return refuse(c.tool + " on " + c.database + " would change " + count(changed) + " rows, over the cap of " + count(limit) + ", and changed nothing. " + advice + ", or ask an administrator to raise " + key + ".")
}

// limitKey names the key that sets one cap of the call, such as
// limits.timeout in server.yaml. For a catalog tool it names the key in
// tools.yaml, because a cap set there does not follow server.yaml.
func (c *call) limitKey(name string) string {
	if c.fromCatalog {
		return "limits." + name + " of " + c.tool + " in tools.yaml"
	}
	return "limits." + name + " in server.yaml"
}

// readAnswer returns the result of a read: the columns, the rows and
// truncated, with text for people.
func readAnswer(a collected, text string) *mcp.CallToolResult {
	out := map[string]any{"columns": a.columns, "rows": a.rows, "truncated": a.more}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: out}
}

// writeAnswer returns the result of a write: the rows it changed, and the
// columns and rows it returned, with text for people.
func writeAnswer(changed int64, a collected, text string) *mcp.CallToolResult {
	out := map[string]any{"rowsAffected": changed, "columns": a.columns, "rows": a.rows}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: out}
}

// stopped returns the text that says where a read stopped when more rows
// exist, and what to do, or empty when the result is whole.
func (c *call) stopped(a collected, limit int, narrow string) string {
	switch {
	case !a.more:
		return ""
	case a.bytesFull:
		return " More rows exist beyond the byte cap of " + count(int64(c.limits.Bytes)) + " bytes. " + narrow
	case limit < c.limits.Rows:
		return " More rows exist beyond the limit of " + count(int64(limit)) + "."
	}
	return " More rows exist beyond the row cap of " + count(int64(c.limits.Rows)) + ". " + narrow
}
