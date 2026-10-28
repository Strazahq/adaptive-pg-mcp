package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// dataConfig is a config with the databases helpdesk and sales_eu, free SQL
// at freeSQL, and no pools, so a call that passes every check before the
// statement answers the refusal of a server that has not started.
func dataConfig(mode config.Mode, freeSQL config.FreeSQL) *config.Config {
	cfg := &config.Config{
		Mode:      mode,
		Listen:    "127.0.0.1:0",
		FreeSQL:   freeSQL,
		Limits:    config.Limits{Rows: 1000, Bytes: 1 << 20, Timeout: 30 * time.Second, WriteRows: 100},
		Databases: map[string]config.Database{"helpdesk": {}, "sales_eu": {}},
	}
	if mode == config.Straza {
		cfg.GatewayTokens = []config.Hash{hashOf(gatewayToken)}
		return cfg
	}
	cfg.Callers = []config.Caller{
		{
			Name: "support_bot", TokenSHA256: hashOf(tokenA),
			Tools:     []string{"list_tables", "describe_table", "read_rows", "aggregate", "insert_rows", "delete_rows", "query_sql", "execute_ddl", "revenue_eu", "tickets_by_day"},
			Databases: []string{"helpdesk"},
			Tables:    []string{"public.tickets", "audit.log"},
		},
		{
			Name: "ops_bot", TokenSHA256: hashOf(tokenB),
			Tools:     []string{"list_databases", "list_schemas", "read_rows", "update_rows", "query_sql", "execute_sql", "execute_ddl", "tickets_by_day"},
			Databases: []string{"helpdesk", "sales_eu"},
		},
		{Name: "empty_bot", TokenSHA256: hashOf(tokenEmpty), Tools: []string{}, Databases: []string{"helpdesk"}},
		{Name: "nodb_bot", TokenSHA256: hashOf(tokenNoDB), Tools: []string{"list_databases", "read_rows"}, Databases: []string{}},
	}
	return cfg
}

var (
	// tokenEmpty is the token of empty_bot in dataConfig, whose entry lists no tool.
	tokenEmpty = testToken("empty-bot")
	// tokenNoDB is the token of nodb_bot in dataConfig, whose entry lists no database.
	tokenNoDB = testToken("nodb-bot")
)

// overMemory connects an SDK client to s over an in-memory transport for
// caller, the way serving on stdio hands the checked caller to every request,
// asking for revision v.
func overMemory(t *testing.T, s *Server, caller, v string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.mcp.Connect(context.WithValue(ctx, stdioCallerKey{}, caller), st, nil)
	if err != nil {
		cancel()
		t.Fatalf("serve in memory: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: v})
	if err != nil {
		cancel()
		t.Fatalf("connect in memory: %v", err)
	}
	t.Cleanup(func() {
		cs.Close()
		ss.Close()
		cancel()
	})
	return cs
}

// overHandler serves s.Handler on a loopback test server and connects an SDK client with token, asking for revision v.
func overHandler(t *testing.T, s *Server, token, v string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	cs, err := overHTTP(t, ts.URL+endpointPath, token, v)
	if err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	return cs
}

// callTool calls tool with args and meta, and fails the test on a protocol error, where a tool error belongs.
func callTool(t *testing.T, cs *mcp.ClientSession, tool string, args any, meta mcp.Meta) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: meta, Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s answered a protocol error, where a tool error belongs: %v", tool, err)
	}
	return res
}

// notStarted is what a call that passes every check before its statement answers on a server without pools.
func notStarted(tool, database string) string {
	return tool + " on " + database + " cannot run, because adaptive-pg-mcp has not run its start checks, so it has no connection to the database. Serve it with adaptive-pg-mcp serve, which runs them before it serves."
}

