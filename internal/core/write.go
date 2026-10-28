package core

import (
	"context"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// insertRowsArgs are the arguments of insert_rows.
type insertRowsArgs struct {
	target
	Rows      []map[string]json.RawMessage `json:"rows"`
	Returning []string                     `json:"returning"`
}

// updateRowsArgs are the arguments of update_rows.
type updateRowsArgs struct {
	target
	Set       map[string]json.RawMessage `json:"set"`
	Where     []condition                `json:"where"`
	Returning []string                   `json:"returning"`
}

// deleteRowsArgs are the arguments of delete_rows.
type deleteRowsArgs struct {
	target
	Where     []condition `json:"where"`
	Returning []string    `json:"returning"`
}

// narrowWhere is the advice of a write cap refusal of update_rows and delete_rows.
const narrowWhere = "Narrow the where filter"

// writeLimits says what the caps of every write tool are, for its description.
func (s *Server) writeLimits() string {
	return "A call that would change more than " + count(int64(s.cfg.Limits.WriteRows)) + " rows is rolled back and refused. " +
		"It answers rowsAffected, and the columns and rows that returning names, at most " + count(int64(s.cfg.Limits.Rows)) + " rows."
}

// valuesProp is an object of column names and the values to write, with at least one column.
func valuesProp(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Description: description, MinProperties: atLeast(1), AdditionalProperties: &jsonschema.Schema{Description: "The value for the column, which travels as a bind parameter."}}
}

// insertRowsTool is insert_rows as tools/list shows it.
func (s *Server) insertRowsTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{
		"schema": schemaProp(),
		"table":  tableProp("to insert into"),
		"rows": {Type: "array", MinItems: atLeast(1), Description: "The rows to insert, each an object of column names and values. A column a row leaves out takes its default.",
			Items: valuesProp("One row: column names and their values.")},
		"returning": returningProp(),
	}, []string{"schema", "table", "rows", "returning"}, "table", "rows")
	description := "Inserts rows into one table in a read-write transaction on the write login. " + s.writeLimits()
	return &dataTool{tool: writeTool(insertRowsName, description, in), write: true, run: s.insertRows}
}

// updateRowsTool is update_rows as tools/list shows it.
func (s *Server) updateRowsTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{
		"schema":    schemaProp(),
		"table":     tableProp("to update"),
		"set":       valuesProp("The columns to change and their new values."),
		"where":     whereProp(1, "The rows to change: each entry is {column, op, value}, and a row must match every entry. It needs at least one entry."),
		"returning": returningProp(),
	}, []string{"schema", "table", "set", "where", "returning"}, "table", "set", "where")
	description := "Updates the rows of one table that match a where filter, in a read-write transaction on the write login. The filter needs at least one entry. " + s.writeLimits()
	return &dataTool{tool: writeTool(updateRowsName, description, in), write: true, run: s.updateRows}
}

// deleteRowsTool is delete_rows as tools/list shows it.
func (s *Server) deleteRowsTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{
		"schema":    schemaProp(),
		"table":     tableProp("to delete from"),
		"where":     whereProp(1, "The rows to delete: each entry is {column, op, value}, and a row must match every entry. It needs at least one entry."),
		"returning": returningProp(),
	}, []string{"schema", "table", "where", "returning"}, "table", "where")
	description := "Deletes the rows of one table that match a where filter, in a read-write transaction on the write login. The filter needs at least one entry. " + s.writeLimits()
	return &dataTool{tool: writeTool(deleteRowsName, description, in), write: true, run: s.deleteRows}
}

// insertRows answers insert_rows: INSERT of the columns any row names, in
// table order, with DEFAULT where a row leaves a column out.
func (s *Server) insertRows(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args insertRowsArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	if err := args.resolve(c); err != nil {
		return toolError(err)
	}
	return c.change(ctx, args.target, args.Returning, "Send at most "+count(int64(s.cfg.Limits.WriteRows))+" rows in one call", func(q *statement, cols *tableColumns) error {
		for _, row := range args.Rows {
			for name := range row {
				if _, err := cols.get(name); err != nil {
					return err
				}
			}
		}
		var named []tableColumn
		for _, col := range cols.order {
			for _, row := range args.Rows {
				if _, ok := row[col.name]; ok {
					named = append(named, col)
					break
				}
			}
		}
		q.add("INSERT INTO ", qualified(args.Schema, args.Table), " (")
		for i, col := range named {
			if i > 0 {
				q.add(", ")
			}
			q.add(pg.Quote(col.name))
		}
		q.add(") VALUES ")
		for i, row := range args.Rows {
			if i > 0 {
				q.add(", ")
			}
			q.add("(")
			for j, col := range named {
				if j > 0 {
					q.add(", ")
				}
				if v, ok := row[col.name]; ok {
					q.add(q.bind(bindValue(v, col.json)))
				} else {
					q.add("DEFAULT")
				}
			}
			q.add(")")
		}
		return nil
	})
}

