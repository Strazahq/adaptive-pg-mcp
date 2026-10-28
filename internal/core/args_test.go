package core

import (
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

func TestBadArgumentsAreToolErrors(t *testing.T) {
	refused := func(tool, why string) string {
		return tool + " refused the call, because " + why + ". Correct the arguments as the input schema of " + tool + " describes them, and call it again."
	}
	badCursor := func(tool string) string {
		return "the cursor of " + tool + " does not decode to a page of this listing, so the call is refused. Pass the nextCursor of the previous page exactly as it came, or leave cursor out to start from the first page."
	}
	where := func(op string, value any) []any {
		entry := map[string]any{"column": "id", "op": op}
		if value != nil {
			entry["value"] = value
		}
		return []any{entry}
	}
	schemasCursor := encodeCursor(pagePosition{Tool: listSchemasName, Database: "helpdesk", Schema: "public"})
	public := "public"
	tablesCursor := encodeCursor(pagePosition{Tool: listTablesName, Database: "helpdesk", Filter: &public, Schema: "public", Table: "tickets"})
	cases := []struct {
		name, tool string
		args       any
		want       string
	}{
		{"arguments that are a list", "read_rows", []any{"helpdesk"}, refused("read_rows", "the arguments are a list, not a JSON object")},
		{"no arguments", "read_rows", nil, refused("read_rows", "it needs the argument database, and the call leaves it out")},
		{"a missing table", "read_rows", map[string]any{"database": "helpdesk"}, refused("read_rows", "it needs the argument table, and the call leaves it out")},
		{"an unknown argument", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "tabel": "t"}, refused("read_rows", `it takes no argument named "tabel"`)},
		{"a database the config does not have", "read_rows", map[string]any{"database": "finance", "table": "t"}, refused("read_rows", `the argument database must be one of "helpdesk" and "sales_eu", and the call passed "finance"`)},
		{"a database that is a number", "list_tables", map[string]any{"database": 7}, refused("list_tables", "the argument database must be a string, and the call passed an integer")},
		{"an empty table", "describe_table", map[string]any{"database": "helpdesk", "table": ""}, refused("describe_table", "the argument table must not be empty")},
		{"a limit that is a string", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "limit": "5"}, refused("read_rows", "the argument limit must be an integer, and the call passed a string")},
		{"a limit with a fraction", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "limit": 1.5}, refused("read_rows", "the argument limit must be an integer, and the call passed a number")},
		{"a limit of 0", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "limit": 0}, refused("read_rows", "the argument limit must be at least 1, and the call passed 0")},
		{"a limit past the row cap", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "limit": 5000}, refused("read_rows", "the argument limit must be at most 1,000, and the call passed 5000")},
		{"an empty columns list", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "columns": []any{}}, refused("read_rows", "the argument columns must hold at least 1 entry, and the call passed none")},
		{"an op that is not in the list", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("==", 1)}, refused("read_rows", `the argument where[0].op must be one of "=", "<>", "<", "<=", ">", ">=", "in", "not in", "is null", "is not null", "like" and "ilike", and the call passed "=="`)},
		{"a where entry without op", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": []any{map[string]any{"column": "id"}}}, refused("read_rows", "the argument where[0] needs op, and the call leaves it out")},
		{"a where entry with another key", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": []any{map[string]any{"column": "id", "op": "=", "val": 1}}}, refused("read_rows", `the argument where[0] has the key "val", which it does not take`)},
		{"a where entry that is a string", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": []any{"id = 1"}}, refused("read_rows", "the argument where[0] must be an object, and the call passed a string")},
		{"an op = without a value", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("=", nil)}, refused("read_rows", "where[0] has op =, which takes a value that is not null, and the call passed none, while is null matches a null")},
		{"an op = with null", "update_rows", map[string]any{"database": "helpdesk", "table": "t", "set": map[string]any{"a": 1}, "where": []any{map[string]any{"column": "id", "op": "=", "value": nil}}}, refused("update_rows", "where[0] has op =, which takes a value that is not null, and the call passed null, while is null matches a null")},
		{"an op in with one value", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("in", 1)}, refused("read_rows", "where[0] has op in, which takes a list of values, and the call passed an integer")},
		{"an op in with an empty list", "delete_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("in", []any{})}, refused("delete_rows", "where[0] has op in, which takes 1 to 1,000 values, and the call passed 0")},
		{"an op is null with a value", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("is null", 1)}, refused("read_rows", "where[0] has op is null, which takes no value, and the call passed one")},
		{"a direction that is not in the list", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "order_by": []any{map[string]any{"column": "id", "direction": "up"}}}, refused("read_rows", `the argument order_by[0].direction must be one of "asc" and "desc", and the call passed "up"`)},
		{"sum without a column", "aggregate", map[string]any{"database": "helpdesk", "table": "t", "function": "sum"}, refused("aggregate", "the function sum needs the argument column, and the call leaves it out")},
		{"a function that is not in the list", "aggregate", map[string]any{"database": "helpdesk", "table": "t", "function": "median"}, refused("aggregate", `the argument function must be one of "count", "sum", "avg", "min" and "max", and the call passed "median"`)},
		{"update without where", "update_rows", map[string]any{"database": "helpdesk", "table": "t", "set": map[string]any{"a": 1}}, refused("update_rows", "it needs the argument where, and the call leaves it out")},
		{"update with an empty where", "update_rows", map[string]any{"database": "helpdesk", "table": "t", "set": map[string]any{"a": 1}, "where": []any{}}, refused("update_rows", "the argument where must hold at least 1 entry, and the call passed none")},
		{"update with an empty set", "update_rows", map[string]any{"database": "helpdesk", "table": "t", "set": map[string]any{}, "where": where("=", 1)}, refused("update_rows", "the argument set must name at least 1 column, and the call passed none")},
		{"delete without where", "delete_rows", map[string]any{"database": "helpdesk", "table": "t"}, refused("delete_rows", "it needs the argument where, and the call leaves it out")},
		{"delete with an empty where", "delete_rows", map[string]any{"database": "helpdesk", "table": "t", "where": []any{}}, refused("delete_rows", "the argument where must hold at least 1 entry, and the call passed none")},
		{"insert without rows", "insert_rows", map[string]any{"database": "helpdesk", "table": "t", "rows": []any{}}, refused("insert_rows", "the argument rows must hold at least 1 entry, and the call passed none")},
		{"insert of an empty row", "insert_rows", map[string]any{"database": "helpdesk", "table": "t", "rows": []any{map[string]any{"a": 1}, map[string]any{}}}, refused("insert_rows", "the argument rows[1] must name at least 1 column, and the call passed none")},
		{"insert with an empty returning", "insert_rows", map[string]any{"database": "helpdesk", "table": "t", "rows": []any{map[string]any{"a": 1}}, "returning": []any{}}, refused("insert_rows", "the argument returning must hold at least 1 entry, and the call passed none")},
		{"empty sql", "query_sql", map[string]any{"database": "helpdesk", "sql": ""}, refused("query_sql", "the argument sql must not be empty")},
		{"sql that is a list", "execute_sql", map[string]any{"database": "helpdesk", "sql": []any{"SELECT 1", "SELECT 2"}}, refused("execute_sql", "the argument sql must be a string, and the call passed a list")},
		{"a cursor that is not base64url", "list_schemas", map[string]any{"database": "helpdesk", "cursor": "!!"}, badCursor("list_schemas")},
		{"an empty cursor", "list_tables", map[string]any{"database": "helpdesk", "cursor": ""}, badCursor("list_tables")},
		{"a cursor of list_schemas in list_tables", "list_tables", map[string]any{"database": "helpdesk", "cursor": schemasCursor}, badCursor("list_tables")},
		{"a cursor of another database", "list_schemas", map[string]any{"database": "sales_eu", "cursor": schemasCursor}, badCursor("list_schemas")},
		{"a cursor of another schema filter", "list_tables", map[string]any{"database": "helpdesk", "schema": "audit", "cursor": tablesCursor}, badCursor("list_tables")},
		{"a cursor without its schema filter", "list_tables", map[string]any{"database": "helpdesk", "cursor": tablesCursor}, badCursor("list_tables")},
		{"a cursor with its schema filter passes", "list_tables", map[string]any{"database": "helpdesk", "schema": "public", "cursor": tablesCursor}, notStarted("list_tables", "helpdesk")},
	}
	s := New(dataConfig(config.Straza, config.FreeSQLReadWrite), slog.New(slog.DiscardHandler))
	for _, v := range mcp.SupportedProtocolVersions() {
		cs := overMemory(t, s, gatewayCaller, v)
		for _, c := range cases {
			t.Run(v+"/"+c.name, func(t *testing.T) {
				res := callTool(t, cs, c.tool, c.args, nil)
				if !res.IsError {
					t.Fatalf("%s answered without isError: %v", c.tool, res.StructuredContent)
				}
				if got := textOf(t, res); got != c.want {
					t.Errorf("the refusal:\n got %s\nwant %s", got, c.want)
				}
			})
		}
	}
}
