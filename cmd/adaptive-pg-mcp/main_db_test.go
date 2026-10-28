//go:build pg

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// harness returns the environment of the test config on one harness server:
// every login is l on a fresh database, and the token is the known one.
func harness(t *testing.T, l pgtest.Login, d pgtest.Database) map[string]string {
	t.Helper()
	dsn := l.DSN(t, d)
	return map[string]string{"SALES_EU_DSN": dsn, "HELPDESK_READ_DSN": dsn, "HELPDESK_WRITE_DSN": dsn, "ADAPTIVE_PG_MCP_TOKEN": knownToken}
}

func TestCheckCommand(t *testing.T) {
	const passes = "check: no start check refuses, so adaptive-pg-mcp would start.\n"
	cases := []struct {
		name    string
		options []string
		extra   string
		code    int
		stdout  func(l pgtest.Login) string
	}{
		{"a plain login passes", nil, "", 0, func(pgtest.Login) string { return passes }},
		{"a report does not refuse", []string{"BYPASSRLS"}, "", 0, func(pgtest.Login) string {
			return "report: the read login of helpdesk has BYPASSRLS, so no row-level security policy limits what its calls read or change. A login without BYPASSRLS keeps the policies in force.\n" +
				"report: the write login of helpdesk has BYPASSRLS, so no row-level security policy limits what its calls read or change. A login without BYPASSRLS keeps the policies in force.\n" +
				"report: the login of sales_eu has BYPASSRLS, so no row-level security policy limits what its calls read or change. A login without BYPASSRLS keeps the policies in force.\n" + passes
		}},
		{"a superuser refuses on every database", []string{"SUPERUSER"}, "", 1, func(pgtest.Login) string {
			return "refusal: the read login of helpdesk is a superuser and free SQL is on, so adaptive-pg-mcp will not start. Use a login without superuser, turn free_sql off, or set allow_superuser: true for helpdesk.\n" +
				"refusal: the write login of helpdesk is a superuser and free SQL is on, so adaptive-pg-mcp will not start. Use a login without superuser, turn free_sql off, or set allow_superuser: true for helpdesk.\n" +
				"refusal: the login of sales_eu is a superuser and free SQL is on, so adaptive-pg-mcp will not start. Use a login without superuser, turn free_sql off, or set allow_superuser: true for sales_eu.\n" +
				"check: 3 of the findings above refuse the start, so adaptive-pg-mcp would not start. Fix what each refusal says, and run adaptive-pg-mcp check again.\n"
		}},
		{"allow_superuser on one database leaves one refusal", []string{"SUPERUSER"}, "    allow_superuser: true\n", 1, func(pgtest.Login) string {
			return "report: the read login of helpdesk is a superuser and free SQL is on, which allow_superuser: true accepts for helpdesk. A superuser's free SQL is not limited to the database, so a login without superuser is the safer setup.\n" +
				"report: the write login of helpdesk is a superuser and free SQL is on, which allow_superuser: true accepts for helpdesk. A superuser's free SQL is not limited to the database, so a login without superuser is the safer setup.\n" +
				"refusal: the login of sales_eu is a superuser and free SQL is on, so adaptive-pg-mcp will not start. Use a login without superuser, turn free_sql off, or set allow_superuser: true for sales_eu.\n" +
				"check: 1 of the findings above refuses the start, so adaptive-pg-mcp would not start. Fix what it says, and run adaptive-pg-mcp check again.\n"
		}},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					l := pgtest.NewLogin(t, s, password, c.options...)
					d := pgtest.NewDatabase(t, s)
					var out, errs bytes.Buffer
					code := run(context.Background(), []string{"check", "-config", writeConfig(t, "127.0.0.1:0", c.extra)}, &out, &errs, lookup(harness(t, l, d)))
					if code != c.code {
						t.Errorf("check exited %d, want %d: %s", code, c.code, errs.String())
					}
					if want := c.stdout(l); out.String() != want {
						t.Errorf("check printed:\n%s\nwant:\n%s", out.String(), want)
					}
				})
			}
		})
	}
}

