package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// objectPage is the largest page of list_schemas and list_tables.
const objectPage = 100

// pageSize is the page size of list_schemas and list_tables: objectPage, or
// the row cap when it is lower.
func (s *Server) pageSize() int { return min(objectPage, s.cfg.Limits.Rows) }

// listSchemasSQL lists the schemas the login of the connection may use, after
// the cursor schema $1, by name and without the system schemas. A $3 that is
// not null narrows the list to those schemas.
const listSchemasSQL = `SELECT n.nspname::text AS "schema"
FROM pg_namespace n
WHERE has_schema_privilege(n.oid, 'USAGE')
  AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  AND n.nspname > $1::name
  AND ($3::text[] IS NULL OR n.nspname::text = ANY($3::text[]))
ORDER BY n.nspname
LIMIT $2`

// listTablesSQL lists the tables, views, materialized views and foreign
// tables the login of the connection may use, in schema $1 or in all, after
// the cursor position $2.$3, by name. $5 and $6, when not null, are parallel
// lists of schemas and table names that narrow the list to those tables.
const listTablesSQL = `SELECT n.nspname::text AS "schema", c.relname::text AS "table",
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'partitioned table' WHEN 'v' THEN 'view'
    WHEN 'm' THEN 'materialized view' ELSE 'foreign table' END AS "kind"
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
  AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  AND has_schema_privilege(n.oid, 'USAGE')
  AND (has_table_privilege(c.oid, 'SELECT, INSERT, UPDATE, DELETE') OR has_any_column_privilege(c.oid, 'SELECT, INSERT, UPDATE'))
  AND ($1::text IS NULL OR n.nspname::text = $1::text)
  AND (n.nspname, c.relname) > ($2::name, $3::name)
  AND ($5::text[] IS NULL OR (n.nspname::text, c.relname::text) IN (SELECT * FROM unnest($5::text[], $6::text[])))
ORDER BY n.nspname, c.relname
LIMIT $4`

// describeSQL describes the columns of table $1.$2 that the login of the
// connection may use, in table order.
const describeSQL = `SELECT a.attname::text AS "column", format_type(a.atttypid, a.atttypmod) AS "type",
  NOT a.attnotnull AS "nullable", pg_get_expr(d.adbin, d.adrelid) AS "default",
  coalesce(a.attnum = ANY(i.indkey::int2[]), false) AS "primary_key"
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
LEFT JOIN pg_index i ON i.indrelid = c.oid AND i.indisprimary
WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
  AND has_schema_privilege(n.oid, 'USAGE')
  AND has_column_privilege(c.oid, a.attnum, 'SELECT, INSERT, UPDATE, REFERENCES')
ORDER BY a.attnum`

// pagePosition is what a cursor of list_schemas or list_tables holds: the
// listing it belongs to and the last object of the previous page.
type pagePosition struct {
	Tool     string  `json:"t"`
	Database string  `json:"d"`
	Filter   *string `json:"f,omitempty"`
	Schema   string  `json:"s"`
	Table    string  `json:"n,omitempty"`
}

// encodeCursor writes a position as an opaque cursor.
func encodeCursor(p pagePosition) string {
	raw, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodePosition reads a cursor of tool on database with the schema filter,
// and refuses one that belongs to another listing.
func decodePosition(tool, database string, filter *string, cursor string) (pagePosition, error) {
	var p pagePosition
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	same := p.Tool == tool && p.Database == database && (filter == nil) == (p.Filter == nil) && (filter == nil || *filter == *p.Filter)
	if err != nil || cursor == "" || !same {
		return p, errors.New("the cursor of " + tool + " is not from this listing. Pass the nextCursor of the previous page unchanged, or leave cursor out for the first page.")
	}
	return p, nil
}

// listingArgs are the arguments of list_schemas and list_tables.
type listingArgs struct {
	Database string  `json:"database"`
	Schema   *string `json:"schema"`
	Cursor   *string `json:"cursor"`
}

// listSchemasTool is list_schemas as tools/list shows it.
func (s *Server) listSchemasTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{"cursor": cursorProp(listSchemasName)}, []string{"cursor"})
	description := "Lists the schemas that the read login may use, without the system schemas, " + strconv.Itoa(s.pageSize()) + " per page."
	return &dataTool{tool: listingTool(listSchemasName, description, in), run: s.listSchemas}
}

