//go:build pg

package core

import (
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// loadData writes server.yaml in mode straza with free SQL at freeSQL, the
// limits, the gateway hash of gatewayToken and the databases section, and
// loads it with env.
func loadData(t *testing.T, freeSQL, limits, databases string, env map[string]string) *config.Config {
	t.Helper()
	if limits == "" {
		limits = "{ rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }"
	}
	h := hashOf(gatewayToken)
	text := "version: 1\nmode: straza\nlisten: stdio\nfree_sql: \"" + freeSQL + "\"\nlimits: " + limits + "\n" +
		"gateway_tokens_sha256: [\"" + hex.EncodeToString(h[:]) + "\"]\ncatalog: none\ndatabases:\n" + databases
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// run runs each statement on conn as the superuser, with {login} for the quoted name of l.
func run(t *testing.T, d pgtest.Database, l pgtest.Login, sqls ...string) {
	t.Helper()
	admin := d.Connect(t)
	for _, sql := range sqls {
		sql = strings.ReplaceAll(sql, "{login}", pg.Quote(l.Name))
		if _, err := admin.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// helpdeskSingle is the database helpdesk with one login.
const helpdeskSingle = "  helpdesk:\n    login: { dsn_env: HELPDESK_DSN }\n"

func TestWriteCap(t *testing.T) {
	capped := func(tool, n, advice string) string {
		return tool + " on helpdesk would change " + n + " rows and the cap is 100, so nothing was changed. " + advice + ", or ask an administrator to raise write_rows."
	}
	const narrow = "Narrow the where filter"
	const elsewhere = "The statement changes rows in other tables through triggers or cascades, so narrow the call"
	all := []any{map[string]any{"column": "id", "op": ">", "value": 0}}
	var many []any
	for i := range 101 {
		many = append(many, map[string]any{"id": 5000 + i})
	}
	cases := []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"update_rows over the cap", "update_rows", map[string]any{"database": "helpdesk", "table": "tickets", "set": map[string]any{"status": "closed"}, "where": all}, capped("update_rows", "1,240", narrow)},
		{"delete_rows over the cap", "delete_rows", map[string]any{"database": "helpdesk", "table": "tickets", "where": all}, capped("delete_rows", "1,240", narrow)},
		{"delete_rows over the cap with returning", "delete_rows", map[string]any{"database": "helpdesk", "table": "tickets", "where": all, "returning": []any{"id"}}, capped("delete_rows", "1,240", narrow)},
		{"insert_rows over the cap", "insert_rows", map[string]any{"database": "helpdesk", "table": "tickets", "rows": many}, capped("insert_rows", "101", "Send at most 100 rows in one call")},
		{"execute_sql over the cap", "execute_sql", map[string]any{"database": "helpdesk", "sql": "UPDATE tickets SET status = 'closed'"}, capped("execute_sql", "1,240", "Narrow the statement")},
		{"execute_sql that deletes in a WITH query", "execute_sql", map[string]any{"database": "helpdesk", "sql": "WITH gone AS (DELETE FROM tickets RETURNING 1) SELECT count(*) FROM gone"}, capped("execute_sql", "1,240", "Narrow the statement")},
		{"execute_sql that deletes in a function", "execute_sql", map[string]any{"database": "helpdesk", "sql": "SELECT purge()"}, capped("execute_sql", "1,240", "Narrow the statement")},
		{"delete_rows that cascades past the cap", "delete_rows", map[string]any{"database": "helpdesk", "table": "parent", "where": []any{map[string]any{"column": "id", "op": "=", "value": 1}}}, capped("delete_rows", "501", elsewhere)},
		{"insert_rows that a trigger fans out past the cap", "insert_rows", map[string]any{"database": "helpdesk", "table": "fanin", "rows": []any{map[string]any{"id": 2, "v": "b"}}}, capped("insert_rows", "151", elsewhere)},
		{"update_rows that a trigger fans out past the cap", "update_rows", map[string]any{"database": "helpdesk", "table": "fanin", "set": map[string]any{"v": "c"}, "where": []any{map[string]any{"column": "id", "op": "=", "value": 1}}}, capped("update_rows", "151", elsewhere)},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l,
				"CREATE TABLE tickets (id int PRIMARY KEY, status text NOT NULL DEFAULT 'open')",
				"INSERT INTO tickets (id) SELECT generate_series(1, 1240)",
				"CREATE FUNCTION purge() RETURNS void LANGUAGE sql AS 'DELETE FROM tickets'",
				"GRANT SELECT, INSERT, UPDATE, DELETE ON tickets TO {login}",
				"CREATE TABLE parent (id int PRIMARY KEY)",
				"CREATE TABLE child (id serial PRIMARY KEY, parent_id int NOT NULL REFERENCES parent ON DELETE CASCADE)",
				"INSERT INTO parent VALUES (1), (2)",
				"INSERT INTO child (parent_id) SELECT 1 FROM generate_series(1, 500)",
				"CREATE TABLE fanin (id int PRIMARY KEY, v text)",
				"CREATE TABLE fanout (n int)",
				"INSERT INTO fanin VALUES (1, 'a')",
				"CREATE FUNCTION fan() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$BEGIN INSERT INTO fanout SELECT generate_series(1, 150); RETURN NEW; END$$",
				"CREATE TRIGGER fan AFTER INSERT OR UPDATE ON fanin FOR EACH ROW EXECUTE FUNCTION fan()",
				"GRANT SELECT, DELETE ON parent TO {login}",
				"GRANT SELECT, INSERT, UPDATE ON fanin TO {login}")
			srv := New(loadData(t, "read-write", "", helpdeskSingle, map[string]string{"HELPDESK_DSN": l.DSN(t, d)}), slog.New(slog.DiscardHandler))
			cs := overStdio(t, srv, gatewayToken, "")
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, c.args, nil)
					if !res.IsError {
						t.Fatalf("%s answered without isError: %v", c.tool, res.StructuredContent)
					}
					if got := textOf(t, res); got != c.want {
						t.Errorf("the refusal:\n got %s\nwant %s", got, c.want)
					}
				})
			}
			t.Run("a write at the cap passes", func(t *testing.T) {
				res := callTool(t, cs, "update_rows", map[string]any{"database": "helpdesk", "table": "tickets", "set": map[string]any{"status": "closed"}, "where": []any{map[string]any{"column": "id", "op": "<=", "value": 100}}}, nil)
				if res.IsError {
					t.Fatalf("update_rows of 100 rows refused: %s", textOf(t, res))
				}
				if got := structured(t, res)["rowsAffected"]; got != float64(100) {
					t.Errorf("rowsAffected is %v, want 100", got)
				}
			})
			var total, closed int
			if err := d.Connect(t).QueryRow(t.Context(), "SELECT count(*), count(*) FILTER (WHERE status = 'closed') FROM tickets").Scan(&total, &closed); err != nil {
				t.Fatal(err)
			}
			if total != 1240 || closed != 100 {
				t.Errorf("tickets holds %d rows with %d closed, want 1240 with 100 closed, so a refused write changed rows", total, closed)
			}
			var parents, children, fanned, changedFanin int
			if err := d.Connect(t).QueryRow(t.Context(), "SELECT (SELECT count(*) FROM parent), (SELECT count(*) FROM child), (SELECT count(*) FROM fanout), (SELECT count(*) FROM fanin WHERE id <> 1 OR v <> 'a')").Scan(&parents, &children, &fanned, &changedFanin); err != nil {
				t.Fatal(err)
			}
			if parents != 2 || children != 500 || fanned != 0 || changedFanin != 0 {
				t.Errorf("parent, child and fanout hold %d, %d and %d rows and fanin %d changed rows, want 2, 500, 0 and 0, so a write past the cap through a cascade or a trigger committed", parents, children, fanned, changedFanin)
			}
		})
	}
}

