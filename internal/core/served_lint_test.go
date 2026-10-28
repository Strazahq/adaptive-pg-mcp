package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

func TestServedToolsPassTheToolLint(t *testing.T) {
	reads := []string{"aggregate", "describe_table", "list_databases", "list_schemas", "list_tables", "read_rows"}
	writes := []string{"delete_rows", "insert_rows", "update_rows"}
	cases := []struct {
		freeSQL      config.FreeSQL
		reads, write []string
	}{
		{config.FreeSQLOff, reads, writes},
		{config.FreeSQLRead, append(slices.Clone(reads), "query_sql"), writes},
		{config.FreeSQLReadWrite, append(slices.Clone(reads), "query_sql"), append(slices.Clone(writes), "execute_ddl", "execute_sql")},
	}
	modes := []struct {
		mode          config.Mode
		token, caller string
	}{
		{config.Straza, gatewayToken, gatewayCaller},
	}
	for _, m := range modes {
		for _, c := range cases {
			s := New(dataConfig(m.mode, c.freeSQL), slog.New(slog.DiscardHandler))
			for _, v := range mcp.SupportedProtocolVersions() {
				sessions := map[string]*mcp.ClientSession{"http": overHandler(t, s, m.token, v), "memory": overMemory(t, s, m.caller, v)}
				for transport, cs := range sessions {
					t.Run(string(m.mode)+"/free_sql "+string(c.freeSQL)+"/"+v+"/"+transport, func(t *testing.T) {
						tools, byName := toolsOf(t, cs)
						lintTools(t, tools)
						want := slices.Sorted(slices.Values(append(slices.Clone(c.reads), c.write...)))
						var got []string
						for _, tool := range tools {
							got = append(got, tool.Name)
						}
						if slices.Sort(got); !reflect.DeepEqual(got, want) {
							t.Fatalf("tools/list holds %v, want %v", got, want)
						}
						for _, name := range want {
							lintDataTool(t, byName[name], slices.Contains(c.write, name), []any{"helpdesk", "sales_eu"})
						}
					})
				}
			}
		}
	}
}

// lintDataTool fails t when a data tool breaks docs/DESIGN.md section 4: a
// read tool sets readOnlyHint and a write tool destructiveHint, every data
// tool's database is limited to databases, and every tool has an output
// schema with the read or write shape.
func lintDataTool(t *testing.T, m map[string]any, write bool, databases []any) {
	t.Helper()
	name, _ := m["name"].(string)
	ann, _ := m["annotations"].(map[string]any)
	if ann["readOnlyHint"] != !write || ann["destructiveHint"] != write || ann["openWorldHint"] != false {
		t.Errorf("tool %s: the hints are %v, want readOnlyHint %v, destructiveHint %v and openWorldHint false", name, ann, !write, write)
	}
	if name == listDatabasesName {
		return
	}
	out, _ := m["outputSchema"].(map[string]any)
	props, _ := out["properties"].(map[string]any)
	shape := []string{"columns", "rows", "truncated"}
	if write {
		shape = []string{"rowsAffected", "columns", "rows"}
	}
	for _, key := range shape {
		if _, ok := props[key]; !ok || !slices.Contains(required(out), key) {
			t.Errorf("tool %s: the output schema does not require %s", name, key)
		}
	}
	in, _ := m["inputSchema"].(map[string]any)
	inProps, _ := in["properties"].(map[string]any)
	db, _ := inProps[databaseArg].(map[string]any)
	if enum, _ := db["enum"].([]any); !reflect.DeepEqual(enum, databases) {
		t.Errorf("tool %s: the database argument takes %v, want %v", name, db["enum"], databases)
	}
}

