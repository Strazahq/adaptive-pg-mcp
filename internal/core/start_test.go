//go:build pg

package core

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// loginPassword is the password of every login the core tests make.
const loginPassword = "pw-core-tests-0d5e"

// logins are the login roles of one harness server that the start checks run on.
type logins struct {
	plain, other, super, member, bypass, createrole, program, monitor pgtest.Login
}

// newLogins makes the logins on s, before any database of the test exists.
func newLogins(t *testing.T, s pgtest.Server) logins {
	t.Helper()
	super := pgtest.NewLogin(t, s, loginPassword, "SUPERUSER")
	return logins{
		plain:      pgtest.NewLogin(t, s, loginPassword),
		other:      pgtest.NewLogin(t, s, loginPassword),
		super:      super,
		member:     pgtest.NewLogin(t, s, loginPassword, "IN ROLE "+pg.Quote(super.Name)),
		bypass:     pgtest.NewLogin(t, s, loginPassword, "BYPASSRLS"),
		createrole: pgtest.NewLogin(t, s, loginPassword, "CREATEROLE"),
		program:    pgtest.NewLogin(t, s, loginPassword, "IN ROLE pg_execute_server_program, pg_write_server_files"),
		monitor:    pgtest.NewLogin(t, s, loginPassword, "IN ROLE pg_monitor"),
	}
}

