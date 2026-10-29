//go:build pg

package core

import (
	"testing"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

func TestExecuteDDL(t *testing.T) {
	notCounted := func(tag string) string {
		return "execute_sql on helpdesk rolled back the statement with command tag " + tag + ", which the write cap cannot count. Use execute_ddl for TRUNCATE and changes of structure."
	}
	onRows := func(tag string) string {
		return "execute_ddl on helpdesk rolled back the statement with command tag " + tag + ", because it changed no structure. Use execute_sql to change rows, or query_sql to read them."
	}
	// Each step runs in order on a table that the login owns, and answers rowsAffected and text, or the refusal.
	steps := []struct {
		name, tool, sql, want string
	}{
		{"execute_ddl creates a table", "execute_ddl", "CREATE TABLE owned (a int)", "execute_ddl ran CREATE TABLE on helpdesk and returned 0 rows."},
		{"execute_sql fills it", "execute_sql", "INSERT INTO owned SELECT generate_series(1, 3)", ""},
		{"execute_ddl creates a function that writes", "execute_ddl", "CREATE FUNCTION purge_owned() RETURNS void LANGUAGE sql AS 'DELETE FROM owned'", ""},
		{"execute_sql refuses TRUNCATE", "execute_sql", "TRUNCATE owned", notCounted("TRUNCATE TABLE")},
		{"execute_sql refuses CREATE TABLE", "execute_sql", "CREATE TABLE refused (a int)", notCounted("CREATE TABLE")},
		{"execute_sql refuses ALTER TABLE", "execute_sql", "ALTER TABLE owned ADD COLUMN refused int", notCounted("ALTER TABLE")},
		{"execute_sql refuses DROP TABLE", "execute_sql", "DROP TABLE owned", notCounted("DROP TABLE")},
		{"execute_ddl refuses INSERT", "execute_ddl", "INSERT INTO owned VALUES (9)", onRows("INSERT 0 1")},
		{"execute_ddl refuses UPDATE", "execute_ddl", "UPDATE owned SET a = 0", onRows("UPDATE 3")},
		{"execute_ddl refuses DELETE", "execute_ddl", "DELETE FROM owned", onRows("DELETE 3")},
		{"execute_ddl refuses a SELECT that calls a function which writes", "execute_ddl", "SELECT purge_owned()", onRows("SELECT 1")},
		{"execute_ddl refuses two statements", "execute_ddl", "ALTER TABLE owned ADD COLUMN b int; DROP TABLE owned", "execute_ddl on helpdesk changed nothing, because Postgres said: cannot insert multiple commands into a prepared statement (SQLSTATE 42601). Correct the call and try again."},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, "GRANT CREATE ON SCHEMA public TO {login}")
			cs, out := served(t, l, d, "read-write", "")
			admin := d.Connect(t)
			state := func() string {
				var rows, columns int
				var refused bool
				if err := admin.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM owned), (SELECT count(*) FROM pg_attribute WHERE attrelid = 'owned'::regclass AND attnum > 0), to_regclass('refused') IS NOT NULL").Scan(&rows, &columns, &refused); err != nil {
					return "unreadable: " + err.Error()
				}
				return jsonOf(t, []any{rows, columns, refused})
			}
			for i, c := range steps {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(map[string]any{"sql": c.sql}), nil)
					if i < 3 {
						out.check(t, c.tool, res)
						if got := textOf(t, res); c.want != "" && got != c.want {
							t.Errorf("text:\n got %s\nwant %s", got, c.want)
						}
						return
					}
					if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
					if got := state(); got != "[3,1,false]" {
						t.Errorf("after the refusal, owned holds rows, columns and a table refused of %s, want [3,1,false], so a refused statement changed something", got)
					}
				})
			}
			for _, c := range []struct{ sql, tag, state string }{
				{"ALTER TABLE owned ADD COLUMN b int", "ALTER TABLE", "[3,2,false]"},
				{"TRUNCATE owned", "TRUNCATE TABLE", "[0,2,false]"},
			} {
				res := callTool(t, cs, "execute_ddl", helpdesk(map[string]any{"sql": c.sql}), nil)
				m := out.check(t, "execute_ddl", res)
				if got, want := textOf(t, res), "execute_ddl ran "+c.tag+" on helpdesk and returned 0 rows."; got != want || m["rowsAffected"] != float64(0) {
					t.Errorf("execute_ddl %s answered %s and %v rows changed, want %s and 0", c.sql, got, m["rowsAffected"], want)
				}
				if got := state(); got != c.state {
					t.Errorf("after %s, owned holds %s, want %s", c.sql, got, c.state)
				}
			}
			out.check(t, "execute_ddl", callTool(t, cs, "execute_ddl", helpdesk(map[string]any{"sql": "DROP FUNCTION purge_owned()"}), nil))
			out.check(t, "execute_ddl", callTool(t, cs, "execute_ddl", helpdesk(map[string]any{"sql": "DROP TABLE owned"}), nil))
			var gone bool
			if err := admin.QueryRow(t.Context(), "SELECT to_regclass('owned') IS NULL").Scan(&gone); err != nil || !gone {
				t.Errorf("owned is still there after execute_ddl DROP TABLE (%v)", err)
			}
		})
	}
}

