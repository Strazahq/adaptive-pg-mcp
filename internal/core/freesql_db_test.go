//go:build pg

package core

import (
	"testing"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

func TestFreeSQL(t *testing.T) {
	failed := func(tool, reason, done string) string {
		return tool + " on helpdesk failed, because the server answered: " + reason + ", so nothing was " + done + ". Correct the call as the reason says, and call it again."
	}
	const two = "cannot insert multiple commands into a prepared statement (SQLSTATE 42601)"
	answers := []struct {
		name, tool, sql string
		changed         any
		rows, text      string
	}{
		{"query_sql reads", "query_sql", "SELECT id, status FROM tickets ORDER BY id", nil, `[[1,"open"],[2,"closed"],[3,"open"],[4,"pending"]]`, "query_sql answered 4 rows from helpdesk."},
		{"query_sql names a type pgx does not know", "query_sql", "SELECT 'happy'::mood AS feeling", nil, `[["happy"]]`, ""},
		{"execute_sql reads and changes nothing", "execute_sql", "SELECT count(*) FROM tickets", float64(0), `[[4]]`, "execute_sql changed 0 rows on helpdesk and answered 1 row."},
		{"execute_sql inserts with returning", "execute_sql", "INSERT INTO tickets VALUES (9, 'open', 'x') RETURNING id", float64(1), `[[9]]`, "execute_sql changed 1 row on helpdesk and answered 1 row."},
		{"execute_sql deletes in a WITH query under the cap", "execute_sql", "WITH gone AS (DELETE FROM tickets WHERE id = 9 RETURNING 1) SELECT count(*) FROM gone", float64(1), `[[1]]`, ""},
	}
	refusals := []struct {
		name, tool, sql, want string
	}{
		{"query_sql refuses two statements", "query_sql", "SELECT 1; SELECT 2", failed("query_sql", two, "read")},
		{"execute_sql refuses two statements", "execute_sql", "SELECT 1; DELETE FROM tickets", failed("execute_sql", two, "changed")},
		{"execute_sql refuses a statement after a comment", "execute_sql", "SELECT 1 /* ; */; DELETE FROM tickets", failed("execute_sql", two, "changed")},
		{"query_sql runs read-only", "query_sql", "DELETE FROM tickets", failed("query_sql", "cannot execute DELETE in a read-only transaction (SQLSTATE 25006)", "read")},
		{"query_sql cannot switch to read-write", "query_sql", "SET TRANSACTION READ WRITE", failed("query_sql", "transaction read-write mode must be set before any query (SQLSTATE 25001)", "read")},
		{"a procedure cannot commit inside the call's transaction", "execute_sql", "CALL wipe()", failed("execute_sql", "invalid transaction termination (SQLSTATE 2D000)", "changed")},
		{"execute_sql refuses a change of structure", "execute_sql", "CREATE TABLE made (a int)", "execute_sql on helpdesk ran a statement whose command tag is CREATE TABLE, which the write cap cannot count, so it was rolled back and nothing was changed. Use execute_ddl for TRUNCATE and for statements that change structure."},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			other := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, readFixture...)
			run(t, d, l,
				"CREATE TYPE mood AS ENUM ('happy', 'sad')",
				"CREATE PROCEDURE wipe() LANGUAGE plpgsql AS 'BEGIN DELETE FROM tickets; COMMIT; END'",
				"GRANT CREATE ON SCHEMA public TO {login}",
				"GRANT "+pg.Quote(other.Name)+" TO {login}")
			cs, out := served(t, l, d, "read-write", "")
			for _, c := range answers {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(map[string]any{"sql": c.sql}), nil)
					m := out.check(t, c.tool, res)
					if got := rowsJSON(t, m); got != c.rows || (c.changed != nil && m["rowsAffected"] != c.changed) {
						t.Errorf("%s answered %s with %v rows changed, want %s and %v", c.tool, got, m["rowsAffected"], c.rows, c.changed)
					}
					if got := textOf(t, res); c.text != "" && got != c.text {
						t.Errorf("text:\n got %s\nwant %s", got, c.text)
					}
				})
			}
			for _, c := range refusals {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(map[string]any{"sql": c.sql}), nil)
					if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
				})
			}
			var n int
			if err := d.Connect(t).QueryRow(t.Context(), "SELECT count(*) FROM tickets").Scan(&n); err != nil || n != 4 {
				t.Errorf("tickets holds %d rows (%v), want 4, so a refused statement changed it", n, err)
			}

			t.Run("what a statement changes in its session reaches no later call", func(t *testing.T) {
				for _, change := range []struct{ tool, sql string }{
					{"query_sql", "SELECT set_config('search_path', 'audit', false)"},
					{"query_sql", "SET ROLE " + pg.Quote(other.Name)},
					{"execute_ddl", "SET search_path TO audit"},
					{"execute_ddl", "CREATE TEMP TABLE scratch (a int)"},
				} {
					if res := callTool(t, cs, change.tool, helpdesk(map[string]any{"sql": change.sql}), nil); res.IsError {
						t.Fatalf("%s %s refused: %s", change.tool, change.sql, textOf(t, res))
					}
				}
				for _, tool := range []string{"query_sql", "execute_sql"} {
					for range 3 {
						m := out.check(t, tool, callTool(t, cs, tool, helpdesk(map[string]any{"sql": "SELECT current_user::text, current_setting('search_path'), to_regclass('pg_temp.scratch') IS NULL"}), nil))
						if got, want := rowsJSON(t, m), `[["`+l.Name+`","\"$user\", public",true]]`; got != want {
							t.Errorf("%s after the session changes answered %s, want %s", tool, got, want)
						}
					}
				}
			})

			t.Run("the row cap stops a free read", func(t *testing.T) {
				cs, out := served(t, l, d, "read", "{ rows: 2, bytes: 1048576, timeout: 30s, write_rows: 100 }")
				res := callTool(t, cs, "query_sql", helpdesk(map[string]any{"sql": "SELECT g FROM generate_series(1, 100000) g"}), nil)
				m := out.check(t, "query_sql", res)
				want := "query_sql answered 2 rows from helpdesk. More rows follow, and the answer stopped at the row cap of 2. Add a LIMIT, or select fewer columns."
				if rowsJSON(t, m) != `[[1],[2]]` || m["truncated"] != true || textOf(t, res) != want {
					t.Errorf("query_sql at a row cap of 2 answered %v %s", m, textOf(t, res))
				}
			})
		})
	}
}
