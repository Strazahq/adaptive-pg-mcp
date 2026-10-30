package core

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// listingToken is a token Straza sends only to list_databases.
var listingToken = strings.TrimPrefix(testToken("straza-list-token"), tokenPrefix)

// listingConfig is dataConfig in straza mode, whose gateway holds
// gatewayToken for calls and listingToken for the listing.
func listingConfig() *config.Config {
	cfg := dataConfig(config.Straza, config.FreeSQLReadWrite)
	cfg.ListingTokens = []config.Hash{hashOf(listingToken)}
	return cfg
}

// listingRefused is the refusal of tool for a listing token.
func listingRefused(tool string) string {
	return tool + " refused the call, because it came with a listing token, which works on list_databases only. Send a token whose SHA-256 is in gateway_tokens_sha256 instead."
}

func TestListingTokenOnEveryRevision(t *testing.T) {
	const listed = "list_databases cannot read the id of helpdesk on its read login, because adaptive-pg-mcp has not run its start checks, so it has no connection to the database. Try again, or ask an administrator to run adaptive-pg-mcp check."
	s := withCatalogTools(t, New(listingConfig(), slog.New(slog.DiscardHandler)))
	for _, v := range mcp.SupportedProtocolVersions() {
		t.Run(v, func(t *testing.T) {
			gateway := overHandler(t, s, gatewayToken, v)
			every, _ := toolsOf(t, gateway)
			if len(every) < 10 {
				t.Fatalf("the gateway token sees %d tools, want every tool", len(every))
			}
			for transport, cs := range map[string]*mcp.ClientSession{"http": overHandler(t, s, listingToken, v), "memory": overMemory(t, s, listingCaller, v)} {
				t.Run(transport, func(t *testing.T) {
					tools, _ := toolsOf(t, cs)
					if len(tools) != 1 || tools[0].Name != listDatabasesName {
						var names []string
						for _, tool := range tools {
							names = append(names, tool.Name)
						}
						t.Errorf("the listing token sees %v, want list_databases only", names)
					}
					for _, tool := range append(every, &mcp.Tool{Name: "no_such_tool"}) {
						args, want := map[string]any{"database": 5}, listingRefused(tool.Name)
						if tool.Name == listDatabasesName {
							args, want = map[string]any{}, listed
						}
						res := callTool(t, cs, tool.Name, args, nil)
						if got := textOf(t, res); !res.IsError || got != want {
							t.Errorf("%s with the listing token answered isError %v:\n got %s\nwant %s", tool.Name, res.IsError, got, want)
						}
					}
				})
			}
			for _, tool := range every {
				if got := textOf(t, callTool(t, gateway, tool.Name, map[string]any{"database": "helpdesk"}, nil)); strings.Contains(got, "listing token") {
					t.Errorf("%s with the gateway token answered %s, want no listing refusal", tool.Name, got)
				}
			}
		})
	}
}

func TestListingTokenListIsPrivate(t *testing.T) {
	v := "2026-07-28"
	cases := []struct {
		name, token, want string
		cfg               *config.Config
	}{
		{"the gateway token beside a listing token", gatewayToken, "private", listingConfig()},
		{"the listing token", listingToken, "private", listingConfig()},
		{"the gateway token alone", gatewayToken, "public", dataConfig(config.Straza, config.FreeSQLReadWrite)},
	}
	for _, c := range cases {
		res, err := overHandler(t, New(c.cfg, slog.New(slog.DiscardHandler)), c.token, v).ListTools(t.Context(), nil)
		if err != nil {
			t.Fatalf("%s: tools/list: %v", c.name, err)
		}
		if res.CacheScope != c.want {
			t.Errorf("%s: the cache scope of the list is %q, want %s", c.name, res.CacheScope, c.want)
		}
	}
}

func TestListingTokenGetsItsOwnSession(t *testing.T) {
	url := serve(t, New(listingConfig(), slog.New(slog.DiscardHandler)))
	for _, v := range mcp.SupportedProtocolVersions() {
		if !sessionful(v) {
			continue
		}
		t.Run(v, func(t *testing.T) {
			for _, pair := range [][2]string{{gatewayToken, listingToken}, {listingToken, gatewayToken}} {
				session := open(t, url, v, pair[0])
				if a := send(t, url, whoamiCall(v, session, "Bearer "+pair[1])); a.status != http.StatusForbidden {
					t.Errorf("a request on a session of another Straza token answered %d, want 403: %s", a.status, a.body)
				}
			}
		})
	}
}

func TestListingTokenOverStdio(t *testing.T) {
	s := New(strazaConfig(), slog.New(slog.DiscardHandler))
	for _, v := range mcp.SupportedProtocolVersions() {
		t.Run(v, func(t *testing.T) {
			cs := overStdio(t, s, listingToken, v)
			if tools, _ := toolsOf(t, cs); len(tools) != 1 || tools[0].Name != listDatabasesName {
				t.Errorf("the listing token on stdio sees %d tools, want list_databases only", len(tools))
			}
			if res := callTool(t, cs, "list_schemas", map[string]any{"database": "helpdesk"}, nil); !res.IsError || textOf(t, res) != listingRefused("list_schemas") {
				t.Errorf("list_schemas with the listing token on stdio answered %s, want the listing refusal", textOf(t, res))
			}
			if res := callTool(t, cs, listDatabasesName, map[string]any{}, nil); res.IsError {
				t.Errorf("list_databases with the listing token on stdio answered %s, want the listing", textOf(t, res))
			}
		})
	}
}

func TestListingCallerNameInStandalone(t *testing.T) {
	cfg := dataConfig(config.Standalone, config.FreeSQLReadWrite)
	cfg.Callers = append(cfg.Callers, config.Caller{Name: listingCaller, TokenSHA256: hashOf(listingToken), Tools: []string{"list_databases", "list_schemas"}, Databases: []string{"helpdesk"}})
	cs := overHandler(t, New(cfg, slog.New(slog.DiscardHandler)), listingToken, mcp.SupportedProtocolVersions()[0])
	if tools, _ := toolsOf(t, cs); len(tools) != 2 {
		t.Errorf("a standalone caller named %s sees %d tools, want the 2 that access.yaml lists", listingCaller, len(tools))
	}
	if got := textOf(t, callTool(t, cs, "list_schemas", map[string]any{"database": "helpdesk"}, nil)); got != notStarted("list_schemas", "helpdesk") {
		t.Errorf("list_schemas for a standalone caller named %s answered %s, want it to pass the checks", listingCaller, got)
	}
}
