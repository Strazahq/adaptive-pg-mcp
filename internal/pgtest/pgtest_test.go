//go:build pg

package pgtest_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// TestHarness checks that each harness server runs its major version and that NewDatabase makes a database that is gone after cleanup.
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
