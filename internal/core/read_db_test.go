//go:build pg

package core

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// oddTable is a table name that would end the identifier and add a statement if the server did not quote it.
const oddTable = `odd "name"; DROP TABLE tickets; --`

// readFixture is the SQL of the read tests: tables the login may read,
// a view, a table it may not read, a schema it may not use, and a table
// whose name breaks unquoted SQL. {login} is the quoted name of the login.
var readFixture = []string{
	"CREATE TABLE tickets (id int PRIMARY KEY, status text NOT NULL DEFAULT 'open', note text)",
	"INSERT INTO tickets VALUES (1, 'open', 'printer'), (2, 'closed', NULL), (3, 'open', 'Printer jam'), (4, 'pending', 'VPN')",
	"CREATE VIEW open_tickets AS SELECT id, note FROM tickets WHERE status = 'open'",
	"CREATE TABLE secrets (id int)",
	"CREATE SCHEMA audit",
	"CREATE TABLE audit.log (id int)",
	"CREATE SCHEMA hidden",
	"CREATE TABLE hidden.x (id int)",
	`CREATE TABLE "odd ""name""; DROP TABLE tickets; --" ("we""ird" int)`,
	`INSERT INTO "odd ""name""; DROP TABLE tickets; --" VALUES (7)`,
	"CREATE TABLE kinds (id int PRIMARY KEY, i2 int2, i8 int8, n numeric, f float8, t text, b bool, d date, ts timestamp, tstz timestamptz, u uuid, j json, jb jsonb)",
	"GRANT SELECT, INSERT, UPDATE, DELETE ON tickets, kinds TO {login}",
	`GRANT SELECT ON open_tickets, "odd ""name""; DROP TABLE tickets; --", hidden.x TO {login}`,
	"GRANT USAGE ON SCHEMA audit TO {login}",
	"GRANT SELECT ON audit.log TO {login}",
}

// served serves the database helpdesk on login l over stdio with the limits, and returns the session and its output schemas.
func served(t *testing.T, l pgtest.Login, d pgtest.Database, freeSQL, limits string) (*mcp.ClientSession, outputs) {
	t.Helper()
	srv := New(loadData(t, freeSQL, limits, helpdeskSingle, map[string]string{"HELPDESK_DSN": l.DSN(t, d)}), slog.New(slog.DiscardHandler))
	cs := overStdio(t, srv, gatewayToken, "")
	return cs, outputsOf(t, cs)
}

// rowsJSON writes the rows of an answer as JSON, for comparison with a literal.
func rowsJSON(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m["rows"])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// helpdesk adds database helpdesk to args.
func helpdesk(args map[string]any) map[string]any {
	args["database"] = "helpdesk"
	return args
}

