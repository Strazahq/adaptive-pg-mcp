//go:build pg

package core

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// listDatabasesSection names finance, helpdesk and sales_eu, each with one login.
const listDatabasesSection = `databases:
  finance:
    login: { dsn_env: FINANCE_DSN }
  helpdesk:
    logins:
      read: { dsn_env: HELPDESK_READ_DSN }
      write: { dsn_env: HELPDESK_WRITE_DSN }
  sales_eu:
    login: { dsn_env: SALES_EU_DSN }
`

// loadMode writes server.yaml in mode with the three databases, and access.yaml in mode standalone, and loads them with env.
func loadMode(t *testing.T, mode string, env map[string]string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	server := "version: 1\nmode: " + mode + "\nlisten: stdio\nfree_sql: \"off\"\n" +
		"limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }\ncatalog: none\n"
	if mode == "straza" {
		h := hashOf(gatewayToken)
		server += `gateway_tokens_sha256: ["` + hex.EncodeToString(h[:]) + `"]` + "\n"
	} else {
		a, b := hashOf(tokenA), hashOf(tokenB)
		access := "version: 1\ncallers:\n" +
			"  - name: support_bot\n    token_sha256: \"" + hex.EncodeToString(a[:]) + "\"\n    tools: [list_databases]\n    databases: [sales_eu, helpdesk, sales_eu]\n" +
			"  - name: ops_bot\n    token_sha256: \"" + hex.EncodeToString(b[:]) + "\"\n    tools: [list_databases]\n    databases: [finance]\n"
		if err := os.WriteFile(filepath.Join(dir, "access.yaml"), []byte(access), 0o600); err != nil {
			t.Fatal(err)
		}
		server += "access: access.yaml\n"
	}
	path := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(path, []byte(server+listDatabasesSection), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// page decodes the structured answer of list_databases.
func page(t *testing.T, res *mcp.CallToolResult) listedDatabases {
	t.Helper()
	if res.IsError {
		t.Fatalf("list_databases refused: %s", textOf(t, res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out listedDatabases
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structuredContent %s does not decode: %v", raw, err)
	}
	return out
}

func TestListDatabases(t *testing.T) {
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			reader := pgtest.NewLogin(t, s, loginPassword)
			writer := pgtest.NewLogin(t, s, loginPassword)
			dbs := map[string]pgtest.Database{"finance": pgtest.NewDatabase(t, s), "helpdesk": pgtest.NewDatabase(t, s), "sales_eu": pgtest.NewDatabase(t, s)}
			env := map[string]string{
				"FINANCE_DSN":        reader.DSN(t, dbs["finance"]),
				"HELPDESK_READ_DSN":  reader.DSN(t, dbs["helpdesk"]),
				"HELPDESK_WRITE_DSN": writer.DSN(t, dbs["helpdesk"]),
				"SALES_EU_DSN":       reader.DSN(t, dbs["sales_eu"]),
			}
			object := func(name string) listedDatabase {
				return listedDatabase{Value: name, ID: idOf(t, dbs[name], name), Title: name}
			}

			t.Run("straza pages through every database", func(t *testing.T) {
				srv := New(loadMode(t, "straza", env), slog.New(slog.DiscardHandler))
				srv.page = 2
				for _, v := range mcp.SupportedProtocolVersions() {
					t.Run(v, func(t *testing.T) {
						cs := overStdio(t, srv, gatewayToken, v)
						first := callList(t, cs, map[string]any{})
						if got, want := textOf(t, first), "list_databases found 2 databases: finance and helpdesk. More follow, so call list_databases again with cursor set to nextCursor."; got != want {
							t.Errorf("the first text is %q, want %q", got, want)
						}
						p1 := page(t, first)
						if want := []listedDatabase{object("finance"), object("helpdesk")}; !reflect.DeepEqual(p1.Objects, want) || p1.NextCursor == "" {
							t.Fatalf("the first page is %+v, want %+v and a nextCursor", p1, want)
						}
						second := callList(t, cs, map[string]any{"cursor": p1.NextCursor})
						if got, want := textOf(t, second), "list_databases found 1 database: sales_eu."; got != want {
							t.Errorf("the second text is %q, want %q", got, want)
						}
						p2 := page(t, second)
						if want := []listedDatabase{object("sales_eu")}; !reflect.DeepEqual(p2.Objects, want) || p2.NextCursor != "" {
							t.Errorf("the second page is %+v, want %+v and no nextCursor", p2, want)
						}
						if p2.NextCursor == p1.NextCursor {
							t.Errorf("the second page answers the cursor it was called with")
						}
					})
				}
			})

			cases := []struct {
				name, token string
				want        []string
			}{
				{"a standalone caller sees its own databases once each", tokenA, []string{"helpdesk", "sales_eu"}},
				{"another standalone caller sees only its own", tokenB, []string{"finance"}},
			}
			srv := New(loadMode(t, "standalone", env), slog.New(slog.DiscardHandler))
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					got := page(t, callList(t, overStdio(t, srv, c.token, ""), nil))
					var want []listedDatabase
					for _, name := range c.want {
						want = append(want, object(name))
					}
					if !reflect.DeepEqual(got.Objects, want) || got.NextCursor != "" {
						t.Errorf("list_databases answered %+v, want %+v and no nextCursor", got, want)
					}
				})
			}

			t.Run("a standalone caller cannot page into another caller's databases", func(t *testing.T) {
				res := callList(t, overStdio(t, srv, tokenA, ""), map[string]any{"cursor": base64.RawURLEncoding.EncodeToString([]byte("finance"))})
				if !res.IsError || !strings.HasPrefix(textOf(t, res), "the cursor of list_databases does not decode to a page of this listing") {
					t.Errorf("a cursor naming finance answered %v %+v, want the cursor refusal", res.IsError, res.Content)
				}
			})

			t.Run("a database and its copy under two names get two ids", func(t *testing.T) {
				copyEnv := map[string]string{}
				for k, v := range env {
					copyEnv[k] = v
				}
				copyEnv["FINANCE_DSN"] = reader.DSN(t, dbs["sales_eu"])
				got := page(t, callList(t, overStdio(t, New(loadMode(t, "straza", copyEnv), slog.New(slog.DiscardHandler)), gatewayToken, ""), nil))
				want := []listedDatabase{
					{Value: "finance", ID: idOf(t, dbs["sales_eu"], "finance"), Title: "finance"},
					object("helpdesk"),
					object("sales_eu"),
				}
				if !reflect.DeepEqual(got.Objects, want) || want[0].ID == want[2].ID {
					t.Errorf("list_databases answered %+v, want %+v with two ids for the one database", got.Objects, want)
				}
			})
		})
	}
}