// helpdeskSeparate is the database helpdesk with a read and a write login.
const helpdeskSeparate = "  helpdesk:\n    logins:\n      read: { dsn_env: HELPDESK_READ_DSN }\n      write: { dsn_env: HELPDESK_WRITE_DSN }\n"

func TestWriteTools(t *testing.T) {
	in := func(column string, values ...any) []any {
		return []any{map[string]any{"column": column, "op": "in", "value": values}}
	}
	cases := []struct {
		name, tool string
		args       map[string]any
		changed    float64
		rows, text string
	}{
		{"insert takes the default of a column a row leaves out", "insert_rows", map[string]any{"table": "tickets", "rows": []any{map[string]any{"id": 5}}, "returning": []any{"id", "status"}}, 1, `[[5,"open"]]`, "insert_rows inserted 1 row of public.tickets on helpdesk."},
		{"insert of rows that name other columns", "insert_rows", map[string]any{"table": "tickets", "rows": []any{map[string]any{"id": 6, "note": "a"}, map[string]any{"id": 7, "status": "closed"}}, "returning": []any{"id", "status", "note"}}, 2, `[[6,"open","a"],[7,"closed",null]]`, ""},
		{"update with returning", "update_rows", map[string]any{"table": "tickets", "set": map[string]any{"note": "b"}, "where": in("id", 6, 7), "returning": []any{"id", "note"}}, 2, `[[6,"b"],[7,"b"]]`, "update_rows updated 2 rows of public.tickets on helpdesk."},
		{"update without returning", "update_rows", map[string]any{"table": "tickets", "set": map[string]any{"status": "pending", "note": nil}, "where": in("id", 6)}, 1, `[]`, ""},
		{"update that matches nothing", "update_rows", map[string]any{"table": "tickets", "set": map[string]any{"note": "c"}, "where": in("id", 99)}, 0, `[]`, "update_rows updated 0 rows of public.tickets on helpdesk."},
		{"delete with returning", "delete_rows", map[string]any{"table": "tickets", "where": in("id", 5, 6, 7), "returning": []any{"id"}}, 3, `[[5],[6],[7]]`, "delete_rows deleted 3 rows of public.tickets on helpdesk."},
	}
	failures := []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"a table the write login may not use", "insert_rows", map[string]any{"table": "secrets", "rows": []any{map[string]any{"id": 1}}}, "insert_rows found no table public.secrets on helpdesk that its login may use, so the call is refused. Call list_tables to see the tables it may use."},
		{"a table the write login may only read", "insert_rows", map[string]any{"schema": "audit", "table": "log", "rows": []any{map[string]any{"id": 1}}}, "insert_rows on helpdesk failed, because the server answered: permission denied for table log (SQLSTATE 42501), so nothing was changed. Correct the call as the reason says, and call it again."},
		{"a key that is taken", "insert_rows", map[string]any{"table": "tickets", "rows": []any{map[string]any{"id": 1}}}, `insert_rows on helpdesk failed, because the server answered: duplicate key value violates unique constraint "tickets_pkey" (SQLSTATE 23505), so nothing was changed. Correct the call as the reason says, and call it again.`},
		{"a column a row names that the table lacks", "insert_rows", map[string]any{"table": "tickets", "rows": []any{map[string]any{"id": 8, "nope": 1}}}, `public.tickets on helpdesk has no column "nope" that the login may use, so insert_rows is refused. Call describe_table to see its columns.`},
		{"a set column the table lacks", "update_rows", map[string]any{"table": "tickets", "set": map[string]any{"nope": 1}, "where": in("id", 1)}, `public.tickets on helpdesk has no column "nope" that the login may use, so update_rows is refused. Call describe_table to see its columns.`},
		{"a returning column the table lacks", "delete_rows", map[string]any{"table": "tickets", "where": in("id", 1), "returning": []any{"nope"}}, `public.tickets on helpdesk has no column "nope" that the login may use, so delete_rows is refused. Call describe_table to see its columns.`},
		{"a read tool runs on the read login", "read_rows", map[string]any{"table": "drafts"}, "read_rows found no table public.drafts on helpdesk that its login may use, so the call is refused. Call list_tables to see the tables it may use."},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			reader := pgtest.NewLogin(t, s, loginPassword)
			writer := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, writer, readFixture...)
			run(t, d, reader, "GRANT SELECT ON tickets TO {login}")
			run(t, d, writer, "CREATE TABLE drafts (id int)", "GRANT SELECT, INSERT ON drafts TO {login}")
			env := map[string]string{"HELPDESK_READ_DSN": reader.DSN(t, d), "HELPDESK_WRITE_DSN": writer.DSN(t, d)}
			srv := New(loadData(t, "read-write", "", helpdeskSeparate, env), slog.New(slog.DiscardHandler))
			cs := overStdio(t, srv, gatewayToken, "")
			out := outputsOf(t, cs)
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(c.args), nil)
					m := out.check(t, c.tool, res)
					if m["rowsAffected"] != c.changed || rowsJSON(t, m) != c.rows {
						t.Errorf("%s answered %v rows changed and %s, want %v and %s", c.tool, m["rowsAffected"], rowsJSON(t, m), c.changed, c.rows)
					}
					if got := textOf(t, res); c.text != "" && got != c.text {
						t.Errorf("text:\n got %s\nwant %s", got, c.text)
					}
				})
			}
			for _, c := range failures {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(c.args), nil)
					if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
				})
			}
			for tool, want := range map[string]string{"query_sql": reader.Name, "execute_sql": writer.Name} {
				m := out.check(t, tool, callTool(t, cs, tool, helpdesk(map[string]any{"sql": "SELECT current_user"}), nil))
				if got := rowsJSON(t, m); got != `[["`+want+`"]]` {
					t.Errorf("%s runs as %s, want its own login %s", tool, got, want)
				}
			}
		})
	}
}