// withCatalogTools serves two catalog tools on s the way the start checks
// serve them, with the parameter types that catalog.Check fills in:
// revenue_eu on sales_eu, and tickets_by_day on helpdesk and sales_eu.
func withCatalogTools(t *testing.T, s *Server) *Server {
	t.Helper()
	queue, half, most := 8, 0.5, 99.5
	for _, e := range []catalog.Entry{
		{Name: "revenue_eu", Description: "Sums revenue per region for one month. Answers one row per region, at most the row cap.", Mode: catalog.Read, Databases: []string{"sales_eu"}, Login: "read", SQL: "select 1", Source: "finance/revenue.sql",
			Params: []catalog.Param{
				{Name: "month", Description: "The first day of the month, such as 2026-09-01.", Type: "date", Required: true},
				{Name: "min_share", Description: "The smallest share of the total a region must have, from 0.5 to 99.5.", Type: "numeric", Minimum: &half, Maximum: &most},
			}},
		{Name: "tickets_by_day", Description: "Counts tickets per day. Answers one row per day, at most the row cap.", Mode: catalog.Read, Databases: []string{"helpdesk", "sales_eu"}, Login: "read", SQL: "select 1", Source: "support/tickets.sql",
			Params: []catalog.Param{
				{Name: "day", Description: "The first day to count, such as 2026-10-01.", Type: "date", Required: true},
				{Name: "queue", Description: "The queue in lowercase letters, or none for every queue.", Type: "text", Pattern: "^[a-z]+$", MaxLength: &queue},
			}},
	} {
		if err := s.addEntry(e); err != nil {
			t.Fatalf("addEntry(%s): %v", e.Name, err)
		}
	}
	return s
}

func TestServedCatalogToolsPassTheToolLint(t *testing.T) {
	s := withCatalogTools(t, New(dataConfig(config.Straza, config.FreeSQLOff), slog.New(slog.DiscardHandler)))
	write := catalog.Entry{
		Name: "close_tickets", Description: "Closes the tickets of one queue. Answers rowsAffected, and changes at most the write cap.", Mode: catalog.Write,
		Databases: []string{"helpdesk"}, Login: "write", SQL: "update tickets set status = 'closed' where queue = $1", Source: "support/close.sql",
		Params: []catalog.Param{{Name: "queue", Description: "The queue to close.", Type: "text", Required: true}},
	}
	if err := s.addEntry(write); err != nil {
		t.Fatalf("addEntry(%s): %v", write.Name, err)
	}
	for _, v := range mcp.SupportedProtocolVersions() {
		for transport, cs := range map[string]*mcp.ClientSession{"http": overHandler(t, s, gatewayToken, v), "memory": overMemory(t, s, gatewayCaller, v)} {
			t.Run(v+"/"+transport, func(t *testing.T) {
				tools, byName := toolsOf(t, cs)
				lintTools(t, tools)
				if len(tools) != 12 {
					t.Errorf("tools/list holds %d tools, want the 9 built-in tools of free_sql off and the 3 catalog tools", len(tools))
				}
				lintDataTool(t, byName["revenue_eu"], false, []any{"sales_eu"})
				lintDataTool(t, byName["tickets_by_day"], false, []any{"helpdesk", "sales_eu"})
				lintDataTool(t, byName["close_tickets"], true, []any{"helpdesk"})
			})
		}
	}
}

