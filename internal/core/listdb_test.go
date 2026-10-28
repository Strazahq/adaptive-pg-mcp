package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolsOf lists the tools a session serves, as JSON objects by name.
func toolsOf(t *testing.T, cs *mcp.ClientSession) ([]*mcp.Tool, map[string]map[string]any) {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, tool := range res.Tools {
		raw, _ := json.Marshal(tool)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		byName[tool.Name] = m
	}
	return res.Tools, byName
}

// callList calls list_databases with args and returns its result.
func callList(t *testing.T, cs *mcp.ClientSession, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: listDatabasesName, Arguments: args})
	if err != nil {
		t.Fatalf("list_databases answered a protocol error, where a tool error belongs: %v", err)
	}
	return res
}

// textOf returns the one text of a result.
func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("the result has %d contents, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("the result's content is %T, want text", res.Content[0])
	}
	return text.Text
}

func TestListDatabasesPassesTheToolLint(t *testing.T) {
	for _, mode := range []struct {
		name, token string
		s           *Server
	}{
		{"standalone", tokenA, New(standaloneConfig(), slog.New(slog.DiscardHandler))},
		{"straza", gatewayToken, New(strazaConfig(), slog.New(slog.DiscardHandler))},
	} {
		t.Run(mode.name, func(t *testing.T) {
			tools, byName := toolsOf(t, overStdio(t, mode.s, mode.token, ""))
			lintTools(t, tools)
			m := byName[listDatabasesName]
			if m == nil {
				t.Fatalf("tools/list does not hold %s", listDatabasesName)
			}
			wantMeta := map[string]any{
				"ai.straza/object":        map[string]any{"type": "database", "reach": "all"},
				"ai.straza/lists-objects": map[string]any{"type": "database"},
			}
			if !reflect.DeepEqual(m["_meta"], wantMeta) {
				t.Errorf("_meta is %v, want %v", m["_meta"], wantMeta)
			}
			wantHints := map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
			if !reflect.DeepEqual(m["annotations"], wantHints) {
				t.Errorf("annotations are %v, want %v", m["annotations"], wantHints)
			}
		})
	}
}

func TestListDatabasesRefusesBadArguments(t *testing.T) {
	cs := overStdio(t, New(standaloneConfig(), slog.New(slog.DiscardHandler)), tokenA, "")
	cursorOf := func(name string) string { return base64.RawURLEncoding.EncodeToString([]byte(name)) }
	badCursor := "the cursor of list_databases does not decode to a page of this listing, so the call is refused. Pass the nextCursor of the previous page exactly as it came, or leave cursor out to start from the first page."
	cases := []struct {
		name string
		args any
		want string
	}{
		{"a database argument", map[string]any{"database": "helpdesk"}, `list_databases takes no argument but cursor, and the call passed "database", so it is refused. Leave "database" out.`},
		{"a cursor that is a number", map[string]any{"cursor": 5}, "the cursor of list_databases is not a string, so the call is refused. Pass the nextCursor of the previous page as it came, or leave cursor out for the first page."},
		{"a cursor that is null", map[string]any{"cursor": nil}, "the cursor of list_databases is not a string, so the call is refused. Pass the nextCursor of the previous page as it came, or leave cursor out for the first page."},
		{"arguments that are a list", []any{"cursor"}, `the arguments of list_databases are not a JSON object, so the call is refused. Pass an object, such as {} for the first page or {"cursor": "..."} for the next.`},
		{"a cursor that is not base64url", map[string]any{"cursor": "!!"}, badCursor},
		{"an empty cursor", map[string]any{"cursor": ""}, badCursor},
		{"a cursor of a database the caller may not use", map[string]any{"cursor": cursorOf("sales_eu")}, badCursor},
		{"a cursor of a database this server has no pool for", map[string]any{"cursor": cursorOf("helpdesk")}, "list_databases cannot read the id of helpdesk on its read login, because no pool is open for the read login of helpdesk, so it answers no page. Run adaptive-pg-mcp check to see why, and fix the login."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := callList(t, cs, c.args)
			if !res.IsError {
				t.Fatalf("list_databases answered without isError: %v", res.StructuredContent)
			}
			if got := textOf(t, res); got != c.want {
				t.Errorf("the refusal:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestListDatabasesWithoutDatabases(t *testing.T) {
	cs := overStdio(t, New(strazaConfig(), slog.New(slog.DiscardHandler)), gatewayToken, "")
	for _, args := range []any{nil, map[string]any{}} {
		res := callList(t, cs, args)
		if res.IsError {
			t.Fatalf("list_databases refused: %s", textOf(t, res))
		}
		if got, want := textOf(t, res), "list_databases found no database the caller may use."; got != want {
			t.Errorf("the text is %q, want %q", got, want)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if string(raw) != `{"objects":[]}` {
			t.Errorf("structuredContent is %s, want an empty objects list and no nextCursor", raw)
		}
	}
}
