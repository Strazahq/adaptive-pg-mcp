//go:build pg

package core

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// countsSQL reads the unflushed row changes of the backend in the user tables and in the system catalogs, without the TOAST schemas.
const countsSQL = `SELECT (SELECT coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del), 0) FROM pg_stat_xact_user_tables)::int8,
  (SELECT coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del), 0) FROM pg_stat_xact_sys_tables WHERE schemaname !~ '^pg_toast')::int8`

// catalogFixture is the SQL of the catalog count measurements. toasted holds a value that Postgres keeps out of line in its TOAST table.
var catalogFixture = []string{
	"CREATE TABLE toasted (id int PRIMARY KEY, body text)",
	"INSERT INTO toasted SELECT 1, string_agg(md5(i::text), '') FROM generate_series(1, 4000) i",
	"CREATE SEQUENCE seq",
	"CREATE TABLE serials (id serial PRIMARY KEY, v int)",
	"CREATE TABLE plain (id int, v int)",
	"INSERT INTO plain SELECT g, g FROM generate_series(1, 500) g",
	"CREATE TABLE rewritten (id int, v int)",
	"INSERT INTO rewritten SELECT g, g FROM generate_series(1, 500) g",
	"CREATE TABLE sink (id int)",
	"CREATE FUNCTION fill_sink() RETURNS SETOF int LANGUAGE plpgsql AS $$BEGIN INSERT INTO public.sink SELECT generate_series(1, 150); RETURN QUERY SELECT 1; END$$",
	"CREATE MATERIALIZED VIEW writes AS SELECT x FROM fill_sink() x WITH NO DATA",
	"CREATE MATERIALIZED VIEW copied AS SELECT * FROM plain",
	"CREATE FUNCTION trunc_plain() RETURNS void LANGUAGE plpgsql AS $$BEGIN TRUNCATE plain; END$$",
	"CREATE PROCEDURE alter_plain() LANGUAGE plpgsql AS $$BEGIN ALTER TABLE plain ADD COLUMN extra int; END$$",
}

// measure runs sql in a transaction of its own on conn and returns the change of the user and the catalog counts across it.
func measure(t *testing.T, conn *pgx.Conn, sql string) (user, sys int64) {
	t.Helper()
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	var u0, s0, u1, s1 int64
	if err := tx.QueryRow(t.Context(), countsSQL).Scan(&u0, &s0); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.QueryRow(t.Context(), countsSQL).Scan(&u1, &s1); err != nil {
		t.Fatal(err)
	}
	return u1 - u0, s1 - s0
}

func TestCatalogCountsPerStatementClass(t *testing.T) {
	cases := []struct {
		class, sql string
		// structure is true when the statement changes the system catalogs.
		structure bool
	}{
		{"INSERT of a toasted value", "INSERT INTO toasted SELECT 2, string_agg(md5(i::text), '') FROM generate_series(1, 4000) i", false},
		{"UPDATE of a toasted value", "UPDATE toasted SET body = body || 'x'", false},
		{"DELETE of a toasted value", "DELETE FROM toasted", false},
		{"nextval", "SELECT nextval('seq')", false},
		{"INSERT with a serial default", "INSERT INTO serials (v) VALUES (1)", false},
		{"SELECT", "SELECT count(*) FROM plain", false},
		{"UPDATE of 500 rows", "UPDATE plain SET v = v + 1", false},
		{"TRUNCATE", "TRUNCATE plain", true},
		{"TRUNCATE in a DO block", "DO $$BEGIN TRUNCATE plain; END$$", true},
		{"ALTER TABLE in a DO block", "DO $$BEGIN ALTER TABLE plain ADD COLUMN extra int; END$$", true},
		{"ALTER TABLE in a procedure", "CALL alter_plain()", true},
		{"TRUNCATE in a function in a SELECT", "SELECT trunc_plain()", true},
		{"CREATE TABLE", "CREATE TABLE made (a int)", true},
		{"ALTER TABLE that rewrites", "ALTER TABLE rewritten ALTER COLUMN v TYPE bigint", true},
		{"REFRESH MATERIALIZED VIEW", "REFRESH MATERIALIZED VIEW copied", true},
		{"REFRESH MATERIALIZED VIEW over a function that writes", "REFRESH MATERIALIZED VIEW writes", true},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			d := pgtest.NewDatabase(t, s)
			conn := d.Connect(t)
			for _, sql := range catalogFixture {
				if _, err := conn.Exec(t.Context(), sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			for _, c := range cases {
				user, sys := measure(t, conn, c.sql)
				if (sys > 0) != c.structure {
					t.Errorf("%s changed %d catalog rows, want structure %v", c.class, sys, c.structure)
				}
				t.Logf("%s: user %d, catalog %d", c.class, user, sys)
			}
		})
	}
}
