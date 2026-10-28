//go:build pg

package core

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

func TestIDCheckInDataTools(t *testing.T) {
	one := func(column string, value any) []any {
		return []any{map[string]any{"column": column, "op": "=", "value": value}}
	}
	// Each call with the wrong id would change the table if it ran, and each call with the right id changes it in a known way.
	cases := []struct {
		tool        string
		wrong, good map[string]any
	}{
		{"list_schemas", map[string]any{"database": "sales_eu"}, nil},
		{"list_tables", map[string]any{"database": "sales_eu"}, nil},
		{"describe_table", map[string]any{"database": "sales_eu", "table": "tickets"}, nil},
		{"read_rows", map[string]any{"database": "sales_eu", "table": "tickets"}, nil},
		{"aggregate", map[string]any{"database": "sales_eu", "table": "tickets", "function": "count"}, nil},
		{"query_sql", map[string]any{"database": "sales_eu", "sql": "SELECT count(*) FROM tickets"}, nil},
		{"insert_rows",
			map[string]any{"database": "sales_eu", "table": "tickets", "rows": []any{map[string]any{"id": 20, "status": "wrong"}}},
			map[string]any{"database": "sales_eu", "table": "tickets", "rows": []any{map[string]any{"id": 10, "status": "new"}}}},
		{"update_rows",
			map[string]any{"database": "sales_eu", "table": "tickets", "set": map[string]any{"status": "wrong"}, "where": one("id", 1)},
			map[string]any{"database": "sales_eu", "table": "tickets", "set": map[string]any{"status": "seen"}, "where": one("id", 10)}},
		{"delete_rows",
			map[string]any{"database": "sales_eu", "table": "tickets", "where": one("id", 2)},
			map[string]any{"database": "sales_eu", "table": "tickets", "where": one("status", "seen")}},
		{"execute_sql",
			map[string]any{"database": "sales_eu", "sql": "DELETE FROM tickets WHERE id = 3"},
			map[string]any{"database": "sales_eu", "sql": "INSERT INTO tickets VALUES (11, 'sql')"}},
		{"execute_ddl",
			map[string]any{"database": "sales_eu", "sql": "TRUNCATE tickets"},
			map[string]any{"database": "sales_eu", "sql": "CREATE TABLE made_by_ddl (a int)"}},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			other := pgtest.NewDatabase(t, s)
			run(t, d, l,
				"CREATE TABLE tickets (id int PRIMARY KEY, status text NOT NULL)",
				"INSERT INTO tickets VALUES (1, 'open'), (2, 'open'), (3, 'open')",
				"GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON tickets TO {login}",
				"GRANT CREATE ON SCHEMA public TO {login}")
			srv := New(loadData(t, "read-write", "", single, map[string]string{"SALES_EU_DSN": l.DSN(t, d)}), slog.New(slog.DiscardHandler))
			cs := overStdio(t, srv, gatewayToken, "")
			right, wrong := mcp.Meta{objectIDKey: idOf(t, d)}, mcp.Meta{objectIDKey: idOf(t, other)}
			for _, c := range cases {
				t.Run(c.tool, func(t *testing.T) {
					res := callTool(t, cs, c.tool, c.wrong, wrong)
					if !res.IsError || textOf(t, res) != recreated {
						t.Errorf("%s with the id of another database answered %v %s, want the refusal %s", c.tool, res.IsError, textOf(t, res), recreated)
					}
					if res := callTool(t, cs, c.tool, c.wrong, mcp.Meta{objectIDKey: 7}); !res.IsError || textOf(t, res) != notAString {
						t.Errorf("%s with an id that is not a string answered %v %s, want the refusal %s", c.tool, res.IsError, textOf(t, res), notAString)
					}
					good := c.good
					if good == nil {
						good = c.wrong
					}
					if res := callTool(t, cs, c.tool, good, right); res.IsError {
						t.Errorf("%s with the right id refused: %s", c.tool, textOf(t, res))
					}
				})
			}
			rows, err := d.Connect(t).Query(t.Context(), "SELECT id, status FROM tickets ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			got := map[int]string{}
			for rows.Next() {
				var id int
				var status string
				if err := rows.Scan(&id, &status); err != nil {
					t.Fatal(err)
				}
				got[id] = status
			}
			if want := map[int]string{1: "open", 2: "open", 3: "open", 11: "sql"}; !reflect.DeepEqual(got, want) {
				t.Errorf("tickets holds %v, want %v, so a call with the wrong id changed it or one with the right id did not", got, want)
			}
		})
	}
}
