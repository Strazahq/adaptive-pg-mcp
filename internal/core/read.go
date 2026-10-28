package core

import (
	"context"
	"errors"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// ordering is one entry of order_by.
type ordering struct {
	Column    string `json:"column"`
	Direction string `json:"direction"`
}

// readRowsArgs are the arguments of read_rows.
type readRowsArgs struct {
	target
	Columns []string    `json:"columns"`
	Where   []condition `json:"where"`
	OrderBy []ordering  `json:"order_by"`
	Limit   *int        `json:"limit"`
}

// aggregateArgs are the arguments of aggregate.
type aggregateArgs struct {
	target
	Function string      `json:"function"`
	Column   string      `json:"column"`
	Where    []condition `json:"where"`
	GroupBy  []string    `json:"group_by"`
}

// aggregates maps each function of aggregate to its SQL, from a fixed list.
var aggregates = map[string]string{"count": "count(", "sum": "sum(", "avg": "avg(", "min": "min(", "max": "max("}

// readRowsTool is read_rows as tools/list shows it.
func (s *Server) readRowsTool() *dataTool {
	rows := float64(s.cfg.Limits.Rows)
	in := s.input(map[string]*jsonschema.Schema{
		"schema":  schemaProp(),
		"table":   tableProp("to read"),
		"columns": identifiers("The columns to answer, in this order. Leave it out for every column the login may read.", "A column name.", 1),
		"where":   whereProp(0, "The filter: each entry is {column, op, value}, and a row must match every entry. Leave it out to read every row."),
		"order_by": {Type: "array", Description: "The order of the rows, by column, first entry first.", MinItems: atLeast(1), Items: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"column":    identifier("The column to order by."),
				"direction": {Type: "string", Description: "asc for ascending, which is the default, or desc for descending.", Enum: []any{"asc", "desc"}},
			},
			Required: []string{"column"}, AdditionalProperties: closed(), PropertyOrder: []string{"column", "direction"},
		}},
		"limit": {Type: "integer", Description: "The most rows to answer, from 1 to the row cap of " + count(int64(rows)) + ". Leave it out for the row cap.", Minimum: ptrTo(1.0), Maximum: &rows},
	}, []string{"schema", "table", "columns", "where", "order_by", "limit"}, "table")
	description := "Reads rows of one table or view in a read-only transaction on the read login, with an optional filter, order and limit. " +
		"It answers columns and rows, each row an array in column order. It answers at most " + count(int64(s.cfg.Limits.Rows)) + " rows and " +
		count(int64(s.cfg.Limits.Bytes)) + " bytes, and truncated is true when more rows follow."
	return &dataTool{tool: readTool(readRowsName, description, in), run: s.readRows}
}

// aggregateTool is aggregate as tools/list shows it.
func (s *Server) aggregateTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{
		"schema":   schemaProp(),
		"table":    tableProp("to aggregate"),
		"function": {Type: "string", Description: "The aggregate function: count, sum, avg, min or max.", Enum: []any{"count", "sum", "avg", "min", "max"}},
		"column":   identifier("The column to aggregate. count may leave it out to count rows, and every other function needs it."),
		"where":    whereProp(0, "The filter: each entry is {column, op, value}, and a row must match every entry. Leave it out to aggregate every row."),
		"group_by": identifiers("The columns to group by. The answer has one row per group, ordered by these columns. Leave it out for one row.", "A column name.", 1),
	}, []string{"schema", "table", "function", "column", "where", "group_by"}, "table", "function")
	description := "Computes count, sum, avg, min or max over one column of a table or view in a read-only transaction on the read login, with an optional filter and grouping. " +
		"It answers columns and rows: the group_by columns and then the aggregate. It answers at most " + count(int64(s.cfg.Limits.Rows)) +
		" groups, and truncated is true when more follow."
	return &dataTool{tool: readTool(aggregateName, description, in), run: s.aggregate}
}

// ptrTo returns a pointer to f, for the bounds of a schema.
func ptrTo(f float64) *float64 { return &f }

