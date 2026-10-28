package pg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// password is the known password the tests hide in connection strings.
const password = "pw-41d7-must-never-print"

func TestQuote(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"a plain name", "tickets", `"tickets"`},
		{"capitals stay", "Tickets", `"Tickets"`},
		{"a quote is doubled", `a"b`, `"a""b"`},
		{"a statement inside stays a name", `x"; DROP TABLE t; --`, `"x""; DROP TABLE t; --"`},
		{"a dot is part of the name", "public.tickets", `"public.tickets"`},
		{"a NUL byte is removed", "a\x00b", `"ab"`},
		{"the empty name", "", `""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Quote(c.in); got != c.want {
				t.Errorf("Quote(%q) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

func TestMillis(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{time.Nanosecond, "1"},
		{time.Microsecond, "1"},
		{time.Millisecond, "1"},
		{time.Millisecond + time.Nanosecond, "2"},
		{1500 * time.Millisecond, "1500"},
		{30 * time.Second, "30000"},
	}
	for _, c := range cases {
		if got := millis(c.in); got != c.want {
			t.Errorf("millis(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestQueryRefusesBeforeSending(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		args []any
		want string
	}{
		{"an empty statement", "", nil, "the statement is empty"},
		{"a query mode", "SELECT 1", []any{pgx.QueryExecModeSimpleProtocol}, "an argument is a pgx query option, which could change how the statement is sent"},
		{"a query mode after a value", "SELECT $1", []any{"x", pgx.QueryExecModeSimpleProtocol}, "an argument is a pgx query option, which could change how the statement is sent"},
		{"named arguments", "SELECT @a", []any{pgx.NamedArgs{"a": 1}}, "an argument is a pgx query option, which could change how the statement is sent"},
		{"result formats", "SELECT 1", []any{pgx.QueryResultFormats{1}}, "an argument is a pgx query option, which could change how the statement is sent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The Tx holds no transaction, so a Query that sent anything would panic.
			_, err := (&Tx{}).Query(context.Background(), c.sql, c.args...)
			if err == nil || err.Error() != c.want {
				t.Errorf("Query: got %v, want %s", err, c.want)
			}
		})
	}
}

func TestAddNeverRepeatsTheConnectionString(t *testing.T) {
	cases := []struct{ name, dsn string }{
		{"a URL with a bad port", "postgres://sales:" + password + "@db.example.internal:99999/sales_eu"},
		{"a URL with a bad escape", "postgres://sales:" + password + "%zz@db.example.internal/sales_eu"},
		{"keywords with an open quote", "host=db.example.internal user=sales password='" + password},
		{"an unknown sslmode", "postgres://sales:" + password + "@db.example.internal/sales_eu?sslmode=sometimes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := NewPools().Add(dsnOf(t, c.dsn), Login{"sales_eu", "read"})
			if err == nil {
				t.Fatalf("Add accepted %s", c.name)
			}
			if text := fmt.Sprintf("%v %+v %#v", err, err, err); strings.Contains(text, password) || strings.Contains(text, "db.example.internal") {
				t.Errorf("the error repeats the connection string: %s", text)
			}
		})
	}
}

func TestReason(t *testing.T) {
	p := NewPools()
	if err := p.Add(dsnOf(t, "postgres://sales:"+password+"@db.example.internal/sales_eu"), Login{"sales_eu", "read"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer p.Close()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"a server error", &pgconn.PgError{Message: `password authentication failed for user "sales"`, Code: "28P01"}, `the server answered: password authentication failed for user "sales" (SQLSTATE 28P01)`},
		{"a server error that repeats the password", fmt.Errorf("wrapped: %w", &pgconn.PgError{Message: "bad value " + password, Code: "22023"}), "the server answered: bad value [hidden] (SQLSTATE 22023)"},
		{"a host that does not resolve", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "db.example.internal"}}, "the host name of the connection string does not resolve"},
		{"a refused connection", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "nothing accepts connections at the host and port of the connection string"},
		{"a deadline", context.DeadlineExceeded, "the server did not answer in time"},
		{"a cancel", fmt.Errorf("x: %w", context.Canceled), "the call was cancelled before the server answered"},
		{"an error of Run", &Error{Reason: "the statement is empty"}, "the statement is empty"},
		{"anything else, at its innermost", fmt.Errorf("dial postgres://sales:%s@db.example.internal: %w", password, errors.New("tls: handshake failure")), "the connection reported: tls: handshake failure"},
		{"anything else that repeats the password", errors.New("odd " + password), "the connection reported: odd [hidden]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.Reason(c.err); got != c.want {
				t.Errorf("Reason:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}