// updateRows answers update_rows: UPDATE of the set columns, in table order, on the rows the where filter matches.
func (s *Server) updateRows(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args updateRowsArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	if err := checkWhere(c.tool, args.Where); err != nil {
		return toolError(err)
	}
	if err := args.resolve(c); err != nil {
		return toolError(err)
	}
	return c.change(ctx, args.target, args.Returning, narrowWhere, func(q *statement, cols *tableColumns) error {
		for name := range args.Set {
			if _, err := cols.get(name); err != nil {
				return err
			}
		}
		q.add("UPDATE ", qualified(args.Schema, args.Table), " SET ")
		first := true
		for _, col := range cols.order {
			v, ok := args.Set[col.name]
			if !ok {
				continue
			}
			if !first {
				q.add(", ")
			}
			first = false
			q.add(pg.Quote(col.name), " = ", q.bind(bindValue(v, col.json)))
		}
		return q.where(args.Where, cols)
	})
}

// deleteRows answers delete_rows: DELETE of the rows the where filter matches.
func (s *Server) deleteRows(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args deleteRowsArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	if err := checkWhere(c.tool, args.Where); err != nil {
		return toolError(err)
	}
	if err := args.resolve(c); err != nil {
		return toolError(err)
	}
	return c.change(ctx, args.target, args.Returning, narrowWhere, func(q *statement, cols *tableColumns) error {
		q.add("DELETE FROM ", qualified(args.Schema, args.Table))
		return q.where(args.Where, cols)
	})
}

// change runs one write tool's statement, which build writes after the
// table's columns are read, with RETURNING of the returning columns. It
// refuses and rolls back a write that changed more rows than write_rows,
// counted as the larger of its command tag and what Postgres counted in
// every table across the statement, so rows that a cascade or a trigger
// changes count too. When the command tag alone stays within the cap, the
// refusal says that the rows changed in other tables passed it. A write that
// changed the system catalogs, which only a trigger can make it do, is
// refused too. rowsAffected is the command tag, the rows of the table the
// call names.
func (c *call) change(ctx context.Context, t target, returning []string, advice string, build func(*statement, *tableColumns) error) *mcp.CallToolResult {
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		cols, err := c.columnsOf(ctx, tx, t.Schema, t.Table)
		if err != nil {
			return nil, err
		}
		var q statement
		if err := build(&q, cols); err != nil {
			return nil, err
		}
		if len(returning) > 0 {
			if _, err := cols.all(returning); err != nil {
				return nil, err
			}
			q.add(" RETURNING ")
			q.list(returning)
		}
		before, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		rows, err := q.query(ctx, tx)
		if err != nil {
			return nil, err
		}
		a, err := c.collect(ctx, tx, rows, c.s.cfg.Limits.Rows)
		if err != nil {
			return nil, err
		}
		after, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		if after.catalog > before.catalog {
			return nil, c.structureChanged(a.tag, "A trigger on the table changes structure, so ask an administrator to change it, or use execute_ddl.")
		}
		changed := a.tag.RowsAffected()
		if changed <= int64(c.s.cfg.Limits.WriteRows) {
			advice = "The statement changes rows in other tables through triggers or cascades, so narrow the call"
		}
		if err := c.writeCap(max(changed, after.rows-before.rows), advice); err != nil {
			return nil, err
		}
		verb := map[string]string{insertRowsName: "inserted", updateRowsName: "updated", deleteRowsName: "deleted"}[c.tool]
		text := c.tool + " " + verb + " " + plural(changed, "row") + " of " + t.Schema + "." + t.Table + " on " + c.database + "."
		if a.more {
			text += " It answers the first " + plural(int64(len(a.rows)), "returned row") + ", because the answer stopped at the row or byte cap."
		}
		return writeAnswer(changed, a, text), nil
	})
}