// listTablesTool is list_tables as tools/list shows it.
func (s *Server) listTablesTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{
		"schema": identifier("The schema to list, by exact name. Omit for every schema."),
		"cursor": cursorProp(listTablesName),
	}, []string{"schema", "cursor"})
	description := "Lists the tables, views, materialized views and foreign tables that the read login may use, with schema, name and kind, " +
		strconv.Itoa(s.pageSize()) + " per page."
	return &dataTool{tool: listingTool(listTablesName, description, in), run: s.listTables}
}

// describeTableTool is describe_table as tools/list shows it.
func (s *Server) describeTableTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{"schema": schemaProp(), "table": tableProp("to describe")}, []string{"schema", "table"}, "table")
	description := "Describes the columns of one table or view that the read login may use: name, type, nullable, default and primary key."
	return &dataTool{tool: readTool(describeTableName, description, in), run: s.describeTable}
}

func (s *Server) listSchemas(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args listingArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	var after pagePosition
	if args.Cursor != nil {
		var err error
		if after, err = decodePosition(listSchemasName, c.database, nil, *args.Cursor); err != nil {
			return toolError(err)
		}
	}
	page := s.pageSize()
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		schemas, _ := s.tablesOf(c.caller)
		a, err := c.read(ctx, tx, page, listSchemasSQL, after.Schema, int64(page+1), schemas)
		if err != nil && !errors.Is(err, errCut) {
			return nil, err
		}
		next := ""
		if a.more && len(a.rows) > 0 {
			last := a.rows[len(a.rows)-1]
			next = encodeCursor(pagePosition{Tool: listSchemasName, Database: c.database, Schema: last[0].(string)})
		}
		return listing(a, next, "list_schemas found "+plural(int64(len(a.rows)), "schema")+" on "+c.database+pageNames(a)+"."), err
	})
}

func (s *Server) listTables(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args listingArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	var after pagePosition
	if args.Cursor != nil {
		var err error
		if after, err = decodePosition(listTablesName, c.database, args.Schema, *args.Cursor); err != nil {
			return toolError(err)
		}
	}
	var filter any
	if args.Schema != nil {
		filter = *args.Schema
	}
	page := s.pageSize()
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		schemas, tables := s.tablesOf(c.caller)
		a, err := c.read(ctx, tx, page, listTablesSQL, filter, after.Schema, after.Table, int64(page+1), schemas, tables)
		if err != nil && !errors.Is(err, errCut) {
			return nil, err
		}
		next := ""
		if a.more && len(a.rows) > 0 {
			last := a.rows[len(a.rows)-1]
			next = encodeCursor(pagePosition{Tool: listTablesName, Database: c.database, Filter: args.Schema, Schema: last[0].(string), Table: last[1].(string)})
		}
		where := " on " + c.database
		if args.Schema != nil {
			where = " in the schema " + *args.Schema + " of " + c.database
		}
		return listing(a, next, "list_tables found "+plural(int64(len(a.rows)), "table")+where+"."), err
	})
}

// listing returns one page of a listing tool, with nextCursor when more
// follow. truncated is true only when more follow and no cursor can reach
// them, which takes one name longer than the byte cap.
func listing(a collected, next, text string) *mcp.CallToolResult {
	out := map[string]any{"columns": a.columns, "rows": a.rows, "truncated": a.more && next == ""}
	if next != "" {
		out["nextCursor"] = next
		text += " More follow: pass nextCursor as cursor."
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: out}
}

// pageNames writes the first column of a short page for the text, such as
// ": audit and public".
func pageNames(a collected) string {
	if len(a.rows) == 0 || len(a.rows) > 10 {
		return ""
	}
	var list []string
	for _, r := range a.rows {
		list = append(list, r[0].(string))
	}
	return ": " + joinAnd(list)
}

func (s *Server) describeTable(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var t target
	if err := c.decode(raw, &t); err != nil {
		return toolError(err)
	}
	if err := t.resolve(c); err != nil {
		return toolError(err)
	}
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		a, err := c.read(ctx, tx, s.cfg.Limits.Rows, describeSQL, t.Schema, t.Table)
		if err != nil && !errors.Is(err, errCut) {
			return nil, err
		}
		if len(a.rows) == 0 && !a.more {
			return nil, refuse("describe_table found no table " + t.Schema + "." + t.Table + " on " + c.database + " that its login may use. Call list_tables for the tables it may use.")
		}
		text := "describe_table found " + plural(int64(len(a.rows)), "column") + " of " + t.Schema + "." + t.Table + " on " + c.database + "."
		return readAnswer(a, text+c.stopped(a, s.cfg.Limits.Rows, "Ask an administrator to raise the cap.")), err
	})
}