func TestReadTools(t *testing.T) {
	where := func(entries ...any) []any { return entries }
	cond := func(column, op string, value any) map[string]any {
		m := map[string]any{"column": column, "op": op}
		if value != nil {
			m["value"] = value
		}
		return m
	}
	byID := []any{map[string]any{"column": "id"}}
	cases := []struct {
		name, tool string
		args       map[string]any
		rows, text string
		truncated  bool
	}{
		{"list_schemas answers the schemas the login may use", "list_schemas", map[string]any{}, `[["audit"],["public"]]`, "list_schemas found 2 schemas on helpdesk: audit and public.", false},
		{"list_tables answers what the login may use", "list_tables", map[string]any{}, `[["audit","log","table"],["public","kinds","table"],["public","odd \"name\"; DROP TABLE tickets; --","table"],["public","open_tickets","view"],["public","tickets","table"]]`, "list_tables found 5 tables on helpdesk.", false},
		{"list_tables of one schema", "list_tables", map[string]any{"schema": "audit"}, `[["audit","log","table"]]`, "list_tables found 1 table in the schema audit of helpdesk.", false},
		{"list_tables of a schema the login may not use", "list_tables", map[string]any{"schema": "hidden"}, `[]`, "list_tables found 0 tables in the schema hidden of helpdesk.", false},
		{"describe_table", "describe_table", map[string]any{"table": "tickets"}, `[["id","integer",false,null,true],["status","text",false,"'open'::text",false],["note","text",true,null,false]]`, "describe_table found 3 columns of public.tickets on helpdesk.", false},
		{"read_rows of every column", "read_rows", map[string]any{"table": "tickets", "order_by": byID}, `[[1,"open","printer"],[2,"closed",null],[3,"open","Printer jam"],[4,"pending","VPN"]]`, "read_rows returned 4 rows of public.tickets on helpdesk.", false},
		{"read_rows of a view", "read_rows", map[string]any{"table": "open_tickets", "order_by": byID}, `[[1,"printer"],[3,"Printer jam"]]`, "read_rows returned 2 rows of public.open_tickets on helpdesk.", false},
		{"read_rows of a name that breaks unquoted SQL", "read_rows", map[string]any{"table": oddTable, "columns": []any{`we"ird`}}, `[[7]]`, "read_rows returned 1 row of public." + oddTable + " on helpdesk.", false},
		{"read_rows of another schema", "read_rows", map[string]any{"schema": "audit", "table": "log"}, `[]`, "read_rows returned 0 rows of audit.log on helpdesk.", false},
		{"=", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("status", "=", "open"))}, `[[1],[3]]`, "", false},
		{"<>", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("status", "<>", "open"))}, `[[2],[4]]`, "", false},
		{"<", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("id", "<", 2))}, `[[1]]`, "", false},
		{"<=", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("id", "<=", 2))}, `[[1],[2]]`, "", false},
		{">", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("id", ">", 3))}, `[[4]]`, "", false},
		{">=", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("id", ">=", 3))}, `[[3],[4]]`, "", false},
		{"in", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("id", "in", []any{1, 4}))}, `[[1],[4]]`, "", false},
		{"not in", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("id", "not in", []any{1, 4}))}, `[[2],[3]]`, "", false},
		{"is null", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("note", "is null", nil))}, `[[2]]`, "", false},
		{"is not null", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("note", "is not null", nil))}, `[[1],[3],[4]]`, "", false},
		{"like", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("note", "like", "Print%"))}, `[[3]]`, "", false},
		{"ilike", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID, "where": where(cond("note", "ilike", "print%"))}, `[[1],[3]]`, "", false},
		{"two entries match both", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "where": where(cond("status", "=", "open"), cond("note", "ilike", "%jam"))}, `[[3]]`, "", false},
		{"a value that reads as SQL stays a value", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "where": where(cond("note", "=", "x' OR '1'='1"))}, `[]`, "", false},
		{"order_by desc with a limit", "read_rows", map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": []any{map[string]any{"column": "id", "direction": "desc"}}, "limit": 2}, `[[4],[3]]`, "read_rows returned 2 rows of public.tickets on helpdesk. More rows exist beyond the limit of 2.", true},
		{"count", "aggregate", map[string]any{"table": "tickets", "function": "count"}, `[[4]]`, "aggregate returned count of public.tickets on helpdesk: 4.", false},
		{"count of a column skips nulls", "aggregate", map[string]any{"table": "tickets", "function": "count", "column": "note"}, `[[3]]`, "", false},
		{"count by group", "aggregate", map[string]any{"table": "tickets", "function": "count", "group_by": []any{"status"}}, `[["closed",1],["open",2],["pending",1]]`, "aggregate returned count of public.tickets on helpdesk for 3 groups.", false},
		{"sum with a filter", "aggregate", map[string]any{"table": "tickets", "function": "sum", "column": "id", "where": where(cond("status", "=", "open"))}, `[[4]]`, "", false},
		{"avg", "aggregate", map[string]any{"table": "tickets", "function": "avg", "column": "id"}, `[[2.5]]`, "", false},
		{"max", "aggregate", map[string]any{"table": "tickets", "function": "max", "column": "id"}, `[[4]]`, "", false},
	}
	refusals := []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"a table the login may not read", "read_rows", map[string]any{"table": "secrets"}, "read_rows found no table public.secrets on helpdesk that its login may use. Call list_tables for the tables it may use."},
		{"a schema the login may not use", "read_rows", map[string]any{"schema": "hidden", "table": "x"}, "read_rows found no table hidden.x on helpdesk that its login may use. Call list_tables for the tables it may use."},
		{"a table that does not exist", "aggregate", map[string]any{"table": "nope", "function": "count"}, "aggregate found no table public.nope on helpdesk that its login may use. Call list_tables for the tables it may use."},
		{"describe of a table the login may not read", "describe_table", map[string]any{"table": "secrets"}, "describe_table found no table public.secrets on helpdesk that its login may use. Call list_tables for the tables it may use."},
		{"a column that does not exist", "read_rows", map[string]any{"table": "tickets", "columns": []any{"nope"}}, `public.tickets on helpdesk has no column "nope" that the login may use. Call describe_table to see its columns.`},
		{"a where column that does not exist", "read_rows", map[string]any{"table": "tickets", "where": where(cond("nope", "=", 1))}, `public.tickets on helpdesk has no column "nope" that the login may use. Call describe_table to see its columns.`},
		{"a value Postgres cannot read as the column's type", "read_rows", map[string]any{"table": "tickets", "where": where(cond("id", "=", "abc"))}, `read_rows on helpdesk failed, because Postgres said: invalid input syntax for type integer: "abc" (SQLSTATE 22P02). Correct the call and try again.`},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, readFixture...)
			cs, out := served(t, l, d, "off", "")
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(c.args), nil)
					m := out.check(t, c.tool, res)
					if got := rowsJSON(t, m); got != c.rows {
						t.Errorf("rows:\n got %s\nwant %s", got, c.rows)
					}
					if m["truncated"] != c.truncated {
						t.Errorf("truncated is %v, want %v", m["truncated"], c.truncated)
					}
					if got := textOf(t, res); c.text != "" && got != c.text {
						t.Errorf("text:\n got %s\nwant %s", got, c.text)
					}
				})
			}
			for _, c := range refusals {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(c.args), nil)
					if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
				})
			}
			t.Run("the row cap and paging", func(t *testing.T) {
				cs, out := served(t, l, d, "off", "{ rows: 2, bytes: 1048576, timeout: 30s, write_rows: 100 }")
				m := out.check(t, "read_rows", callTool(t, cs, "read_rows", helpdesk(map[string]any{"table": "tickets", "columns": []any{"id"}, "order_by": byID}), nil))
				if rowsJSON(t, m) != `[[1],[2]]` || m["truncated"] != true {
					t.Errorf("read_rows at a row cap of 2 answered %v", m)
				}
				for _, tool := range []string{"list_schemas", "list_tables"} {
					var all []any
					args := helpdesk(map[string]any{})
					for page := 1; ; page++ {
						m := out.check(t, tool, callTool(t, cs, tool, args, nil))
						rows, _ := m["rows"].([]any)
						all = append(all, rows...)
						next, _ := m["nextCursor"].(string)
						if next == "" {
							break
						}
						if len(rows) != 2 || page > 5 {
							t.Fatalf("%s page %d holds %d rows with a nextCursor", tool, page, len(rows))
						}
						args = helpdesk(map[string]any{"cursor": next})
					}
					want := map[string]string{"list_schemas": `[["audit"],["public"]]`, "list_tables": `[["audit","log","table"],["public","kinds","table"],["public","odd \"name\"; DROP TABLE tickets; --","table"],["public","open_tickets","view"],["public","tickets","table"]]`}[tool]
					if got, _ := json.Marshal(all); string(got) != want {
						t.Errorf("%s pages hold %s, want %s", tool, got, want)
					}
				}
			})
			t.Run("the byte cap", func(t *testing.T) {
				cs, out := served(t, l, d, "off", "{ rows: 1000, bytes: 30, timeout: 30s, write_rows: 100 }")
				res := callTool(t, cs, "read_rows", helpdesk(map[string]any{"table": "tickets", "order_by": byID}), nil)
				m := out.check(t, "read_rows", res)
				want := "read_rows returned 1 row of public.tickets on helpdesk. More rows exist beyond the byte cap of 30 bytes. Narrow the where filter or the columns, or page with order_by and a where filter past the last row."
				if rowsJSON(t, m) != `[[1,"open","printer"]]` || m["truncated"] != true || textOf(t, res) != want {
					t.Errorf("read_rows at a byte cap of 30 answered %v %s", m, textOf(t, res))
				}
			})
		})
	}
}
