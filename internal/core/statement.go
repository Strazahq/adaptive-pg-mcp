package core

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// statement builds the SQL text of one call and its bind parameters. Its
// text holds only fixed SQL, identifiers quoted by pg.Quote, and
// placeholders, so no value of a call reaches the text.
type statement struct {
	sql  strings.Builder
	args []any
}

// add appends fixed SQL or quoted identifiers to the text.
func (q *statement) add(parts ...string) {
	for _, p := range parts {
		q.sql.WriteString(p)
	}
}

// bind appends v as the next bind parameter and returns its placeholder.
func (q *statement) bind(v any) string {
	q.args = append(q.args, v)
	return "$" + strconv.Itoa(len(q.args))
}

// list appends the quoted names joined by commas.
func (q *statement) list(names []string) {
	for i, n := range names {
		if i > 0 {
			q.add(", ")
		}
		q.add(pg.Quote(n))
	}
}

// query sends the statement on tx.
func (q *statement) query(ctx context.Context, tx *pg.Tx) (pgx.Rows, error) {
	return tx.Query(ctx, q.sql.String(), q.args...)
}

// condition is one entry of a where filter.
type condition struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	// Value is the value as JSON, nil when the entry leaves it out.
	Value json.RawMessage `json:"value"`
}

// whereSQL is the SQL of each op, from a fixed list.
var whereSQL = map[string]string{
	"=": " = ", "<>": " <> ", "<": " < ", "<=": " <= ", ">": " > ", ">=": " >= ",
	"like": " LIKE ", "ilike": " ILIKE ", "in": " IN (", "not in": " NOT IN (",
	"is null": " IS NULL", "is not null": " IS NOT NULL",
}

// maxIn is the most values one in or not in entry takes.
const maxIn = 1000

// checkWhere checks the value of every where entry against its op, before
// the call opens a transaction: in and not in take a list of 1 to maxIn
// values, is null and is not null take none, and every other op takes one
// value that is not null.
func checkWhere(tool string, where []condition) error {
	for i, w := range where {
		at := "where[" + strconv.Itoa(i) + "]"
		given := w.Value != nil && string(w.Value) != "null"
		switch w.Op {
		case "is null", "is not null":
			if given {
				return argError(tool, at+" has op "+w.Op+", which takes no value")
			}
		case "in", "not in":
			var list []json.RawMessage
			if !given || json.Unmarshal(w.Value, &list) != nil {
				return argError(tool, at+" has op "+w.Op+", which takes a list of values, and the call passed "+passed(w.Value))
			}
			if len(list) == 0 || len(list) > maxIn {
				return argError(tool, at+" has op "+w.Op+", which takes 1 to "+count(maxIn)+" values, not "+count(int64(len(list))))
			}
		default:
			if !given {
				return argError(tool, at+" has op "+w.Op+", which takes a value that is not null. Use op is null to match null")
			}
		}
	}
	return nil
}

// passed names the value of a where entry for a sentence: none when the entry
// leaves it out, or its JSON kind.
func passed(raw json.RawMessage) string {
	if raw == nil {
		return "none"
	}
	v, err := argsValue(raw)
	if err != nil {
		return "a value that is not JSON"
	}
	return kindOf(v)
}

// where appends WHERE and every entry, joined by AND. Each column must be
// one of cols, and each value is a bind parameter of the column's type.
func (q *statement) where(where []condition, cols *tableColumns) error {
	for i, w := range where {
		col, err := cols.get(w.Column)
		if err != nil {
			return err
		}
		if i == 0 {
			q.add(" WHERE ")
		} else {
			q.add(" AND ")
		}
		q.add(pg.Quote(w.Column), whereSQL[w.Op])
		switch w.Op {
		case "is null", "is not null":
		case "in", "not in":
			var list []json.RawMessage
			_ = json.Unmarshal(w.Value, &list)
			for j, v := range list {
				if j > 0 {
					q.add(", ")
				}
				q.add(q.bind(bindValue(v, col.json)))
			}
			q.add(")")
		default:
			q.add(q.bind(bindValue(w.Value, col.json)))
		}
	}
	return nil
}