// setup is the SQL a case runs as the superuser in its database, with {plain} and {other} for those logins' quoted names.
func setup(t *testing.T, d pgtest.Database, l logins, sqls ...string) {
	t.Helper()
	admin := d.Connect(t)
	r := strings.NewReplacer("{plain}", pg.Quote(l.plain.Name), "{other}", pg.Quote(l.other.Name), "{monitor}", pg.Quote(l.monitor.Name))
	for _, sql := range sqls {
		if _, err := admin.Exec(t.Context(), r.Replace(sql)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// loadConfig writes server.yaml in mode straza with freeSQL and the databases section, and loads it with env.
func loadConfig(t *testing.T, freeSQL, databases string, env map[string]string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.yaml")
	text := `version: 1
mode: straza
listen: stdio
free_sql: "` + freeSQL + `"
limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }
gateway_tokens_sha256: ["` + strings.Repeat("0", 64) + `"]
catalog: none
databases:
` + databases
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

const (
	// single is a database with one login.
	single = "  sales_eu:\n    login: { dsn_env: SALES_EU_DSN }\n"
	// separate is a database with a read and a write login.
	separate = "  helpdesk:\n    logins:\n      read: { dsn_env: HELPDESK_READ_DSN }\n      write: { dsn_env: HELPDESK_WRITE_DSN }\n"
	// extra is a database with a read, a write and a catalog login.
	extra = separate + "      finance_reader: { dsn_env: HELPDESK_FINANCE_DSN }\n"
	// allowCross accepts dblink and postgres_fdw on the database before it.
	allowCross = "    allow_cross_database: true\n"
	// lock is the end of every line about a login that can write where it reads.
	lock = "so its read tools rely on read-only transactions alone."
)

// refuses is a finding that refuses the start.
func refuses(text string) Finding { return Finding{Refuses: true, Text: text} }

// reports is a finding that is reported and does not refuse the start.
func reports(text string) Finding { return Finding{Text: text} }

func TestStartChecks(t *testing.T) {
	type env struct {
		l          logins
		d, another pgtest.Database
	}
	cases := []struct {
		name      string
		freeSQL   string
		databases string
		another   bool
		sql       []string
		// anotherSQL runs as the superuser in the second database.
		anotherSQL []string
		dsns       func(e env) map[string]string
		want       func(e env) []Finding
	}{
		{
			name: "a login with SELECT only passes", freeSQL: "read-write", databases: single,
			sql:  []string{"CREATE TABLE a (x int)", "CREATE TABLE b (x int)", "GRANT SELECT ON a, b TO {plain}"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding { return nil },
		},
		{
			name: "a superuser with free SQL read refuses the start", freeSQL: "read", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.super.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{refuses("the login of sales_eu is a superuser and free SQL is on, so adaptive-pg-mcp will not start. Use a login without superuser, turn free_sql off, or set allow_superuser: true for sales_eu.")}
			},
		},
		{
			name: "a superuser with free SQL read-write refuses the start", freeSQL: "read-write", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.super.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{refuses("the login of sales_eu is a superuser and free SQL is on, so adaptive-pg-mcp will not start. Use a login without superuser, turn free_sql off, or set allow_superuser: true for sales_eu.")}
			},
		},
		{
			name: "allow_superuser accepts a superuser and reports it", freeSQL: "read", databases: single + "    allow_superuser: true\n",
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.super.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports("the login of sales_eu is a superuser and free SQL is on, which allow_superuser: true accepts for sales_eu. A superuser's free SQL is not limited to the database, so a login without superuser is the safer setup.")}
			},
		},
		{
			name: "a superuser with free SQL off is reported", freeSQL: "off", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.super.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports("the login of sales_eu is a superuser, so no grant of the database limits its calls. A login without superuser adds the database's own lock.")}
			},
		},
		{
			name: "a superuser write login refuses the start by its name", freeSQL: "read-write", databases: separate,
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.super.DSN(t, e.d)}
			},
			want: func(env) []Finding {
				return []Finding{refuses("the write login of helpdesk is a superuser and free SQL is on, so adaptive-pg-mcp will not start. Use a login without superuser, turn free_sql off, or set allow_superuser: true for helpdesk.")}
			},
		},
		{
			name: "a member of a superuser role refuses the start", freeSQL: "read", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.member.DSN(t, e.d)} },
			want: func(e env) []Finding {
				return []Finding{refuses("the login of sales_eu can switch to the superuser role " + e.l.super.Name + " and free SQL is on, so adaptive-pg-mcp will not start. Use a login that is not a member of a superuser role, turn free_sql off, or set allow_superuser: true for sales_eu.")}
			},
		},
		{
			name: "a member of a superuser role with free SQL off passes", freeSQL: "off", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.member.DSN(t, e.d)} },
			want: func(env) []Finding { return nil },
		},
		{
			name: "dblink refuses the start", freeSQL: "off", databases: single, sql: []string{"CREATE EXTENSION dblink"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{refuses("sales_eu has dblink installed, which lets a call reach another database, so adaptive-pg-mcp will not start. Drop the extension from sales_eu, or set allow_cross_database: true for sales_eu.")}
			},
		},
		{
			name: "postgres_fdw refuses the start", freeSQL: "off", databases: single, sql: []string{"CREATE EXTENSION postgres_fdw"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{refuses("sales_eu has postgres_fdw installed, which lets a call reach another database, so adaptive-pg-mcp will not start. Drop the extension from sales_eu, or set allow_cross_database: true for sales_eu.")}
			},
		},
		{
			name: "both extensions refuse the start in one sentence", freeSQL: "off", databases: separate,
			sql: []string{"CREATE EXTENSION postgres_fdw", "CREATE EXTENSION dblink"},
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.d)}
			},
			want: func(env) []Finding {
				return []Finding{refuses("helpdesk has dblink and postgres_fdw installed, which let a call reach another database, so adaptive-pg-mcp will not start. Drop the extensions from helpdesk, or set allow_cross_database: true for helpdesk.")}
			},
		},
		{
			name: "allow_cross_database accepts dblink and reports it", freeSQL: "off", databases: single + "    allow_cross_database: true\n",
			sql:  []string{"CREATE EXTENSION dblink"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports("sales_eu has dblink installed, which allow_cross_database: true accepts for sales_eu. A call can reach another database through it.")}
			},
		},
		{
			name: "a login that can write to every table is reported", freeSQL: "read", databases: single,
			sql:  []string{"CREATE TABLE a (x int)", "CREATE TABLE b (x int)", "CREATE TABLE c (x int)", "GRANT SELECT, INSERT ON a, b, c TO {plain}"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports("the login of sales_eu can write to all 3 tables, " + lock + " A separate read login with SELECT only adds the database's own lock.")}
			},
		},
		{
			name: "a login that owns its only table is reported", freeSQL: "read", databases: single,
			sql:  []string{"CREATE SCHEMA app AUTHORIZATION {plain}", "CREATE TABLE app.a (x int)", "ALTER TABLE app.a OWNER TO {plain}"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports("the login of sales_eu can write to its only table, " + lock + " A separate read login with SELECT only adds the database's own lock.")}
			},
		},
		{
			name: "a read login that can write to some tables is reported, and the write login is not", freeSQL: "read-write", databases: separate,
			sql: []string{"CREATE TABLE a (x int)", "CREATE TABLE b (x int)", "CREATE TABLE c (x int)", "GRANT SELECT ON a, b, c TO {plain}", "GRANT UPDATE (x) ON b TO {plain}", "GRANT ALL ON a, b, c TO {other}"},
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.d)}
			},
			want: func(env) []Finding {
				return []Finding{reports("the read login of helpdesk can write to 1 of its 3 tables, so on those its read tools rely on read-only transactions alone. Granting it SELECT only adds the database's own lock.")}
			},
		},
		{
			name: "a table in a schema the login cannot use does not count", freeSQL: "read", databases: single,
			sql:  []string{"CREATE SCHEMA hidden", "CREATE TABLE hidden.a (x int)", "GRANT INSERT ON hidden.a TO {plain}", "CREATE TABLE b (x int)", "GRANT SELECT ON b TO {plain}"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding { return nil },
		},
		{
			name: "BYPASSRLS is reported", freeSQL: "off", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.bypass.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports("the login of sales_eu has BYPASSRLS, so no row-level security policy limits what its calls read or change. A login without BYPASSRLS keeps the policies in force.")}
			},
		},
		{
			name: "CREATEROLE with free SQL on is reported", freeSQL: "read", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.createrole.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports("the login of sales_eu has CREATEROLE and free SQL is on, so a call can create and change roles of the whole database server, not only of sales_eu. A login without CREATEROLE removes that reach.")}
			},
		},
		{
			name: "CREATEROLE with free SQL off is not reported", freeSQL: "off", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.createrole.DSN(t, e.d)} },
			want: func(env) []Finding { return nil },
		},
		{
			name: "membership in server program and file roles with free SQL on is reported", freeSQL: "read", databases: single,
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.program.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports("the login of sales_eu is a member of pg_execute_server_program and pg_write_server_files, and free SQL is on, so a call can reach the programs or files of the database server, not only sales_eu. A login without that membership removes that reach.")}
			},
		},
		{
			name: "a wrong password refuses the start", freeSQL: "off", databases: single,
			dsns: func(e env) map[string]string {
				return map[string]string{"SALES_EU_DSN": strings.Replace(e.l.plain.DSN(t, e.d), loginPassword, loginPassword+"x", 1)}
			},
			want: func(e env) []Finding {
				return []Finding{refuses(`the login of sales_eu cannot connect, because the server answered: password authentication failed for user "` + e.l.plain.Name + `" (SQLSTATE 28P01), so adaptive-pg-mcp will not start. Check the connection string in SALES_EU_DSN, and that the database server is up and accepts the login.`)}
			},
		},
		{
			name: "a database that does not exist refuses the start", freeSQL: "off", databases: single,
			dsns: func(e env) map[string]string {
				return map[string]string{"SALES_EU_DSN": strings.Replace(e.l.plain.DSN(t, e.d), e.d.Name, e.d.Name+"_gone", 1)}
			},
			want: func(e env) []Finding {
				return []Finding{refuses(`the login of sales_eu cannot connect, because the server answered: database "` + e.d.Name + `_gone" does not exist (SQLSTATE 3D000), so adaptive-pg-mcp will not start. Check the connection string in SALES_EU_DSN, and that the database server is up and accepts the login.`)}
			},
		},
		{
			name: "a port where nothing listens refuses the start", freeSQL: "off", databases: single,
			dsns: func(e env) map[string]string {
				return map[string]string{"SALES_EU_DSN": "postgres://nobody:" + loginPassword + "@127.0.0.1:1/sales_eu?sslmode=disable&connect_timeout=5"}
			},
			want: func(env) []Finding {
				return []Finding{refuses("the login of sales_eu cannot connect, because nothing accepts connections at the host and port of the connection string, so adaptive-pg-mcp will not start. Check the connection string in SALES_EU_DSN, and that the database server is up and accepts the login.")}
			},
		},
		{
			name: "a connection string that does not parse refuses the start", freeSQL: "off", databases: separate,
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": "postgres://nobody:" + loginPassword + "@127.0.0.1:99999/helpdesk"}
			},
			want: func(env) []Finding {
				return []Finding{refuses("the connection string in HELPDESK_WRITE_DSN for the write login of helpdesk cannot be used, because it does not parse as a postgres:// URL or as keyword=value pairs, so adaptive-pg-mcp will not start. Correct it in the environment the server starts with. It is not repeated here, because it may hold a password.")}
			},
		},
		{
			name: "a login that cannot read the system identifier is reported", freeSQL: "off", databases: single,
			sql:  []string{"REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports(idUnreadable("the login of sales_eu", "sales_eu"))}
			},
		},
		{
			name: "pg_monitor does not restore the system identifier once PUBLIC loses it", freeSQL: "off", databases: single,
			sql:  []string{"REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.monitor.DSN(t, e.d)} },
			want: func(env) []Finding {
				return []Finding{reports(idUnreadable("the login of sales_eu", "sales_eu"))}
			},
		},
		{
			name: "a grant to the login restores the system identifier", freeSQL: "off", databases: single,
			sql:  []string{"REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC", "GRANT EXECUTE ON FUNCTION pg_control_system() TO {plain}"},
			dsns: func(e env) map[string]string { return map[string]string{"SALES_EU_DSN": e.l.plain.DSN(t, e.d)} },
			want: func(env) []Finding { return nil },
		},
		{
			name: "a write login that reaches a database with dblink refuses the start", freeSQL: "off", databases: separate, another: true,
			anotherSQL: []string{"CREATE EXTENSION dblink"},
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.another)}
			},
			want: func(env) []Finding {
				return []Finding{
					refuses("the write login of helpdesk reaches a database that has dblink installed, which lets a call reach another database, so adaptive-pg-mcp will not start. Drop the extension from that database, or set allow_cross_database: true for helpdesk."),
					reports(elsewhere("the write login of helpdesk")),
				}
			},
		},
		{
			name: "allow_cross_database accepts a write login that reaches a database with dblink", freeSQL: "off", databases: separate + allowCross, another: true,
			anotherSQL: []string{"CREATE EXTENSION dblink"},
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.another)}
			},
			want: func(env) []Finding {
				return []Finding{
					reports("the write login of helpdesk reaches a database that has dblink installed, which allow_cross_database: true accepts for helpdesk. A call can reach another database through it."),
					reports(elsewhere("the write login of helpdesk")),
				}
			},
		},
		{
			name: "a catalog login that reaches a database with postgres_fdw refuses the start", freeSQL: "off", databases: extra, another: true,
			anotherSQL: []string{"CREATE EXTENSION postgres_fdw"},
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.d), "HELPDESK_FINANCE_DSN": e.l.other.DSN(t, e.another)}
			},
			want: func(env) []Finding {
				return []Finding{
					refuses("the finance_reader login of helpdesk reaches a database that has postgres_fdw installed, which lets a call reach another database, so adaptive-pg-mcp will not start. Drop the extension from that database, or set allow_cross_database: true for helpdesk."),
					reports(elsewhere("the finance_reader login of helpdesk")),
				}
			},
		},
		{
			name: "allow_cross_database accepts a catalog login that reaches a database with postgres_fdw", freeSQL: "off", databases: extra + allowCross, another: true,
			anotherSQL: []string{"CREATE EXTENSION postgres_fdw"},
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.d), "HELPDESK_FINANCE_DSN": e.l.other.DSN(t, e.another)}
			},
			want: func(env) []Finding {
				return []Finding{
					reports("the finance_reader login of helpdesk reaches a database that has postgres_fdw installed, which allow_cross_database: true accepts for helpdesk. A call can reach another database through it."),
					reports(elsewhere("the finance_reader login of helpdesk")),
				}
			},
		},
		{
			name: "a write login that reaches a database with dblink refuses the start when the read login cannot read the id", freeSQL: "off", databases: separate, another: true,
			sql:        []string{"REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC"},
			anotherSQL: []string{"CREATE EXTENSION dblink"},
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.another)}
			},
			want: func(env) []Finding {
				return []Finding{
					reports(idUnreadable("the read login of helpdesk", "helpdesk")),
					refuses("the write login of helpdesk reaches a database that has dblink installed, which lets a call reach another database, so adaptive-pg-mcp will not start. Drop the extension from that database, or set allow_cross_database: true for helpdesk."),
				}
			},
		},
		{
			name: "two logins on one database with dblink refuse the start once when no id can be read", freeSQL: "off", databases: separate,
			sql: []string{"REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC", "CREATE EXTENSION dblink"},
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.d)}
			},
			want: func(env) []Finding {
				return []Finding{
					reports(idUnreadable("the read login of helpdesk", "helpdesk")),
					reports(idUnreadable("the write login of helpdesk", "helpdesk")),
					refuses("helpdesk has dblink installed, which lets a call reach another database, so adaptive-pg-mcp will not start. Drop the extension from helpdesk, or set allow_cross_database: true for helpdesk."),
				}
			},
		},
		{
			name: "a write login that reaches another database is reported", freeSQL: "off", databases: separate, another: true,
			dsns: func(e env) map[string]string {
				return map[string]string{"HELPDESK_READ_DSN": e.l.plain.DSN(t, e.d), "HELPDESK_WRITE_DSN": e.l.other.DSN(t, e.another)}
			},
			want: func(env) []Finding {
				return []Finding{reports(elsewhere("the write login of helpdesk"))}
			},
		},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := newLogins(t, s)
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					e := env{l: l, d: pgtest.NewDatabase(t, s)}
					if c.another {
						e.another = pgtest.NewDatabase(t, s)
					}
					setup(t, e.d, l, c.sql...)
					if c.another {
						setup(t, e.another, l, c.anotherSQL...)
					}
					srv := New(loadConfig(t, c.freeSQL, c.databases, c.dsns(e)), slog.New(slog.DiscardHandler))
					got := srv.Check(t.Context())
					srv.Close()
					want := c.want(e)
					if !sameFindings(got, want) {
						t.Errorf("Check found:\n%s\nwant:\n%s", lines(got), lines(want))
					}
				})
			}
		})
	}
}

// idUnreadable is the report about a login of database db that cannot read the system identifier.
func idUnreadable(login, db string) string {
	return login + " cannot read the id of " + db + ", because the server answered: permission denied for function pg_control_system (SQLSTATE 42501), so list_databases fails and every call that carries an ai.straza/object-id is refused on this login. Grant EXECUTE on the function pg_control_system() to the login, or restore the default grant to PUBLIC."
}

// elsewhere is the report about a login of helpdesk that reaches another database than its read login.
func elsewhere(login string) string {
	return login + " reaches another database than the read login of helpdesk, so its calls can change a database that list_databases does not name. Point both connection strings at the same database."
}

// sameFindings reports whether two lists of findings are equal, sentence for sentence.
func sameFindings(a, b []Finding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lines prints findings one per line, for a failure message.
func lines(fs []Finding) string {
	if len(fs) == 0 {
		return "  (nothing)"
	}
	var b strings.Builder
	for _, f := range fs {
		kind := "report"
		if f.Refuses {
			kind = "refusal"
		}
		b.WriteString("  " + kind + ": " + f.Text + "\n")
	}
	return b.String()
}