func TestToolsListIsFilteredPerCaller(t *testing.T) {
	helpdesk, both := []any{"helpdesk"}, []any{"helpdesk", "sales_eu"}
	callers := []struct {
		name, token string
		freeSQL     config.FreeSQL
		// want maps each tool the caller sees to the databases its enum holds.
		want map[string][]any
	}{
		{"support_bot", tokenA, config.FreeSQLReadWrite, map[string][]any{
			"aggregate": helpdesk, "delete_rows": helpdesk, "describe_table": helpdesk, "insert_rows": helpdesk,
			"list_tables": helpdesk, "read_rows": helpdesk, "tickets_by_day": helpdesk,
		}},
		{"support_bot", tokenA, config.FreeSQLOff, map[string][]any{
			"aggregate": helpdesk, "delete_rows": helpdesk, "describe_table": helpdesk, "insert_rows": helpdesk,
			"list_tables": helpdesk, "read_rows": helpdesk, "tickets_by_day": helpdesk,
		}},
		{"ops_bot", tokenB, config.FreeSQLReadWrite, map[string][]any{
			"execute_ddl": both, "execute_sql": both, "list_databases": nil, "list_schemas": both, "query_sql": both, "read_rows": both, "tickets_by_day": both, "update_rows": both,
		}},
		{"ops_bot", tokenB, config.FreeSQLRead, map[string][]any{
			"list_databases": nil, "list_schemas": both, "query_sql": both, "read_rows": both, "tickets_by_day": both, "update_rows": both,
		}},
		{"empty_bot", tokenEmpty, config.FreeSQLReadWrite, map[string][]any{}},
		{"nodb_bot", tokenNoDB, config.FreeSQLReadWrite, map[string][]any{"list_databases": nil}},
	}
	for _, c := range callers {
		s := withCatalogTools(t, New(dataConfig(config.Standalone, c.freeSQL), slog.New(slog.DiscardHandler)))
		for _, v := range mcp.SupportedProtocolVersions() {
			for transport, cs := range map[string]*mcp.ClientSession{"http": overHandler(t, s, c.token, v), "memory": overMemory(t, s, c.name, v)} {
				t.Run(c.name+"/free_sql "+string(c.freeSQL)+"/"+v+"/"+transport, func(t *testing.T) {
					res, err := cs.ListTools(context.Background(), nil)
					if err != nil {
						t.Fatalf("tools/list: %v", err)
					}
					if res.Tools == nil {
						t.Fatalf("tools/list answered no list, want an empty one at least")
					}
					if v == "2026-07-28" && res.CacheScope != "private" {
						t.Errorf("the cache scope of a list for one caller is %q, want private", res.CacheScope)
					}
					lintTools(t, res.Tools)
					_, byName := toolsOf(t, cs)
					var got []string
					for name := range byName {
						got = append(got, name)
					}
					want := slices.Sorted(maps.Keys(c.want))
					if slices.Sort(got); !reflect.DeepEqual(got, want) {
						t.Fatalf("%s sees %v, want %v", c.name, got, want)
					}
					for name, databases := range c.want {
						if databases != nil && name != "tickets_by_day" {
							lintDataTool(t, byName[name], slices.Contains([]string{"insert_rows", "update_rows", "delete_rows", "execute_sql", "execute_ddl"}, name), databases)
						}
						if databases != nil {
							in, _ := byName[name]["inputSchema"].(map[string]any)
							props, _ := in["properties"].(map[string]any)
							db, _ := props[databaseArg].(map[string]any)
							if enum, _ := db["enum"].([]any); !reflect.DeepEqual(enum, databases) {
								t.Errorf("%s: the database argument takes %v, want %v", name, db["enum"], databases)
							}
						}
					}
				})
			}
		}
	}
	t.Run("in mode straza every caller sees every tool and database", func(t *testing.T) {
		s := withCatalogTools(t, New(dataConfig(config.Straza, config.FreeSQLReadWrite), slog.New(slog.DiscardHandler)))
		for _, v := range mcp.SupportedProtocolVersions() {
			_, byName := toolsOf(t, overHandler(t, s, gatewayToken, v))
			if len(byName) != 14 {
				t.Errorf("%s: Straza sees %d tools, want 14", v, len(byName))
			}
			if ann, _ := byName["execute_ddl"]["annotations"].(map[string]any); ann["destructiveHint"] != true || ann["idempotentHint"] != false {
				t.Errorf("%s: execute_ddl has the hints %v, want destructiveHint true and idempotentHint false", v, ann)
			}
			in, _ := byName["revenue_eu"]["inputSchema"].(map[string]any)
			props, _ := in["properties"].(map[string]any)
			if db, _ := props[databaseArg].(map[string]any); !reflect.DeepEqual(db["enum"], []any{"sales_eu"}) {
				t.Errorf("%s: revenue_eu takes %v, want its entry's databases", v, db["enum"])
			}
		}
	})
}

// outputs holds the output schema of every tool a session lists, resolved for validation.
type outputs map[string]*jsonschema.Resolved

// outputsOf resolves the output schemas of the tools that cs lists.
func outputsOf(t *testing.T, cs *mcp.ClientSession) outputs {
	t.Helper()
	tools, _ := toolsOf(t, cs)
	out := outputs{}
	for _, tool := range tools {
		raw, _ := json.Marshal(tool.OutputSchema)
		var s jsonschema.Schema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("tool %s: the output schema does not parse: %v", tool.Name, err)
		}
		r, err := s.Resolve(nil)
		if err != nil {
			t.Fatalf("tool %s: the output schema does not resolve: %v", tool.Name, err)
		}
		out[tool.Name] = r
	}
	return out
}

// check fails t unless res answers tool without isError, with one short text
// for people and structuredContent that matches the tool's output schema.
func (o outputs) check(t *testing.T, tool string, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("%s refused: %s", tool, textOf(t, res))
	}
	if text := textOf(t, res); text == "" || len(text) > 400 {
		t.Errorf("%s answered a text of %d bytes, want a short sentence: %s", tool, len(text), text)
	}
	m := structured(t, res)
	if err := o[tool].Validate(m); err != nil {
		t.Errorf("%s answered structuredContent that does not match its output schema: %v\n%v", tool, err, m)
	}
	return m
}
