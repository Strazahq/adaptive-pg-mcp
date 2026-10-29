// Package pg holds a connection pool per login, the quoting helper for
// identifiers, and the only path by which adaptive-pg-mcp sends SQL to
// Postgres.
//
// Every statement goes through Tx.Query, in a transaction that Pools.Run
// opens. Query sends it as one unnamed prepared statement on the extended
// protocol, which carries exactly one statement, even without arguments. pgx
// would send an argument-free Exec on the simple query protocol, which can
// carry several statements, so nothing in this package calls Exec.
// Tx.Describe uses the same protocol to learn the parameter types of a
// statement without running it.
package pg

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// Login names one login by the config name of its database and its role,
// such as read or write.
type Login struct {
	// Database is the config name of the database.
	Database string
	// Role is the login's key in the database's logins, such as read, write
	// or finance_reader.
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

// closeTimeout bounds the close of a connection that Run does not return to
// its pool.
const closeTimeout = 5 * time.Second

// Pools holds one connection pool per login. It connects to a login the
// first time a call runs on it.
type Pools struct {
	pools map[Login]*pgxpool.Pool
	// secrets are the connection strings and passwords that Reason hides.
	secrets []string
}

// NewPools returns an empty set of pools.
func NewPools() *Pools { return &Pools{pools: map[Login]*pgxpool.Pool{}} }

// Add makes one pool from dsn and shares it among logins, so a database with
// a single login holds one pool. It does not connect yet. TLS settings come
// from the connection string unchanged. A connection string that does not
// parse returns an Error that never repeats it.
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
// ReadWrite gives a read-write transaction, and any other access runs as
// BEGIN READ ONLY. The first statement sets statement_timeout to timeout for
// this transaction only, through a bind parameter. Run commits when fn
// returns nil, and otherwise rolls back and returns the error of fn
// unchanged. The connection goes back to the pool unless fn called
// Tx.Discard. Every failure of Run itself is an *Error, whose text holds no
// connection string or password.
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
	// discard is true when Run closes the connection instead of returning it
	// to the pool.
	discard bool
}

// Discard makes Run close the connection of t when its transaction ends,
// instead of returning it to the pool. A caller's statement can change the
// session, such as its role, search_path or temporary tables, and a closed
// connection carries none of that into a later call.
func (t *Tx) Discard() { t.discard = true }

// Query sends sql with args as one unnamed prepared statement on the extended
// protocol and returns its rows, which the caller closes. Every statement
// that adaptive-pg-mcp sends takes this path. Postgres refuses a text with
// more than one statement, and an empty text is refused before it is sent.
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
// protocol with Parse and Describe only, and returns its parameter type OIDs
// in $1 order. Postgres analyzes the statement without running it, reports a
// domain parameter as the domain, and refuses a text with more than one
// statement. An empty text is refused before it is sent.
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

// Quote returns name as one quoted Postgres identifier. It doubles every
// double quote in name, so the result never ends the identifier early. It is
// the only way an identifier enters SQL text.
func Quote(name string) string { return pgx.Identifier{name}.Sanitize() }
