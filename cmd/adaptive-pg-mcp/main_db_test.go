//go:build pg

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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

			// serve on HTTP with the right password, refused and admitted requests, and list_databases.
			serveHTTP(t, path, env, keep)

			for _, secret := range []string{password, knownToken, env["SALES_EU_DSN"]} {
				if strings.Contains(seen.String(), secret) {
					t.Errorf("a log line, an error or an answer shows %q:\n%s", secret, seen.String())
				}
			}
			for _, must := range []string{"password authentication failed for user", "does not exist", "refused an HTTP request", "list_databases found 1 database: helpdesk."} {
				if !strings.Contains(seen.String(), must) {
					t.Errorf("the capture misses %q, which it should hold:\n%s", must, seen.String())
				}
			}
		})
	}
}