func TestAuthorizationOnEveryRevision(t *testing.T) {
	const (
		toolRefused = "support_bot may not use update_rows, because its entry in access.yaml does not list that tool, so the call is refused. Use a tool that its entry lists, or ask an administrator to add update_rows to its tools."
		dbRefused   = "support_bot may not use sales_eu, because its entry in access.yaml does not list that database, so the call is refused. Name a database that list_databases answers, or ask an administrator to add sales_eu to its databases."
		freeRefused = "support_bot may not use query_sql, because its entry in access.yaml narrows it to tables, and free SQL can reach any table, so the call is refused. Use the table tools, such as read_rows, on its tables, or ask an administrator to remove tables from its entry."
	)
	tableRefused := func(table string) string {
		return "support_bot may not use " + table + " on helpdesk, because its entry in access.yaml narrows it to the tables public.tickets and audit.log, so the call is refused. Name one of those tables, or ask an administrator to add " + table + " to its tables."
	}
	where := []any{map[string]any{"column": "id", "op": "=", "value": 1}}
	cases := []struct {
		name, caller, tool string
		args               map[string]any
		want               string
	}{
		{"a tool its entry does not list", "support_bot", "update_rows", map[string]any{"database": "helpdesk", "table": "tickets", "set": map[string]any{"status": "closed"}, "where": where}, toolRefused},
		{"a tool it does not list, before its arguments are read", "support_bot", "update_rows", map[string]any{"database": 5}, toolRefused},
		{"a database its entry does not list", "support_bot", "read_rows", map[string]any{"database": "sales_eu", "table": "tickets"}, dbRefused},
		{"a database it does not list, before its arguments are read", "support_bot", "read_rows", map[string]any{"database": "sales_eu", "no_such_argument": true}, dbRefused},
		{"a database the config does not have", "support_bot", "read_rows", map[string]any{"database": "finance", "table": "tickets"}, "support_bot may not use finance, because its entry in access.yaml does not list that database, so the call is refused. Name a database that list_databases answers, or ask an administrator to add finance to its databases."},
		{"a table outside its tables", "support_bot", "read_rows", map[string]any{"database": "helpdesk", "table": "customers"}, tableRefused("public.customers")},
		{"a table outside its tables, by schema", "support_bot", "read_rows", map[string]any{"database": "helpdesk", "schema": "private", "table": "tickets"}, tableRefused("private.tickets")},
		{"a schema that differs only in case", "support_bot", "describe_table", map[string]any{"database": "helpdesk", "schema": "Public", "table": "tickets"}, tableRefused("Public.tickets")},
		{"a write to a table outside its tables", "support_bot", "insert_rows", map[string]any{"database": "helpdesk", "table": "customers", "rows": []any{map[string]any{"name": "x"}}}, tableRefused("public.customers")},
		{"a delete outside its tables", "support_bot", "delete_rows", map[string]any{"database": "helpdesk", "schema": "audit", "table": "log2", "where": where}, tableRefused("audit.log2")},
		{"an aggregate outside its tables", "support_bot", "aggregate", map[string]any{"database": "helpdesk", "table": "orders", "function": "count"}, tableRefused("public.orders")},
		{"free SQL for a caller with tables", "support_bot", "query_sql", map[string]any{"database": "helpdesk", "sql": "SELECT 1"}, freeRefused},
		{"a table in its tables passes", "support_bot", "read_rows", map[string]any{"database": "helpdesk", "table": "tickets"}, notStarted("read_rows", "helpdesk")},
		{"a table in its tables by schema passes", "support_bot", "aggregate", map[string]any{"database": "helpdesk", "schema": "audit", "table": "log", "function": "count"}, notStarted("aggregate", "helpdesk")},
		{"a write to a table in its tables passes", "support_bot", "insert_rows", map[string]any{"database": "helpdesk", "table": "tickets", "rows": []any{map[string]any{"status": "open"}}}, notStarted("insert_rows", "helpdesk")},
		{"listing tables passes", "support_bot", "list_tables", map[string]any{"database": "helpdesk"}, notStarted("list_tables", "helpdesk")},
		{"a caller without tables reaches any table", "ops_bot", "update_rows", map[string]any{"database": "sales_eu", "schema": "billing", "table": "invoices", "set": map[string]any{"paid": true}, "where": where}, notStarted("update_rows", "sales_eu")},
		{"a caller without tables may use free SQL", "ops_bot", "execute_sql", map[string]any{"database": "sales_eu", "sql": "SELECT 1"}, notStarted("execute_sql", "sales_eu")},
		{"a caller without tables may use execute_ddl", "ops_bot", "execute_ddl", map[string]any{"database": "helpdesk", "sql": "CREATE TABLE t (a int)"}, notStarted("execute_ddl", "helpdesk")},
		{"execute_ddl for a caller with tables", "support_bot", "execute_ddl", map[string]any{"database": "helpdesk", "sql": "DROP TABLE tickets"}, "support_bot may not use execute_ddl, because its entry in access.yaml narrows it to tables, and free SQL can reach any table, so the call is refused. Use the table tools, such as read_rows, on its tables, or ask an administrator to remove tables from its entry."},
		{"execute_ddl when its entry does not list it", "nodb_bot", "execute_ddl", map[string]any{"database": "helpdesk", "sql": "DROP TABLE tickets"}, "nodb_bot may not use execute_ddl, because its entry in access.yaml does not list that tool, so the call is refused. Use a tool that its entry lists, or ask an administrator to add execute_ddl to its tools."},
		{"list_databases when its entry does not list it", "support_bot", "list_databases", map[string]any{}, "support_bot may not use list_databases, because its entry in access.yaml does not list that tool, so the call is refused. Use a tool that its entry lists, or ask an administrator to add list_databases to its tools."},
		{"list_databases when its entry does not list it, before its arguments are read", "support_bot", "list_databases", map[string]any{"cursor": 5}, "support_bot may not use list_databases, because its entry in access.yaml does not list that tool, so the call is refused. Use a tool that its entry lists, or ask an administrator to add list_databases to its tools."},
		{"list_databases when its entry lists it passes", "ops_bot", "list_databases", map[string]any{}, "list_databases cannot read the id of helpdesk on its read login, because adaptive-pg-mcp has not run its start checks, so it has no connection to the database, so it answers no page. Run adaptive-pg-mcp check to see why, and fix the login."},
		{"another caller is refused a tool it does not list", "ops_bot", "delete_rows", map[string]any{"database": "helpdesk", "table": "tickets", "where": where}, "ops_bot may not use delete_rows, because its entry in access.yaml does not list that tool, so the call is refused. Use a tool that its entry lists, or ask an administrator to add delete_rows to its tools."},
		{"a catalog tool its entry does not list", "ops_bot", "revenue_eu", map[string]any{"database": "sales_eu", "month": "2026-09-01"}, "ops_bot may not use revenue_eu, because its entry in access.yaml does not list that tool, so the call is refused. Use a tool that its entry lists, or ask an administrator to add revenue_eu to its tools."},
		{"a catalog tool on a database its entry does not list", "support_bot", "tickets_by_day", map[string]any{"database": "sales_eu", "day": "2026-10-01"}, dbRefused},
		{"a catalog tool its entry lists passes, also for a caller narrowed to tables", "support_bot", "tickets_by_day", map[string]any{"database": "helpdesk", "day": "2026-10-01"}, notStarted("tickets_by_day", "helpdesk")},
	}
	standalone := withCatalogTools(t, New(dataConfig(config.Standalone, config.FreeSQLReadWrite), slog.New(slog.DiscardHandler)))
	straza := withCatalogTools(t, New(dataConfig(config.Straza, config.FreeSQLReadWrite), slog.New(slog.DiscardHandler)))
	tokens := map[string]string{"support_bot": tokenA, "ops_bot": tokenB, "nodb_bot": tokenNoDB}
	for _, v := range mcp.SupportedProtocolVersions() {
		t.Run(v, func(t *testing.T) {
			sessions := map[string]map[string]*mcp.ClientSession{}
			for caller, token := range tokens {
				sessions[caller] = map[string]*mcp.ClientSession{
					"http":   overHandler(t, standalone, token, v),
					"memory": overMemory(t, standalone, caller, v),
				}
			}
			for _, c := range cases {
				for transport, cs := range sessions[c.caller] {
					t.Run(transport+"/"+c.name, func(t *testing.T) {
						res := callTool(t, cs, c.tool, c.args, nil)
						if !res.IsError {
							t.Fatalf("%s answered without isError: %v", c.tool, res.StructuredContent)
						}
						if got := textOf(t, res); got != c.want {
							t.Errorf("the answer:\n got %s\nwant %s", got, c.want)
						}
					})
				}
			}
			t.Run("in mode straza Straza decides", func(t *testing.T) {
				for transport, cs := range map[string]*mcp.ClientSession{"http": overHandler(t, straza, gatewayToken, v), "memory": overMemory(t, straza, gatewayCaller, v)} {
					for _, c := range cases {
						if got := textOf(t, callTool(t, cs, c.tool, c.args, nil)); strings.Contains(got, "access.yaml") {
							t.Errorf("%s/%s in mode straza answered %s, want no access.yaml refusal", transport, c.name, got)
						}
					}
				}
			})
		})
	}
}

// structured returns the structuredContent of a result as a JSON object, as a client reads it.
func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("structuredContent does not marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		t.Fatalf("structuredContent %s is not a JSON object: %v", raw, err)
	}
	return m
}
