// Package pg holds a connection pool per database login, the one quoting
// helper for identifiers, and the one path that sends a statement of the
// server to Postgres.
//
// Every statement goes through Tx.Query, inside a transaction that Pools.Run
// opens. Query sends it as one unnamed prepared statement on the extended
// protocol, which carries exactly one statement, even when it has no
// arguments. pgx itself would send an argument-free Exec on the simple query
// protocol, which can carry several statements, so nothing in this package
// calls Exec. Tx.Describe sends a statement on the same protocol to learn its
// parameter types, and never runs it.
package pg

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// Login names one login of one database by the database's config name and the login's role, such as read or write.
type Login struct {
	// Database is the config name of the database.
	Database string
	// Role is the login's name in the database's logins, such as read, write or finance_reader.
	Role string
}

// Access says whether a transaction may write.
type Access int

const (
	// ReadOnly runs the transaction as BEGIN READ ONLY.
	ReadOnly Access = iota + 1
	// ReadWrite runs the transaction as an ordinary read-write transaction.
	ReadWrite
)

// closeTimeout bounds the close of a connection that Run does not return to its pool.
const closeTimeout = 5 * time.Second

// Pools holds one connection pool per login. It connects to a login the first time a call runs on it.
type Pools struct {
	pools map[Login]*pgxpool.Pool
	// secrets are the connection strings and passwords of every pool, which Error texts never carry.
	secrets []string
}

// NewPools returns an empty set of pools.
func NewPools() *Pools { return &Pools{pools: map[Login]*pgxpool.Pool{}} }

// Add makes one pool from dsn and gives it to every login in logins, so that
// a database whose one login reads and writes holds one pool. It connects to
// nothing yet. The TLS settings are the ones the connection string asks for,
// unchanged. A connection string that does not parse is refused with an
// Error that never repeats it.
func (p *Pools) Add(dsn config.DSN, logins ...Login) error {
	cfg, err := pgxpool.ParseConfig(dsn.Reveal())
	if err != nil {
		return &Error{Reason: "it does not parse as a postgres:// URL or as keyword=value pairs"}
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return &Error{Reason: "pgxpool cannot make a pool from it"}
	}
	p.secrets = append(p.secrets, dsn.Reveal())
	if cfg.ConnConfig.Password != "" {
		p.secrets = append(p.secrets, cfg.ConnConfig.Password)
	}
	for _, l := range logins {
		p.pools[l] = pool
	}
	return nil
}

// Close closes every pool once, and waits for their connections to close.
func (p *Pools) Close() {
	closed := map[*pgxpool.Pool]bool{}
	for _, pool := range p.pools {
		if !closed[pool] {
			pool.Close()
			closed[pool] = true
		}
	}
}

// Run runs fn in one transaction on one connection of login l. Only
// ReadWrite gives a read-write transaction, and every other access runs as
// BEGIN READ ONLY. Its first statement sets
// statement_timeout to timeout for this transaction only, through a bind
// parameter. Run commits when fn returns nil, and rolls back and returns the
// error of fn unchanged otherwise. The connection goes back to the pool,
// unless fn called Tx.Discard. Every failure of Run itself is an *Error,
// whose text holds no connection string and no password.
func (p *Pools) Run(ctx context.Context, l Login, access Access, timeout time.Duration, fn func(*Tx) error) error {
	pool, ok := p.pools[l]
	if !ok {
		return &Error{Reason: "no pool is open for the " + l.Role + " login of " + l.Database}
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return p.wrap(err, true)
	}
	t := &Tx{}
	defer func() {
		if !t.discard {
			conn.Release()
			return
		}
		closing, cancel := context.WithTimeout(context.Background(), closeTimeout)
		_ = conn.Hijack().Close(closing)
		cancel()
	}()

	opts := pgx.TxOptions{AccessMode: pgx.ReadOnly}
	if access == ReadWrite {
		opts.AccessMode = pgx.ReadWrite
	}
	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		return p.wrap(err, false)
	}
	t.tx = tx
	if err := t.setTimeout(ctx, timeout); err != nil {
		_ = tx.Rollback(ctx)
		return p.wrap(err, false)
	}
	if err := fn(t); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return p.wrap(err, false)
	}
	return nil
}

// Tx is one transaction on one connection, open for the length of a Run.
type Tx struct {
	tx pgx.Tx
	// discard is true when Run closes the connection instead of returning it to the pool.
	discard bool
}

// Discard makes Run close the connection of t once its transaction ends,
// instead of returning it to the pool. A statement that is not the server's
// own can change its session, such as its role, its search_path or a
// temporary table, and a closed connection carries none of it into a later call.
func (t *Tx) Discard() { t.discard = true }

// Query sends sql with args as one unnamed prepared statement on the extended
// protocol and returns its rows, which the caller closes. It is the one path
// every statement of the server takes. Postgres refuses a text that holds more
// than one statement, and an empty text is refused before it is sent.
func (t *Tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql == "" {
		return nil, &Error{Reason: "the statement is empty"}
	}
	for _, a := range args {
		switch a.(type) {
		case pgx.QueryExecMode, pgx.QueryResultFormats, pgx.QueryResultFormatsByOID, pgx.QueryRewriter:
			return nil, &Error{Reason: "an argument is a pgx query option, which could change how the statement is sent"}
		}
	}
	return t.tx.Query(ctx, sql, append([]any{pgx.QueryExecModeDescribeExec}, args...)...)
}

// Describe sends sql as one unnamed prepared statement on the extended
// protocol with Parse and Describe only, and returns the type OIDs of its
// parameters in $1 order. Postgres parses and analyzes the statement and
// never runs it, it reports a parameter of a domain as the domain, and it
// refuses a text that holds more than one statement. An empty text is
// refused before it is sent.
func (t *Tx) Describe(ctx context.Context, sql string) ([]uint32, error) {
	if sql == "" {
		return nil, &Error{Reason: "the statement is empty"}
	}
	d, err := t.tx.Conn().PgConn().Prepare(ctx, "", sql, nil)
	if err != nil {
		return nil, err
	}
	return d.ParamOIDs, nil
}

// setTimeout sets statement_timeout for the rest of the transaction.
func (t *Tx) setTimeout(ctx context.Context, timeout time.Duration) error {
	rows, err := t.Query(ctx, "SELECT set_config('statement_timeout', $1, true)", millis(timeout))
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

// millis writes d as whole milliseconds for statement_timeout, rounded up and
// at least 1, because 0 would switch the timeout off.
func millis(d time.Duration) string {
	return strconv.FormatInt(int64(max((d+time.Millisecond-1)/time.Millisecond, 1)), 10)
}

// Quote returns name as one quoted Postgres identifier. It is the one helper
// that puts an identifier into SQL text, and it doubles every quote in name,
// so the result never ends the identifier early.
func Quote(name string) string { return pgx.Identifier{name}.Sanitize() }