func TestSecretsStayOutOfLogsAndErrorsOnAHarnessDatabase(t *testing.T) {
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			var seen strings.Builder
			keep := func(text string) { seen.WriteString(text + "\n") }
			l := pgtest.NewLogin(t, s, password)
			d := pgtest.NewDatabase(t, s)
			env := harness(t, l, d)
			path := writeConfig(t, "127.0.0.1:0", "")

			// check with the right password, a wrong one and a database that does not exist.
			wrong := map[string]string{}
			gone := map[string]string{}
			for k, v := range env {
				wrong[k] = strings.Replace(v, password, password+"-wrong", 1)
				gone[k] = strings.Replace(v, "/"+d.Name, "/"+d.Name+"_gone", 1)
			}
			for _, c := range []struct {
				name string
				env  map[string]string
				code int
			}{{"the right password", env, 0}, {"a wrong password", wrong, 1}, {"a database that does not exist", gone, 1}} {
				var out, errs bytes.Buffer
				if code := run(context.Background(), []string{"check", "-config", path}, &out, &errs, lookup(c.env)); code != c.code {
					t.Errorf("check with %s exited %d, want %d: %s %s", c.name, code, c.code, out.String(), errs.String())
				}
				keep(out.String() + errs.String())
			}

			// serve on HTTP with a wrong password, which the start checks refuse.
			var out, errs bytes.Buffer
			if code := run(context.Background(), []string{"serve", "-config", path}, &out, &errs, lookup(wrong)); code != 1 {
				t.Errorf("serve with a wrong password exited %d, want 1", code)
			}
			keep(out.String() + errs.String())

			// serve on stdio with the right token and a wrong password, which must leave stdout to the protocol.
			out.Reset()
			errs.Reset()
			if code := run(context.Background(), []string{"serve", "-config", writeConfig(t, "stdio", "")}, &out, &errs, lookup(wrong)); code != 1 || out.Len() != 0 {
				t.Errorf("serve on stdio with a wrong password exited %d and wrote %q to stdout, want 1 and nothing", code, out.String())
			}
			if !strings.Contains(errs.String(), "adaptive-pg-mcp will not start, because 3 start checks refused it") {
				t.Errorf("serve on stdio did not say why it will not start:\n%s", errs.String())
			}
			keep(out.String() + errs.String())

			// serve on HTTP with the right password, refused and admitted requests, list_databases and the data tools.
			admin := d.Connect(t)
			for _, sql := range []string{"CREATE TABLE tickets (id int)", "GRANT SELECT ON tickets TO " + pgx.Identifier{l.Name}.Sanitize()} {
				if _, err := admin.Exec(t.Context(), sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			serveHTTP(t, path, env, keep)

			// check with a catalog statement that does not prepare, and serve one whose call fails, both with the known password in a value that Postgres repeats.
			const failing = "-- name: count_as\nSELECT $1::text::int4 AS n\n"
			const failingEntry = "  count_as:\n    description: Reads a value as a number. Answers one row.\n    mode: read\n    databases: [helpdesk]\n    login: read\n    params:\n      - name: value\n        description: The value.\n        required: true\n"
			unprepared := writeConfig(t, "127.0.0.1:0", "")
			addCatalog(t, unprepared, map[string]string{
				"helpdesk/tickets.sql": "-- name: password_cast\nSELECT '" + password + "'::int4\n\n" + failing,
				"helpdesk/tools.yaml":  "version: 1\ntools:\n  password_cast:\n    description: Reads a fixed text as a number. Answers one row.\n    mode: read\n    databases: [helpdesk]\n    login: read\n" + failingEntry,
			}, "count_as")
			out.Reset()
			errs.Reset()
			if code := run(context.Background(), []string{"check", "-config", unprepared}, &out, &errs, lookup(env)); code != 1 {
				t.Errorf("check with a statement that does not prepare exited %d, want 1: %s %s", code, out.String(), errs.String())
			}
			keep(out.String() + errs.String())
			served := writeConfig(t, "127.0.0.1:0", "")
			addCatalog(t, served, map[string]string{"helpdesk/tickets.sql": failing, "helpdesk/tools.yaml": "version: 1\ntools:\n" + failingEntry}, "count_as")
			keep(callCatalog(t, served, env, "count_as", map[string]any{"database": "helpdesk", "value": password}))

			for _, secret := range []string{password, knownToken, env["SALES_EU_DSN"]} {
				if strings.Contains(seen.String(), secret) {
					t.Errorf("a log line, an error or an answer shows %q:\n%s", secret, seen.String())
				}
			}
			for _, must := range []string{
				"password authentication failed for user", "does not exist", "refused an HTTP request", "list_databases found 1 database: helpdesk.",
				"list_tables found 1 table on helpdesk.", `invalid input syntax for type integer: \"[hidden]\"`, "read_rows found no table public.absent on helpdesk",
				"support_bot may not use sales_eu", "support_bot may not use query_sql",
				`statement password_cast does not prepare on helpdesk: the server answered: invalid input syntax for type integer: "[hidden]" (SQLSTATE 22P02)`,
				`count_as on helpdesk failed, because the server answered: invalid input syntax for type integer: "[hidden]" (SQLSTATE 22P02)`,
			} {
				if !strings.Contains(seen.String(), must) {
					t.Errorf("the capture misses %q, which it should hold:\n%s", must, seen.String())
				}
			}
		})
	}
}

