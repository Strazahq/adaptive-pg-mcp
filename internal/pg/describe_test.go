//go:build pg

package pg

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

func TestDescribe(t *testing.T) {
	login := Login{"sales_eu", "write"}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, password)
			d := pgtest.NewDatabase(t, s)
			admin := d.Connect(t)
			for _, sql := range []string{
				"CREATE DOMAIN tiny_note AS text CHECK (length(VALUE) <= 10)",
				"CREATE TABLE notes (id int, note tiny_note)",
				"GRANT SELECT, INSERT ON notes TO " + Quote(l.Name),
			} {
				if _, err := admin.Exec(t.Context(), sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			var domain uint32
			if err := admin.QueryRow(t.Context(), "SELECT 'tiny_note'::regtype::oid").Scan(&domain); err != nil {
				t.Fatal(err)
			}
			p := NewPools()
			if err := p.Add(dsnOf(t, l.DSN(t, d)), login); err != nil {
				t.Fatalf("Add: %v", err)
			}
			t.Cleanup(p.Close)
			cases := []struct {
				name, sql string
				want      []uint32
				state     string
			}{
				{"the parameter types in $1 order", "SELECT $2::int8, $1::text", []uint32{pgtype.TextOID, pgtype.Int8OID}, ""},
				{"no parameters", "SELECT 1", nil, ""},
				{"a parameter of a domain reports the domain", "INSERT INTO notes (id, note) VALUES ($1, $2)", []uint32{pgtype.Int4OID, domain}, ""},
				{"two statements are refused", "SELECT 1; SELECT 2", nil, "42601"},
				{"a column that does not exist is refused", "SELECT nope FROM notes", nil, "42703"},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					var got []uint32
					err := p.Run(t.Context(), login, ReadWrite, time.Second, func(tx *Tx) error {
						var err error
						got, err = tx.Describe(t.Context(), c.sql)
						return err
					})
					if state := sqlState(err); state != c.state || (c.state == "" && err != nil) {
						t.Fatalf("Describe: got %v with SQLSTATE %q, want SQLSTATE %q", err, state, c.state)
					}
					if !slices.Equal(got, c.want) {
						t.Errorf("Describe answered the types %v, want %v", got, c.want)
					}
				})
			}
			t.Run("an empty statement is refused before it is sent", func(t *testing.T) {
				err := p.Run(t.Context(), login, ReadOnly, time.Second, func(tx *Tx) error {
					_, err := tx.Describe(context.Background(), "")
					return err
				})
				var e *Error
				if !errors.As(err, &e) || e.Reason != "the statement is empty" {
					t.Errorf("Describe of an empty statement returned %v, want the reason that it is empty", err)
				}
			})
			var n int
			if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM notes").Scan(&n); err != nil || n != 0 {
				t.Errorf("notes holds %d rows (%v) after a committed Describe of an INSERT, want 0, because Describe never runs a statement", n, err)
			}
		})
	}
}