// bindValue turns a JSON value into a bind parameter for a column. A JSON
// null is SQL NULL. A json or jsonb column takes the value's JSON text. Any
// other column takes the value's text, which pgx sends in text format and
// Postgres parses as the column's own type, so a number keeps every digit
// and a string reaches a date, uuid or numeric column as Postgres reads it.
func bindValue(raw json.RawMessage, isJSON bool) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if isJSON {
		return string(trimmed)
	}
	var s string
	if trimmed[0] == '"' && json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	return string(trimmed)
}

// tableColumn is one column of a table that the login may use.
type tableColumn struct {
	name string
	// json is true for a column of type json or jsonb, or a domain over one.
	json bool
	// readable is true when the login may select the column.
	readable bool
}

// tableColumns are the columns of one table that the login may use, in table
// order.
type tableColumns struct {
	call   *call
	schema string
	table  string
	order  []tableColumn
}

// columnsSQL reads the columns of one table that the login of the
// connection may use, in the order of the table, and nothing when the login
// may not use the schema or any column of the table.
const columnsSQL = `SELECT a.attname::text,
  coalesce(b.typname IN ('json', 'jsonb'), false),
  has_column_privilege(c.oid, a.attnum, 'SELECT')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
JOIN pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_type b ON b.oid = CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.oid END
WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
  AND has_schema_privilege(n.oid, 'USAGE')
  AND has_column_privilege(c.oid, a.attnum, 'SELECT, INSERT, UPDATE, REFERENCES')
ORDER BY a.attnum`

// columnsOf reads the columns of schema.table that the login may use on
// tx, and refuses the call when there are none.
func (c *call) columnsOf(ctx context.Context, tx *pg.Tx, schema, table string) (*tableColumns, error) {
	rows, err := tx.Query(ctx, columnsSQL, schema, table)
	if err != nil {
		return nil, err
	}
	cols := &tableColumns{call: c, schema: schema, table: table}
	var col tableColumn
	_, err = pgx.ForEachRow(rows, []any{&col.name, &col.json, &col.readable}, func() error {
		cols.order = append(cols.order, col)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(cols.order) == 0 {
		return nil, refuse(c.tool + " found no table " + schema + "." + table + " on " + c.database + " that its login may use. Call list_tables for the tables it may use.")
	}
	return cols, nil
}

// get returns the column of that name, or refuses the call when the table
// has none that the login may use.
func (t *tableColumns) get(name string) (tableColumn, error) {
	for _, col := range t.order {
		if col.name == name {
			return col, nil
		}
	}
	return tableColumn{}, refuse(t.schema + "." + t.table + " on " + t.call.database + " has no column " + strconv.Quote(name) + " that the login may use. Call describe_table to see its columns.")
}

// all checks that every name is a column, and returns the names.
func (t *tableColumns) all(names []string) ([]string, error) {
	for _, n := range names {
		if _, err := t.get(n); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// readable returns the columns the login may select, in table order.
func (t *tableColumns) readable() []string {
	var out []string
	for _, col := range t.order {
		if col.readable {
			out = append(out, col.name)
		}
	}
	return out
}

// qualified writes schema.table with both names quoted.
func qualified(schema, table string) string { return pg.Quote(schema) + "." + pg.Quote(table) }

// target is the table a tool names, with public when the call leaves the
// schema out.
type target struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Table    string `json:"table"`
}

// resolve fills in public for a schema the call leaves out, and refuses a
// table outside the tables access.yaml lists for the caller.
func (t *target) resolve(c *call) error {
	if t.Schema == "" {
		t.Schema = "public"
	}
	return c.s.authorizeTable(c.caller, c.database, t.Schema, t.Table)
}