// writeCatalogConfig writes server.yaml in mode straza with a catalog of one
// statement on helpdesk, which reads the status column of tickets, and
// returns the path of server.yaml.
func writeCatalogConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"server.yaml": "version: 1\nmode: straza\nlisten: stdio\nfree_sql: \"off\"\nlimits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }\n" +
			"gateway_tokens_sha256: [\"" + hashHex(knownToken) + "\"]\ncatalog: ./catalog\ndatabases:\n  helpdesk:\n    login: { dsn_env: HELPDESK_DSN }\n",
		"catalog/helpdesk/tickets.sql": "-- name: open_tickets\nSELECT id FROM tickets WHERE status = 'open'\n",
		"catalog/helpdesk/tools.yaml":  "version: 1\ntools:\n  open_tickets:\n    description: Lists the open tickets. Answers one row per ticket.\n    mode: read\n    databases: [helpdesk]\n    login: read\n",
	}
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "server.yaml")
}

func TestCheckCommandChecksTheCatalog(t *testing.T) {
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, password)
			cases := []struct {
				name, table string
				code        int
				stdout      string
			}{
				{"a catalog that prepares", "CREATE TABLE tickets (id int, status text)", 0, "check: no start check refuses, so adaptive-pg-mcp would start.\n"},
				{"a statement that does not prepare", "CREATE TABLE tickets (id int)", 1,
					"refusal: helpdesk/tickets.sql statement open_tickets does not prepare on helpdesk: the server answered: column \"status\" does not exist (SQLSTATE 42703). Fix the statement or remove helpdesk from its databases.\n" +
						"check: 1 of the findings above refuses the start, so adaptive-pg-mcp would not start. Fix what it says, and run adaptive-pg-mcp check again.\n"},
			}
			path := writeCatalogConfig(t)
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					d := pgtest.NewDatabase(t, s)
					admin := d.Connect(t)
					for _, sql := range []string{c.table, "GRANT SELECT ON tickets TO " + pgx.Identifier{l.Name}.Sanitize()} {
						if _, err := admin.Exec(t.Context(), sql); err != nil {
							t.Fatalf("%s: %v", sql, err)
						}
					}
					var out, errs bytes.Buffer
					code := run(context.Background(), []string{"check", "-config", path}, &out, &errs, lookup(map[string]string{"HELPDESK_DSN": l.DSN(t, d)}))
					if code != c.code || out.String() != c.stdout {
						t.Errorf("check exited %d and printed:\n%s\nwant %d and:\n%s", code, out.String(), c.code, c.stdout)
					}
				})
			}
		})
	}
}

// addCatalog points the config at path to a catalog folder beside it that
// holds files, and adds tools to the tools of support_bot in access.yaml.
func addCatalog(t *testing.T, path string, files map[string]string, tools ...string) {
	t.Helper()
	dir := filepath.Dir(path)
	for name, change := range map[string][2]string{
		"server.yaml": {"catalog: none", "catalog: ./catalog"},
		"access.yaml": {"tools: [list_databases, list_tables, read_rows]", "tools: [list_databases, list_tables, read_rows, " + strings.Join(tools, ", ") + "]"},
	} {
		text, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(text), change[0]) {
			t.Fatalf("%s does not hold %q (%v)", name, change[0], err)
		}
		files[name] = strings.Replace(string(text), change[0], change[1], 1)
	}
	for name, text := range files {
		if name != "server.yaml" && name != "access.yaml" {
			name = filepath.Join("catalog", name)
		}
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// callCatalog serves the config at path on HTTP, calls tool with args with
// the known token, and stops the server. It returns the text of the answer
// and every log line.
func callCatalog(t *testing.T, path string, env map[string]string, tool string, args map[string]any) string {
	t.Helper()
	logs := newSyncBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"serve", "-config", path}, io.Discard, logs, lookup(env)) }()
	url := waitForAddress(t, logs)
	call, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{}}, MaxRetries: -1}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(call, tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := cs.CallTool(call, &mcp.CallToolParams{Name: tool, Arguments: args})
	cs.Close()
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !res.IsError {
		t.Errorf("%s answered %q without isError, want a failed call", tool, text)
	}
	cancel()
	if code := <-exited; code != 0 {
		t.Errorf("serve exited %d after its context ended, want 0", code)
	}
	return text + "\n" + logs.String()
}
