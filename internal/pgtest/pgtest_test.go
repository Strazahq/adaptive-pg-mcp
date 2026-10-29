//go:build pg

package pgtest_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// TestHarness checks that each harness server runs its major version, and
// that NewDatabase and NewLogin make a database and a login that are gone
// after cleanup.
func TestHarness(t *testing.T) {
	tests := []struct {
		major int
	}{
		{major: 14},
		{major: 15},
		{major: 16},
		{major: 17},
		{major: 18},
	}
	servers := pgtest.Servers(t)
	if len(servers) != len(tests) {
		t.Fatalf("Servers returned %d servers, want %d", len(servers), len(tests))
	}
	for i, tt := range tests {
		s := servers[i]
		t.Run(s.String(), func(t *testing.T) {
			if s.Major != tt.major {
				t.Fatalf("server %d is %v, want Postgres %d", i, s, tt.major)
			}
			admin := s.Connect(t)
			var num int
			if err := admin.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
				t.Fatalf("reading server_version_num on %v: %v", s, err)
			}
			if got := num / 10000; got != tt.major {
				t.Errorf("server_version_num on %v is %d, which is major %d, want %d", s, num, got, tt.major)
			}

			var name string
			t.Run("database", func(t *testing.T) {
				d := pgtest.NewDatabase(t, s)
				name = d.Name
				if !exists(t, admin, d.Name) {
					t.Errorf("%v is not in pg_database after NewDatabase", d)
				}
				var current string
				if err := d.Connect(t).QueryRow(t.Context(), "SELECT current_database()").Scan(&current); err != nil {
					t.Fatalf("reading current_database on %v: %v", d, err)
				}
				if current != d.Name {
					t.Errorf("a connection to %v reaches %s", d, current)
				}
			})
			if name == "" {
				t.Fatalf("the database subtest made no database on %v, so its cleanup cannot be checked", s)
			}
			if exists(t, admin, name) {
				t.Errorf("database %s on %v is still in pg_database after cleanup", name, s)
			}

			var role string
			t.Run("login", func(t *testing.T) {
				l := pgtest.NewLogin(t, s, "pw-pgtest-login", "BYPASSRLS")
				role = l.Name
				d := pgtest.NewDatabase(t, s)
				cfg, err := pgx.ParseConfig(l.DSN(t, d))
				if err != nil {
					t.Fatalf("the DSN of %v does not parse: %v", l, err)
				}
				conn, err := pgx.ConnectConfig(t.Context(), cfg)
				if err != nil {
					t.Fatalf("%v cannot connect to %v: %v", l, d, err)
				}
				defer conn.Close(t.Context())
				var user, db string
				var bypass bool
				if err := conn.QueryRow(t.Context(), "SELECT session_user, current_database(), rolbypassrls FROM pg_roles WHERE rolname = session_user").Scan(&user, &db, &bypass); err != nil {
					t.Fatalf("reading the session of %v: %v", l, err)
				}
				if user != l.Name || db != d.Name || !bypass {
					t.Errorf("the DSN of %v logs in as %s to %s with BYPASSRLS %v", l, user, db, bypass)
				}
			})
			if role == "" {
				t.Fatalf("the login subtest made no login on %v, so its cleanup cannot be checked", s)
			}
			var n int
			if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM pg_roles WHERE rolname = $1", role).Scan(&n); err != nil || n != 0 {
				t.Errorf("login %s on %v is still in pg_roles after cleanup (%v)", role, s, err)
			}
		})
	}
}

// exists reports whether pg_database on the connection's server lists name.
func exists(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	var n int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM pg_database WHERE datname = $1", name).Scan(&n); err != nil {
		t.Fatalf("reading pg_database: %v", err)
	}
	return n == 1
}