func TestRowWorkGoesThroughExecuteSQL(t *testing.T) {
	const capped = "execute_sql on helpdesk would change 150 rows, over the cap of 100, and changed nothing. Narrow the statement, or ask an administrator to raise limits.write_rows in server.yaml."
	onRows := func(tag string) string {
		return "execute_ddl on helpdesk rolled back the statement with command tag " + tag + ", because it changed no structure. Use execute_sql to change rows, or query_sql to read them."
	}
	cases := []struct {
		name, tool, sql string
		// changed is the rowsAffected of a call that passes, and want the refusal of one that does not.
		changed float64
		want    string
		left    int
	}{
		{"a procedure past the cap", "execute_sql", "CALL purge_big(150)", 0, capped, 150},
		{"a DO block past the cap", "execute_sql", "DO $$BEGIN DELETE FROM big WHERE id <= 150; END$$", 0, capped, 150},
		{"an EXPLAIN ANALYZE of a DELETE past the cap", "execute_sql", "EXPLAIN ANALYZE DELETE FROM big WHERE id <= 150", 0, capped, 150},
		{"execute_ddl refuses a procedure", "execute_ddl", "CALL purge_big(150)", 0, onRows("CALL"), 150},
		{"execute_ddl refuses a DO block", "execute_ddl", "DO $$BEGIN DELETE FROM big WHERE id <= 150; END$$", 0, onRows("DO"), 150},
		{"execute_ddl refuses an EXPLAIN", "execute_ddl", "EXPLAIN ANALYZE DELETE FROM big WHERE id <= 150", 0, onRows("EXPLAIN"), 150},
		{"execute_ddl refuses a COPY", "execute_ddl", "COPY (SELECT 1) TO STDOUT", 0, onRows("COPY 1"), 150},
		{"a procedure under the cap", "execute_sql", "CALL purge_big(10)", 10, "", 140},
		{"a DO block under the cap", "execute_sql", "DO $$BEGIN DELETE FROM big WHERE id <= 20; END$$", 10, "", 130},
		{"an EXPLAIN ANALYZE of a DELETE under the cap", "execute_sql", "EXPLAIN ANALYZE DELETE FROM big WHERE id <= 30", 10, "", 120},
		{"a COPY to the client", "execute_sql", "COPY (SELECT 1) TO STDOUT", 0, "", 120},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l,
				"CREATE TABLE big (id int)",
				"INSERT INTO big SELECT generate_series(1, 150)",
				"CREATE PROCEDURE purge_big(n int) LANGUAGE sql AS 'DELETE FROM big WHERE id <= n'",
				"GRANT SELECT, DELETE ON big TO {login}")
			cs, out := served(t, l, d, "read-write", "")
			admin := d.Connect(t)
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(map[string]any{"sql": c.sql}), nil)
					if c.want != "" {
						if got := textOf(t, res); !res.IsError || got != c.want {
							t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
						}
					} else if m := out.check(t, c.tool, res); m["rowsAffected"] != c.changed {
						t.Errorf("%s answered %v rows changed, want %v", c.tool, m["rowsAffected"], c.changed)
					}
					var left int
					if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM big").Scan(&left); err != nil || left != c.left {
						t.Errorf("big holds %d rows (%v), want %d", left, err, c.left)
					}
				})
			}
		})
	}
}

