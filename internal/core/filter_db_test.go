//go:build pg

package core

import (
	"encoding/hex"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

func TestToolsListIsFilteredOnStdio(t *testing.T) {
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			helpdesk, sales := pgtest.NewDatabase(t, s), pgtest.NewDatabase(t, s)
			dir := t.TempDir()
			a, b := hashOf(tokenA), hashOf(tokenB)
			files := map[string]string{
				"server.yaml": "version: 1\nmode: standalone\nlisten: stdio\nfree_sql: read\nlimits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }\naccess: access.yaml\ncatalog: none\ndatabases:\n" + helpdeskSingle + single,
				"access.yaml": "version: 1\ncallers:\n" +
					"  - name: support_bot\n    token_sha256: \"" + hex.EncodeToString(a[:]) + "\"\n    tools: [list_databases, read_rows, query_sql]\n    databases: [helpdesk]\n" +
					"  - name: ops_bot\n    token_sha256: \"" + hex.EncodeToString(b[:]) + "\"\n    tools: []\n    databases: [sales_eu]\n",
			}
			for name, text := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{"HELPDESK_DSN": l.DSN(t, helpdesk), "SALES_EU_DSN": l.DSN(t, sales)}
			cfg, err := config.Load(filepath.Join(dir, "server.yaml"), func(k string) string { return env[k] })
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			srv := New(cfg, slog.New(slog.DiscardHandler))
			for _, v := range mcp.SupportedProtocolVersions() {
				t.Run(v, func(t *testing.T) {
					tools, byName := toolsOf(t, overStdio(t, srv, tokenA, v))
					lintTools(t, tools)
					if got := slices.Sorted(maps.Keys(byName)); !reflect.DeepEqual(got, []string{"list_databases", "query_sql", "read_rows"}) {
						t.Errorf("support_bot sees %v over stdio, want list_databases, query_sql and read_rows", got)
					}
					in, _ := byName["read_rows"]["inputSchema"].(map[string]any)
					props, _ := in["properties"].(map[string]any)
					if db, _ := props[databaseArg].(map[string]any); !reflect.DeepEqual(db["enum"], []any{"helpdesk"}) {
						t.Errorf("support_bot's read_rows takes %v, want helpdesk only", db["enum"])
					}
					if tools, _ := toolsOf(t, overStdio(t, srv, tokenB, v)); len(tools) != 0 {
						t.Errorf("ops_bot, whose entry lists no tool, sees %d tools over stdio, want none", len(tools))
					}
				})
			}
		})
	}
}

// loadStandalone writes server.yaml in mode standalone with the limits, the databases section and access.yaml, and loads them with env.
func loadStandalone(t *testing.T, limits, databases, access string, env map[string]string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	server := "version: 1\nmode: standalone\nlisten: stdio\nfree_sql: \"off\"\nlimits: " + limits + "\naccess: access.yaml\ncatalog: none\ndatabases:\n" + databases
	for name, text := range map[string]string{"server.yaml": server, "access.yaml": access} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(filepath.Join(dir, "server.yaml"), func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// pages calls a listing tool from args to its last page and returns every row as JSON.
func pages(t *testing.T, cs *mcp.ClientSession, out outputs, tool string, args map[string]any) string {
	t.Helper()
	all := []any{}
	for page := 1; ; page++ {
		m := out.check(t, tool, callTool(t, cs, tool, args, nil))
		rows, _ := m["rows"].([]any)
		all = append(all, rows...)
		next, _ := m["nextCursor"].(string)
		if next == "" {
			break
		}
		if page > 10 {
			t.Fatalf("%s holds more than 10 pages", tool)
		}
		args = maps.Clone(args)
		args["cursor"] = next
	}
	return jsonOf(t, all)
}

func TestListingsNarrowToTheCallersTables(t *testing.T) {
	entry := func(name, token, tables string) string {
		h := hashOf(token)
		e := "  - name: " + name + "\n    token_sha256: \"" + hex.EncodeToString(h[:]) + "\"\n    tools: [list_schemas, list_tables]\n    databases: [helpdesk]\n"
		if tables != "" {
			e += "    tables: [" + tables + "]\n"
		}
		return e
	}
	access := "version: 1\ncallers:\n" +
		entry("audit_bot", tokenA, "audit.log, hidden.x, nodot") +
		entry("public_bot", tokenB, "public.tickets, public.kinds, public.open_tickets, public.secrets, public.missing") +
		entry("wide_bot", tokenEmpty, "")
	const everyTable = `[["audit","log","table"],["public","kinds","table"],["public","odd \"name\"; DROP TABLE tickets; --","table"],["public","open_tickets","view"],["public","tickets","table"]]`
	position := func(schema, table string) string {
		return encodeCursor(pagePosition{Tool: listTablesName, Database: "helpdesk", Schema: schema, Table: table})
	}
	cases := []struct {
		name, token, tool string
		args              map[string]any
		want              string
	}{
		{"a caller's schemas are the schemas of its tables", tokenA, "list_schemas", map[string]any{}, `[["audit"]]`},
		{"a caller's tables are its tables that the login may use", tokenA, "list_tables", map[string]any{}, `[["audit","log","table"]]`},
		{"its tables page on the narrowed list", tokenB, "list_tables", map[string]any{}, `[["public","kinds","table"],["public","open_tickets","view"],["public","tickets","table"]]`},
		{"its tables in one schema", tokenB, "list_tables", map[string]any{"schema": "public"}, `[["public","kinds","table"],["public","open_tickets","view"],["public","tickets","table"]]`},
		{"its tables in a schema it has none in", tokenB, "list_tables", map[string]any{"schema": "audit"}, `[]`},
		{"its schemas", tokenB, "list_schemas", map[string]any{}, `[["public"]]`},
		{"a crafted cursor before the first table stays in the narrowed list", tokenA, "list_tables", map[string]any{"cursor": position("", "")}, `[["audit","log","table"]]`},
		{"a crafted cursor past its tables reaches nothing more", tokenB, "list_tables", map[string]any{"cursor": position("audit", "log")}, `[["public","kinds","table"],["public","open_tickets","view"],["public","tickets","table"]]`},
		{"a crafted schemas cursor stays in the narrowed list", tokenA, "list_schemas", map[string]any{"cursor": encodeCursor(pagePosition{Tool: listSchemasName, Database: "helpdesk", Schema: ""})}, `[["audit"]]`},
		{"a caller without tables sees every schema", tokenEmpty, "list_schemas", map[string]any{}, `[["audit"],["public"]]`},
		{"a caller without tables sees every table", tokenEmpty, "list_tables", map[string]any{}, everyTable},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, readFixture...)
			srv := New(loadStandalone(t, "{ rows: 1, bytes: 1048576, timeout: 30s, write_rows: 100 }", helpdeskSingle, access, map[string]string{"HELPDESK_DSN": l.DSN(t, d)}), slog.New(slog.DiscardHandler))
			sessions := map[string]*mcp.ClientSession{}
			for _, token := range []string{tokenA, tokenB, tokenEmpty} {
				sessions[token] = overStdio(t, srv, token, "")
			}
			out := outputsOf(t, sessions[tokenEmpty])
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					c.args["database"] = "helpdesk"
					if got := pages(t, sessions[c.token], out, c.tool, c.args); got != c.want {
						t.Errorf("%s answered\n%s\nwant\n%s", c.tool, got, c.want)
					}
				})
			}
		})
	}
}
