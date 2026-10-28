// Package pgtest gives database tests the Postgres 14 to 18 servers that tools/pg.sh starts in local Docker.
package pgtest

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Majors lists the Postgres major versions the harness runs, oldest first.
var Majors = []int{14, 15, 16, 17, 18}

// envFile is where sh tools/pg.sh up writes the connection strings, relative to the module root.
const envFile = ".tools/pg.env"

// envPrefix starts the name of each connection string, as in PGTEST_PG14.
const envPrefix = "PGTEST_PG"

// timeout bounds each connection attempt and each statement the package runs.
const timeout = 30 * time.Second

// startHint tells the reader how to bring the harness up.
const startHint = "Run sh tools/pg.sh up, or make pgtest, to start the Postgres 14 to 18 harness."

// Server is one harness server, named by its major version wherever it is printed.
type Server struct {
	// Major is the Postgres major version the server runs.
	Major int
	// dsn sits behind a pointer, so printing a Server with any verb shows an address and never the connection string.
	dsn *string
}

// String names the server by its major version.
func (s Server) String() string { return "Postgres " + strconv.Itoa(s.Major) }

// Connect opens a connection to the server's postgres database as the superuser and closes it at t.Cleanup.
func (s Server) Connect(t testing.TB) *pgx.Conn {
	t.Helper()
	return connect(t, s, "")
}

// Database is a fresh database on one harness server.
type Database struct {
	// Name is the database name, made from crypto/rand.
	Name string
	// Server is the server the database lives on.
	Server Server
}

// String names the database and its server.
func (d Database) String() string { return "database " + d.Name + " on " + d.Server.String() }

// Connect opens a connection to the database as the superuser and closes it at t.Cleanup.
func (d Database) Connect(t testing.TB) *pgx.Conn {
	t.Helper()
	return connect(t, d.Server, d.Name)
}

// Servers returns the harness servers in the order of Majors, and fails the test when one has no connection string.
//
// The connection strings come from .tools/pg.env at the module root, and an
// environment variable of the same name, such as PGTEST_PG14, overrides its
// line. A missing server fails the test instead of skipping it, because a
// skipped check is a check that never happened.
func Servers(t testing.TB) []Server {
	t.Helper()
	lines, err := readEnvFile()
	if err != nil {
		t.Fatalf("pgtest: %v %s", err, startHint)
	}
	servers := make([]Server, 0, len(Majors))
	for _, major := range Majors {
		key := envPrefix + strconv.Itoa(major)
		dsn := lines[key]
		if v := os.Getenv(key); v != "" {
			dsn = v
		}
		s := Server{Major: major, dsn: &dsn}
		if dsn == "" {
			t.Fatalf("pgtest: there is no connection string for %v, neither in %s nor in the environment variable %s. %s", s, envFile, key, startHint)
		}
		if _, err := pgx.ParseConfig(dsn); err != nil {
			t.Fatalf("pgtest: the connection string for %v does not parse, so it is not shown here. %s", s, startHint)
		}
		servers = append(servers, s)
	}
	return servers
}

// NewDatabase creates a database with a random name on s and drops it at t.Cleanup, with any session still on it.
func NewDatabase(t testing.TB, s Server) Database {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("pgtest: crypto/rand gave no bytes for a database name on %v: %v", s, err)
	}
	d := Database{Name: "pgtest_" + hex.EncodeToString(b), Server: s}
	quoted := pgx.Identifier{d.Name}.Sanitize()

	admin := connect(t, s, "")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := exec(ctx, admin, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("pgtest: cannot create %v: %s", d, scrub(err, s))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := exec(ctx, admin, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("pgtest: cannot drop %v: %s. Run sh tools/pg.sh down, which removes the harness servers and their data.", d, scrub(err, s))
		}
	})
	return d
}

// connect opens a connection to database on s, or to its postgres database when database is empty, and closes it at t.Cleanup.
func connect(t testing.TB, s Server, database string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(*s.dsn)
	if err != nil {
		t.Fatalf("pgtest: the connection string for %v does not parse, so it is not shown here. %s", s, startHint)
	}
	if database != "" {
		cfg.Database = database
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgtest: cannot connect to %v: %s. %s", s, scrub(err, s), startHint)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = conn.Close(ctx)
	})
	return conn
}

// exec runs sql as one unnamed prepared statement on the extended protocol.
//
// pgx sends an Exec without arguments on the simple query protocol, which
// docs/PLAN.md invariant 2 rules out, so DDL goes through ExecParams instead.
func exec(ctx context.Context, conn *pgx.Conn, sql string) error {
	_, err := conn.PgConn().ExecParams(ctx, sql, nil, nil, nil, nil).Close()
	return err
}

// scrub returns the text of err with the connection string and the password of s replaced.
func scrub(err error, s Server) string {
	msg := err.Error()
	secrets := []string{*s.dsn}
	if cfg, perr := pgx.ParseConfig(*s.dsn); perr == nil {
		secrets = append(secrets, cfg.Password)
	}
	for _, secret := range secrets {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return msg
}

// readEnvFile reads the KEY=value lines of .tools/pg.env at the module root, and returns no lines when the file is absent.
func readEnvFile() (map[string]string, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(root, envFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, errors.New(envFile + " cannot be opened: " + err.Error() + ".")
	}
	defer f.Close()

	lines := map[string]string{}
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, errors.New("line " + strconv.Itoa(n) + " of " + envFile + " is not KEY=value, so it is not shown here.")
		}
		lines[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New(envFile + " cannot be read: " + err.Error() + ".")
	}
	return lines, nil
}

// moduleRoot walks up from the working directory to the directory that holds go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", errors.New("the working directory is unknown: " + err.Error() + ".")
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod was found above the working directory, so " + envFile + " cannot be located.")
		}
		dir = parent
	}
}