func TestStructureChangesStayOutOfExecuteSQL(t *testing.T) {
	structure := func(tag string) string {
		return "execute_sql on helpdesk rolled back the statement with command tag " + tag + ", because it changed the system catalogs. Use execute_ddl for TRUNCATE and changes of structure."
	}
	cases := []struct {
		name, tool, sql string
		// want is the refusal, or empty for a call that passes.
		want string
	}{
		{"a DO block that truncates", "execute_sql", "DO $$BEGIN TRUNCATE public.plain; END$$", structure("DO")},
		{"a DO block that alters", "execute_sql", "DO $$BEGIN ALTER TABLE public.plain ADD COLUMN extra int; END$$", structure("DO")},
		{"a procedure that alters", "execute_sql", "CALL alter_plain()", structure("CALL")},
		{"a function in a SELECT that truncates", "execute_sql", "SELECT trunc_plain()", structure("SELECT 1")},
		{"CREATE TABLE AS, which answers SELECT", "execute_sql", "CREATE TABLE copy AS SELECT * FROM plain", structure("SELECT 500")},
		{"an INSERT of a toasted value", "execute_sql", "INSERT INTO toasted SELECT 2, string_agg(md5(i::text), '') FROM generate_series(1, 4000) i", ""},
		{"an UPDATE of a toasted value", "execute_sql", "UPDATE toasted SET body = body || 'x'", ""},
		{"a DELETE of a toasted value", "execute_sql", "DELETE FROM toasted WHERE id = 2", ""},
		{"nextval", "execute_sql", "SELECT nextval('seq')", ""},
		{"execute_ddl refreshes a materialized view larger than the cap", "execute_ddl", "REFRESH MATERIALIZED VIEW copied", ""},
		{"execute_ddl caps the rows a refresh writes through a function", "execute_ddl", "REFRESH MATERIALIZED VIEW writes", "execute_ddl on helpdesk would change 150 rows, over the cap of 100, and changed nothing. Run row changes through execute_sql, or ask an administrator to raise limits.write_rows in server.yaml."},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, catalogFixture...)
			run(t, d, l,
				"ALTER TABLE plain OWNER TO {login}", "ALTER TABLE toasted OWNER TO {login}", "ALTER SEQUENCE seq OWNER TO {login}",
				"ALTER TABLE sink OWNER TO {login}", "ALTER MATERIALIZED VIEW copied OWNER TO {login}", "ALTER MATERIALIZED VIEW writes OWNER TO {login}",
				"GRANT CREATE ON SCHEMA public TO {login}")
			cs, out := served(t, l, d, "read-write", "")
			admin := d.Connect(t)
			state := func() string {
				var rows, columns, sunk int
				var copied bool
				if err := admin.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM plain), (SELECT count(*) FROM pg_attribute WHERE attrelid = 'plain'::regclass AND attnum > 0), (SELECT count(*) FROM sink), to_regclass('copy') IS NOT NULL").Scan(&rows, &columns, &sunk, &copied); err != nil {
					return "unreadable: " + err.Error()
				}
				return jsonOf(t, []any{rows, columns, sunk, copied})
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(map[string]any{"sql": c.sql}), nil)
					if c.want == "" {
						out.check(t, c.tool, res)
					} else if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
					if got := state(); got != "[500,2,0,false]" {
						t.Errorf("plain, sink and copy hold %s, want [500,2,0,false], so a refused statement changed something", got)
					}
				})
			}
		})
	}
}

