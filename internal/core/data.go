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

// dataTool is one built-in data tool: what tools/list shows, and how its calls run.
type dataTool struct {
	tool *mcp.Tool
	// write is true when the tool runs read-write on the write login, and false when it runs read-only on the read login.
	write bool
	// free is true for query_sql and execute_sql, which a caller narrowed to tables may not use.
	free bool
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
		s.mcp.AddTool(d.tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			c, args, refused := s.begin(ctx, req, d)
			if refused != nil {
				return refused, nil
			}
			return d.run(ctx, c, args), nil
		})
	}
}

// call is one call of a data tool that passed the checks before its statement.
type call struct {
	s        *Server
	tool     string
	caller   string
	database string
	meta     mcp.Meta
	write    bool
	// typed and untyped are the columns of a read that stopped at its cap, and those whose type names run reads after its transaction.
	typed   []resultColumn
	untyped []untypedColumn
}

// begin runs steps 2 and 3 of every call in docs/DESIGN.md section 5: in
// mode standalone the caller may use the tool and then the database, and
// then the arguments match the input schema. The tool is checked before the
// arguments are read, and the database before any other argument, so a
// refusal tells a caller nothing about what it may not use.
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
	c := &call{s: s, tool: name, caller: caller, database: args[databaseArg].(string), meta: req.Params.Meta, write: d.write}
	return c, args, nil
}

// decode decodes arguments that match the input schema into dst, which cannot fail but on a fault of the server.
func (c *call) decode(args map[string]any, dst any) error {
	raw, err := json.Marshal(args)
	if err == nil {
		err = json.Unmarshal(raw, dst)
	}
	if err != nil {
		return argError(c.tool, "its arguments do not decode: "+err.Error())
	}
	return nil
}

// refusedCall is a refusal that a call raises inside its transaction, so
// that Run rolls the transaction back. Its text is the sentence for the agent.
type refusedCall struct{ text string }

// Error returns the sentence.
func (r *refusedCall) Error() string { return r.text }

// refuse is a refusal inside the transaction with the given sentence.
func refuse(text string) error { return &refusedCall{text: text} }

// run runs fn in one transaction of the call's login, after step 4 of
// docs/DESIGN.md section 5: when _meta carries an ai.straza/object-id, it
// must equal the id of the database read in this very transaction. A read
// runs read-only on the read login, and a write read-write on the write
// login, both with statement_timeout from the limits. A refusal or an error
// of fn rolls the transaction back and answers a tool error. A read that
// stopped at its cap answers its rows, and the names of types that pgx does
// not know are read in a transaction of their own, since its connection is closed.
func (c *call) run(ctx context.Context, fn func(tx *pg.Tx) (*mcp.CallToolResult, error)) *mcp.CallToolResult {
	if c.s.pools == nil {
		return toolError(errors.New(c.tool + " on " + c.database + " cannot run, because " + errNotStarted.Error() + ". Serve it with adaptive-pg-mcp serve, which runs them before it serves."))
	}
	login, access := pg.Login{Database: c.database, Role: "read"}, pg.ReadOnly
	if c.write {
		login, access = pg.Login{Database: c.database, Role: "write"}, pg.ReadWrite
	}
	var res *mcp.CallToolResult
	err := c.s.pools.Run(ctx, login, access, c.s.cfg.Limits.Timeout, func(tx *pg.Tx) error {
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
		err = c.s.pools.Run(ctx, login, access, c.s.cfg.Limits.Timeout, func(tx *pg.Tx) error { return nameTypes(ctx, tx, c.typed, c.untyped) })
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

// failed is the sentence for a call whose statement or connection failed.
// The reason comes from pg's Reason, which never holds a connection string
// or a password.
func (c *call) failed(err error) error {
	done := "nothing was read"
	if c.write {
		done = "nothing was changed"
	}
	advice := "Correct the call as the reason says, and call it again."
	var e *pg.Error
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &e) && e.Connect:
		advice = "Try again, and if it fails again, ask an administrator to run adaptive-pg-mcp check."
	case errors.Is(err, context.Canceled):
		advice = "Call it again if the answer is still needed."
	case errors.As(err, &pgErr) && pgErr.Code == "57014":
		advice = "A statement may run for " + c.s.cfg.Limits.Timeout.String() + ", so narrow the call, or ask an administrator to raise limits.timeout."
	}
	return errors.New(c.tool + " on " + c.database + " failed, because " + c.s.pools.Reason(err) + ", so " + done + ". " + advice)
}

// writeCap is step 6 of docs/DESIGN.md section 5 for a write: a write that
// changed more rows than write_rows is refused, and the refusal rolls it back.
func (c *call) writeCap(changed int64, advice string) error {
	limit := int64(c.s.cfg.Limits.WriteRows)
	if changed <= limit {
		return nil
	}
	return refuse(c.tool + " on " + c.database + " would change " + count(changed) + " rows and the cap is " + count(limit) + ", so nothing was changed. " + advice + ", or ask an administrator to raise write_rows.")
}

// readAnswer is the result of a read: the columns, the rows and truncated, with text for people.
func readAnswer(a collected, text string) *mcp.CallToolResult {
	out := map[string]any{"columns": a.columns, "rows": a.rows, "truncated": a.more}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: out}
}

// writeAnswer is the result of a write: the rows it changed, and the columns and rows it returned, with text for people.
func writeAnswer(changed int64, a collected, text string) *mcp.CallToolResult {
	out := map[string]any{"rowsAffected": changed, "columns": a.columns, "rows": a.rows}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: out}
}

// stopped says where a read stopped when more rows follow than its answer
// holds, and what to do, or answers empty when the answer is whole.
func (c *call) stopped(a collected, limit int, narrow string) string {
	switch {
	case !a.more:
		return ""
	case a.bytesFull:
		return " More rows follow, and the answer stopped before the next row, because it would pass the byte cap of " + count(int64(c.s.cfg.Limits.Bytes)) + " bytes. " + narrow
	case limit < c.s.cfg.Limits.Rows:
		return " More rows follow than the limit of " + count(int64(limit)) + "."
	}
	return " More rows follow, and the answer stopped at the row cap of " + count(int64(c.s.cfg.Limits.Rows)) + ". " + narrow
}
