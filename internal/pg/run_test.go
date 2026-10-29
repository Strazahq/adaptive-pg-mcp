//go:build pg

package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// sqlState returns the SQLSTATE of a Postgres error in err's chain, or
// empty.
func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// one runs sql with args on tx and scans its one row into dest.
func one(ctx context.Context, tx *Tx, sql string, args []any, dest ...any) error {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	_, err = pgx.ForEachRow(rows, dest, func() error { return nil })
	return err
}

func TestRun(t *testing.T) {
	login := Login{"sales_eu", "read"}
	cases := []struct {
		name    string
		access  Access
		timeout time.Duration
		fn      func(ctx context.Context, tx *Tx) error
		state   string
	}{
		{"a read runs read-only", ReadOnly, time.Second, func(ctx context.Context, tx *Tx) error {
			var ro string
			if err := one(ctx, tx, "SELECT current_setting('transaction_read_only')", nil, &ro); err != nil {
				return err
			}
			if ro != "on" {
				return fmt.Errorf("transaction_read_only is %s in a read", ro)
			}
			return nil
		}, ""},
		{"the zero access runs read-only", 0, time.Second, func(ctx context.Context, tx *Tx) error {
			return one(ctx, tx, "CREATE TABLE zero_access (a int)", nil)
		}, "25006"},
		{"a read refuses a write", ReadOnly, time.Second, func(ctx context.Context, tx *Tx) error {
			return one(ctx, tx, "INSERT INTO kept (a) VALUES (1)", nil)
		}, "25006"},
		{"a write runs read-write", ReadWrite, time.Second, func(ctx context.Context, tx *Tx) error {
			var ro string
			if err := one(ctx, tx, "SELECT current_setting('transaction_read_only')", nil, &ro); err != nil {
				return err
			}
			if ro != "off" {
				return fmt.Errorf("transaction_read_only is %s in a write", ro)
			}
			return nil
		}, ""},
		{"two statements without arguments are refused", ReadOnly, time.Second, func(ctx context.Context, tx *Tx) error {
			return one(ctx, tx, "SELECT 1; SELECT 2", nil)
		}, "42601"},
		{"two statements with arguments are refused", ReadOnly, time.Second, func(ctx context.Context, tx *Tx) error {
			return one(ctx, tx, "SELECT $1::int; SELECT 2", []any{1})
		}, "42601"},
		{"statement_timeout is the timeout", ReadOnly, 1500 * time.Millisecond, func(ctx context.Context, tx *Tx) error {
			var v string
			if err := one(ctx, tx, "SELECT current_setting('statement_timeout')", nil, &v); err != nil {
				return err
			}
			if v != "1500ms" {
				return fmt.Errorf("statement_timeout is %s, want 1500ms", v)
			}
			return nil
		}, ""},
		{"a part of a millisecond rounds up", ReadOnly, 1500*time.Millisecond + time.Nanosecond, func(ctx context.Context, tx *Tx) error {
			var v string
			if err := one(ctx, tx, "SELECT current_setting('statement_timeout')", nil, &v); err != nil {
				return err
			}
			if v != "1501ms" {
				return fmt.Errorf("statement_timeout is %s, want 1501ms", v)
			}
			return nil
		}, ""},
		{"a statement over the timeout is cancelled", ReadOnly, 200 * time.Millisecond, func(ctx context.Context, tx *Tx) error {
			return one(ctx, tx, "SELECT pg_sleep(5)", nil)
		}, "57014"},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, password)
			d := pgtest.NewDatabase(t, s)
			admin := d.Connect(t)
			for _, sql := range []string{"CREATE TABLE kept (a int)", "GRANT SELECT, INSERT ON kept TO " + Quote(l.Name)} {
				if _, err := admin.Exec(t.Context(), sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			p := NewPools()
			if err := p.Add(dsnOf(t, l.DSN(t, d)), login); err != nil {
				t.Fatalf("Add: %v", err)
			}
			t.Cleanup(p.Close)
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					err := p.Run(t.Context(), login, c.access, c.timeout, func(tx *Tx) error { return c.fn(t.Context(), tx) })
					if got := sqlState(err); got != c.state || (c.state == "" && err != nil) {
						t.Errorf("Run: got %v with SQLSTATE %q, want SQLSTATE %q", err, got, c.state)
					}
				})
			}

			t.Run("a commit keeps a write and an error rolls it back", func(t *testing.T) {
				insert := func(v int, fail error) error {
					return p.Run(t.Context(), login, ReadWrite, time.Second, func(tx *Tx) error {
						if err := one(t.Context(), tx, "INSERT INTO kept (a) VALUES ($1)", []any{v}); err != nil {
							return err
						}
						return fail
					})
				}
				stop := errors.New("the call stops here")
				if err := insert(1, nil); err != nil {
					t.Fatalf("insert 1: %v", err)
				}
				if err := insert(2, stop); err != stop {
					t.Fatalf("insert 2 returned %v, want the error of fn unchanged", err)
				}
				var values []int
				if err := admin.QueryRow(t.Context(), "SELECT array_agg(a ORDER BY a) FROM kept").Scan(&values); err != nil {
					t.Fatal(err)
				}
				if fmt.Sprint(values) != "[1]" {
					t.Errorf("kept holds %v, want [1]", values)
				}
			})

			t.Run("a quoted identifier stays one name", func(t *testing.T) {
				name := `odd "name"; DROP TABLE kept; --`
				if _, err := admin.Exec(t.Context(), "CREATE TABLE "+Quote(name)+" (a int)"); err != nil {
					t.Fatalf("create: %v", err)
				}
				var n int
				if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM pg_class WHERE relname = $1", name).Scan(&n); err != nil || n != 1 {
					t.Errorf("pg_class holds %d tables named %q (%v), want 1", n, name, err)
				}
				if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM pg_class WHERE relname = 'kept'").Scan(&n); err != nil || n != 1 {
					t.Errorf("kept is gone after the quoted create (%v)", err)
				}
			})

			t.Run("a wrong password is a connect error that hides it", func(t *testing.T) {
				bad := NewPools()
				dsn := strings.Replace(l.DSN(t, d), password, password+"x", 1)
				if err := bad.Add(dsnOf(t, dsn), login); err != nil {
					t.Fatalf("Add: %v", err)
				}
				defer bad.Close()
				err := bad.Run(t.Context(), login, ReadOnly, time.Second, func(*Tx) error { return nil })
				var e *Error
				if !errors.As(err, &e) || !e.Connect {
					t.Fatalf("Run returned %v, want a connect *Error", err)
				}
				want := `Postgres said: password authentication failed for user "` + l.Name + `" (SQLSTATE 28P01)`
				if e.Error() != want {
					t.Errorf("the reason:\n got %s\nwant %s", e.Error(), want)
				}
				if text := fmt.Sprintf("%v %+v", err, err); strings.Contains(text, password) {
					t.Errorf("the error shows the password: %s", text)
				}
			})
		})
	}
}