func TestExecuteDDLRunsStructureThatAnswersRows(t *testing.T) {
	structure := func(tag string) string {
		return "execute_sql on helpdesk rolled back the statement with command tag " + tag + ", because it changed the system catalogs. Use execute_ddl for TRUNCATE and changes of structure."
	}
	const mixed = "DO $$BEGIN CREATE TABLE made (a int); DELETE FROM public.plain WHERE id <= 150; END$$"
	cases := []struct {
		name, tool, sql string
		// want is the refusal, or empty for a call that passes. made names the relation the call leaves behind.
		want, made string
	}{
		{"execute_sql refuses CREATE TABLE AS", "execute_sql", "CREATE TABLE copy AS SELECT * FROM plain", structure("SELECT 500"), ""},
		{"execute_sql refuses SELECT INTO", "execute_sql", "SELECT * INTO copy2 FROM plain", structure("SELECT 500"), ""},
		{"execute_sql refuses CREATE MATERIALIZED VIEW AS", "execute_sql", "CREATE MATERIALIZED VIEW mv2 AS SELECT * FROM plain", structure("SELECT 500"), ""},
		{"execute_sql refuses a DO block that creates and deletes", "execute_sql", mixed, structure("DO"), ""},
		{"execute_ddl refuses a DO block that creates a table and deletes past the cap", "execute_ddl", mixed, "execute_ddl on helpdesk would change 150 rows, over the cap of 100, and changed nothing. Run row changes through execute_sql, or ask an administrator to raise limits.write_rows in server.yaml.", ""},
		{"execute_ddl refuses pure row work", "execute_ddl", "DELETE FROM plain WHERE id = 1", "execute_ddl on helpdesk rolled back the statement with command tag DELETE 1, because it changed no structure. Use execute_sql to change rows, or query_sql to read them.", ""},
		{"execute_ddl runs CREATE TABLE AS of 500 rows past the cap", "execute_ddl", "CREATE TABLE copy AS SELECT * FROM plain", "", "copy"},
		{"execute_ddl runs SELECT INTO past the cap", "execute_ddl", "SELECT * INTO copy2 FROM plain", "", "copy2"},
		{"execute_ddl runs CREATE MATERIALIZED VIEW AS past the cap", "execute_ddl", "CREATE MATERIALIZED VIEW mv2 AS SELECT * FROM plain", "", "mv2"},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, catalogFixture...)
			run(t, d, l, "ALTER TABLE plain OWNER TO {login}", "GRANT CREATE ON SCHEMA public TO {login}")
			cs, out := served(t, l, d, "read-write", "")
			admin := d.Connect(t)
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(map[string]any{"sql": c.sql}), nil)
					if c.want == "" {
						if m := out.check(t, c.tool, res); m["rowsAffected"] != float64(0) {
							t.Errorf("%s answered %v rows changed, want 0, since it wrote only into a relation it made", c.tool, m["rowsAffected"])
						}
					} else if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
					var plain, made int
					var other bool
					if err := admin.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM plain), to_regclass('made') IS NOT NULL").Scan(&plain, &other); err != nil || plain != 500 || other {
						t.Errorf("plain holds %d rows and made exists %v (%v), want 500 and false", plain, other, err)
					}
					if c.made != "" {
						if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM "+c.made).Scan(&made); err != nil || made != 500 {
							t.Errorf("%s holds %d rows (%v), want 500", c.made, made, err)
						}
					}
				})
			}
		})
	}
}

func TestWriteToolsRefuseATriggerThatChangesStructure(t *testing.T) {
	refused := func(tool, tag string) string {
		return tool + " on helpdesk rolled back the statement with command tag " + tag + ", because it changed the system catalogs. A trigger on the table changes structure. Ask an administrator to change the trigger, or use execute_ddl."
	}
	one := []any{map[string]any{"column": "id", "op": "=", "value": 1}}
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"insert_rows", map[string]any{"table": "guarded", "rows": []any{map[string]any{"id": 2, "v": "b"}}}, refused("insert_rows", "INSERT 0 1")},
		{"update_rows", map[string]any{"table": "guarded", "set": map[string]any{"v": "c"}, "where": one}, refused("update_rows", "UPDATE 1")},
		{"delete_rows", map[string]any{"table": "guarded", "where": one}, refused("delete_rows", "DELETE 1")},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l,
				"CREATE TABLE guarded (id int PRIMARY KEY, v text)",
				"INSERT INTO guarded VALUES (1, 'a')",
				"CREATE TABLE victim (id int)",
				"INSERT INTO victim VALUES (1), (2), (3)",
				"CREATE FUNCTION trunc_victim() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$BEGIN TRUNCATE public.victim; RETURN NULL; END$$",
				"CREATE TRIGGER trunc_victim AFTER INSERT OR UPDATE OR DELETE ON guarded FOR EACH ROW EXECUTE FUNCTION trunc_victim()",
				"GRANT SELECT, INSERT, UPDATE, DELETE ON guarded TO {login}")
			cs, _ := served(t, l, d, "off", "")
			admin := d.Connect(t)
			for _, c := range cases {
				t.Run(c.tool, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(c.args), nil)
					if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
					var victims, guarded int
					var v string
					if err := admin.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM victim), (SELECT count(*) FROM guarded), (SELECT v FROM guarded WHERE id = 1)").Scan(&victims, &guarded, &v); err != nil || victims != 3 || guarded != 1 || v != "a" {
						t.Errorf("victim holds %d rows and guarded %d with v %q (%v), want 3, 1 and a, so the refused write changed something", victims, guarded, v, err)
					}
				})
			}
		})
	}
}
