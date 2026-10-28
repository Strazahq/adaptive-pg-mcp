//go:build pg

package core

import (
	"log/slog"
	"strconv"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// started builds a server for one database sales_eu with the login dsn, and runs Check, which must find no refusal.
func started(t *testing.T, dsn string) *Server {
	t.Helper()
	srv := New(loadConfig(t, "off", single, map[string]string{"SALES_EU_DSN": dsn}), slog.New(slog.DiscardHandler))
	for _, f := range srv.Check(t.Context()) {
		if f.Refuses {
			t.Fatalf("Check refused: %s", f.Text)
		}
	}
	t.Cleanup(srv.Close)
	return srv
}

// idOf reads the id of d as the superuser does: the system identifier and the OID, joined by a colon.
func idOf(t *testing.T, d pgtest.Database) string {
	t.Helper()
	// pg_control_system answers the identifier as a signed bigint, so the id carries a minus sign when its top bit is set.
	var system int64
	var oid uint32
	admin := d.Server.Connect(t)
	if err := admin.QueryRow(t.Context(), "SELECT system_identifier FROM pg_control_system()").Scan(&system); err != nil {
		t.Fatalf("reading the system identifier on %v: %v", d.Server, err)
	}
	if err := admin.QueryRow(t.Context(), "SELECT oid FROM pg_database WHERE datname = $1", d.Name).Scan(&oid); err != nil {
		t.Fatalf("reading the OID of %v: %v", d, err)
	}
	return strconv.FormatInt(system, 10) + ":" + strconv.FormatUint(uint64(oid), 10)
}

// checkOn runs checkObjectID for sales_eu with meta on a read transaction of srv, and returns its refusal or empty.
func checkOn(t *testing.T, srv *Server, meta mcp.Meta) string {
	t.Helper()
	err := srv.pools.Run(t.Context(), pg.Login{Database: "sales_eu", Role: "read"}, pg.ReadOnly, srv.cfg.Limits.Timeout, func(tx *pg.Tx) error {
		return srv.checkObjectID(t.Context(), tx, "sales_eu", meta)
	})
	if err != nil {
		return err.Error()
	}
	return ""
}

const (
	// recreated is the refusal of a call whose id is not the database's id now.
	recreated = "sales_eu was dropped and created again after Straza listed it, so the call is refused. Try again once Straza lists the databases again, or ask an administrator to refresh the list."
	// notAString is the refusal of a call whose id is not a usable string.
	notAString = "the call carries an ai.straza/object-id that is empty or not a string, so the call is refused. Straza sends the id that list_databases answered as a string, so send the call through Straza."
)

func TestTheIDReadsOnEveryLogin(t *testing.T) {
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := newLogins(t, s)
			d := pgtest.NewDatabase(t, s)
			want := idOf(t, d)
			cases := []struct {
				name  string
				login pgtest.Login
			}{
				{"a plain login", l.plain},
				{"a member of pg_monitor", l.monitor},
				{"a superuser", l.super},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					srv := started(t, c.login.DSN(t, d))
					got, err := srv.readIDOn(t.Context(), pg.Login{Database: "sales_eu", Role: "read"})
					if err != nil || got != want {
						t.Errorf("readIDOn = %q, %v, want %s", got, err, want)
					}
				})
			}
		})
	}
}

func TestIDCheck(t *testing.T) {
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := newLogins(t, s)
			d := pgtest.NewDatabase(t, s)
			other := pgtest.NewDatabase(t, s)
			before, otherID := idOf(t, d), idOf(t, other)

			srv := started(t, l.plain.DSN(t, d))
			cases := []struct {
				name string
				meta mcp.Meta
				want string
			}{
				{"no id passes", mcp.Meta{}, ""},
				{"no _meta passes", nil, ""},
				{"other _meta keys pass", mcp.Meta{"io.modelcontextprotocol/protocolVersion": "2026-07-28"}, ""},
				{"the id now passes", mcp.Meta{objectIDKey: before}, ""},
				{"the id of another database is refused", mcp.Meta{objectIDKey: otherID}, recreated},
				{"an id with a space is refused", mcp.Meta{objectIDKey: before + " "}, recreated},
				{"an empty id is refused", mcp.Meta{objectIDKey: ""}, notAString},
				{"a number is refused", mcp.Meta{objectIDKey: 42}, notAString},
				{"null is refused", mcp.Meta{objectIDKey: nil}, notAString},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					if got := checkOn(t, srv, c.meta); got != c.want {
						t.Errorf("checkObjectID:\n got %q\nwant %q", got, c.want)
					}
				})
			}

			// Drop sales_eu's database and create it again under the same name.
			srv.Close()
			admin := s.Connect(t)
			for _, sql := range []string{"DROP DATABASE " + pg.Quote(d.Name) + " WITH (FORCE)", "CREATE DATABASE " + pg.Quote(d.Name)} {
				if _, err := admin.Exec(t.Context(), sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			after := idOf(t, d)
			if after == before {
				t.Fatalf("the id %s stayed the same after the database was dropped and created again", after)
			}
			again := started(t, l.plain.DSN(t, d))
			recreatedCases := []struct {
				name string
				meta mcp.Meta
				want string
			}{
				{"the id from before is refused", mcp.Meta{objectIDKey: before}, recreated},
				{"the id now passes", mcp.Meta{objectIDKey: after}, ""},
				{"no id passes", mcp.Meta{}, ""},
			}
			for _, c := range recreatedCases {
				t.Run("after drop and create/"+c.name, func(t *testing.T) {
					if got := checkOn(t, again, c.meta); got != c.want {
						t.Errorf("checkObjectID:\n got %q\nwant %q", got, c.want)
					}
				})
			}

			t.Run("an id that cannot be read refuses the call", func(t *testing.T) {
				if _, err := d.Connect(t).Exec(t.Context(), "REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC"); err != nil {
					t.Fatal(err)
				}
				want := "adaptive-pg-mcp cannot read the id of sales_eu to compare it with the ai.straza/object-id of the call, because the server answered: permission denied for function pg_control_system (SQLSTATE 42501), so the call is refused. Try again, and if it fails again, run adaptive-pg-mcp check to see why the login cannot read it."
				if got := checkOn(t, again, mcp.Meta{objectIDKey: after}); got != want {
					t.Errorf("checkObjectID:\n got %q\nwant %q", got, want)
				}
				if got := checkOn(t, again, mcp.Meta{}); got != "" {
					t.Errorf("a call without an id is refused once the id cannot be read: %s", got)
				}
			})
		})
	}
}