// readRows answers read_rows: SELECT of the columns from the table, with
// the where filter, the order and LIMIT one past the limit, so that the
// answer knows whether more rows follow.
func (s *Server) readRows(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args readRowsArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	if err := checkWhere(c.tool, args.Where); err != nil {
		return toolError(err)
	}
	if err := args.resolve(c); err != nil {
		return toolError(err)
	}
	limit := s.cfg.Limits.Rows
	if args.Limit != nil {
		limit = *args.Limit
	}
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		cols, err := c.columnsOf(ctx, tx, args.Schema, args.Table)
		if err != nil {
			return nil, err
		}
		selected := cols.readable()
		if args.Columns != nil {
			if selected, err = cols.all(args.Columns); err != nil {
				return nil, err
			}
		}
		if len(selected) == 0 {
			return nil, refuse("the read login of " + c.database + " may select no column of " + args.Schema + "." + args.Table + ", so read_rows is refused. Ask an administrator to grant SELECT on the columns it needs.")
		}
		var q statement
		q.add("SELECT ")
		q.list(selected)
		q.add(" FROM ", qualified(args.Schema, args.Table))
		if err := q.where(args.Where, cols); err != nil {
			return nil, err
		}
		for i, o := range args.OrderBy {
			if _, err := cols.get(o.Column); err != nil {
				return nil, err
			}
			if i == 0 {
				q.add(" ORDER BY ")
			} else {
				q.add(", ")
			}
			q.add(pg.Quote(o.Column))
			if o.Direction == "desc" {
				q.add(" DESC")
			}
		}
		q.add(" LIMIT ", q.bind(int64(limit+1)))
		a, err := c.read(ctx, tx, limit, q.sql.String(), q.args...)
		if err != nil && !errors.Is(err, errCut) {
			return nil, err
		}
		text := "read_rows answered " + plural(int64(len(a.rows)), "row") + " of " + args.Schema + "." + args.Table + " on " + c.database + "."
		return readAnswer(a, text+c.stopped(a, limit, "Narrow the where filter or the columns, or page with order_by and a where filter past the last row.")), err
	})
}

// aggregate answers aggregate: SELECT of the group_by columns and the
// function over the column, with the where filter, grouped and ordered by
// the group_by columns.
func (s *Server) aggregate(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args aggregateArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	if args.Column == "" && args.Function != "count" {
		return toolError(argError(c.tool, "the function "+args.Function+" needs the argument column, and the call leaves it out"))
	}
	if err := checkWhere(c.tool, args.Where); err != nil {
		return toolError(err)
	}
	if err := args.resolve(c); err != nil {
		return toolError(err)
	}
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		cols, err := c.columnsOf(ctx, tx, args.Schema, args.Table)
		if err != nil {
			return nil, err
		}
		if _, err := cols.all(args.GroupBy); err != nil {
			return nil, err
		}
		var q statement
		q.add("SELECT ")
		if len(args.GroupBy) > 0 {
			q.list(args.GroupBy)
			q.add(", ")
		}
		q.add(aggregates[args.Function])
		if args.Column == "" {
			q.add("*")
		} else {
			if _, err := cols.get(args.Column); err != nil {
				return nil, err
			}
			q.add(pg.Quote(args.Column))
		}
		q.add(") FROM ", qualified(args.Schema, args.Table))
		if err := q.where(args.Where, cols); err != nil {
			return nil, err
		}
		if len(args.GroupBy) > 0 {
			q.add(" GROUP BY ")
			q.list(args.GroupBy)
			q.add(" ORDER BY ")
			q.list(args.GroupBy)
			q.add(" LIMIT ", q.bind(int64(s.cfg.Limits.Rows+1)))
		}
		a, err := c.read(ctx, tx, s.cfg.Limits.Rows, q.sql.String(), q.args...)
		if err != nil && !errors.Is(err, errCut) {
			return nil, err
		}
		text := "aggregate answered " + args.Function + " of " + args.Schema + "." + args.Table + " on " + c.database
		if len(args.GroupBy) > 0 {
			text += " for " + plural(int64(len(a.rows)), "group")
		} else if len(a.rows) == 1 && len(a.rows[0]) == 1 {
			text += ": " + valueOf(a.rows[0][0])
		}
		return readAnswer(a, text+"."+c.stopped(a, s.cfg.Limits.Rows, "Narrow the where filter, or group by fewer columns.")), err
	})
}

// valueOf writes one answered value for the text, such as 1240.
func valueOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case int64:
		return strconv.FormatInt(x, 10)
	case string:
		return x
	}
	b, _ := jsonText(v)
	return b
}
