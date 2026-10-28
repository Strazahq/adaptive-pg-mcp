package pg

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// Error is a failure of Run itself, such as a login that cannot connect. Its
// text never holds a connection string or a password.
type Error struct {
	// Connect is true when Run could not open a connection to the login.
	Connect bool
	// Reason says why, in words that are safe to log.
	Reason string
	// err is the driver's error, kept for errors.Is and errors.As and never printed.
	err error
}

// Error returns the reason.
func (e *Error) Error() string { return e.Reason }

// Unwrap returns the driver's error, for errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.err }

// wrap turns a driver error into an *Error whose reason is safe to log.
func (p *Pools) wrap(err error, connect bool) error {
	return &Error{Connect: connect, Reason: p.Reason(err), err: err}
}

// Reason says why err happened in words that are safe to log, and never
// repeats a connection string or a password of the pools. For an error that
// Postgres reported it is the server's own message and SQLSTATE. For a
// failure to connect it is a plain cause, such as a host name that does not
// resolve, and otherwise the innermost message of the driver.
func (p *Pools) Reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return p.scrub("the server answered: " + pgErr.Message + " (SQLSTATE " + pgErr.Code + ")")
	}
	var dns *net.DNSError
	switch {
	case errors.Is(err, context.Canceled):
		return "the call was cancelled before the server answered"
	case errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err):
		return "the server did not answer in time"
	case errors.As(err, &dns):
		return "the host name of the connection string does not resolve"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "nothing accepts connections at the host and port of the connection string"
	}
	return p.scrub("the connection reported: " + innermost(err).Error())
}

// scrub replaces every connection string and password of the pools in s, also in their URL-escaped forms.
func (p *Pools) scrub(s string) string {
	for _, secret := range p.secrets {
		for _, form := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
			if form != "" {
				s = strings.ReplaceAll(s, form, "[hidden]")
			}
		}
	}
	return s
}

// innermost returns the error at the end of err's chain, which carries the
// cause without the wrapping text that names the connection's settings.
func innermost(err error) error {
	for {
		var next error
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			next = u.Unwrap()
		case interface{ Unwrap() []error }:
			if list := u.Unwrap(); len(list) > 0 {
				next = list[0]
			}
		}
		if next == nil {
			return err
		}
		err = next
	}
}
