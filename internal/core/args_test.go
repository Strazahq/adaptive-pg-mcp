package core

import (
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

func TestBadArgumentsAreToolErrors(t *testing.T) {
	refused := func(tool, why string) string {
		return tool + ": " + why + "."
	}
	badCursor := func(tool string) string {
		return "the cursor of " + tool + " is not from this listing. Pass the nextCursor of the previous page unchanged, or leave cursor out for the first page."
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
		{"no arguments", "read_rows", nil, refused("read_rows", "the argument database is missing")},
		{"a missing table", "read_rows", map[string]any{"database": "helpdesk"}, refused("read_rows", "the argument table is missing")},
		{"an unknown argument", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "tabel": "t"}, refused("read_rows", `there is no argument "tabel"`)},
		{"a database the config does not have", "read_rows", map[string]any{"database": "finance", "table": "t"}, refused("read_rows", `the argument database must be one of "helpdesk" and "sales_eu", not "finance"`)},
		{"a database that is a number", "list_tables", map[string]any{"database": 7}, refused("list_tables", "the argument database must be a string, not an integer")},
		{"an empty table", "describe_table", map[string]any{"database": "helpdesk", "table": ""}, refused("describe_table", "the argument table must not be empty")},
		{"a limit that is a string", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "limit": "5"}, refused("read_rows", "the argument limit must be an integer, not a string")},
		{"a limit with a fraction", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "limit": 1.5}, refused("read_rows", "the argument limit must be an integer, not a number")},
		{"a limit of 0", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "limit": 0}, refused("read_rows", "the argument limit must be at least 1, not 0")},
		{"a limit past the row cap", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "limit": 5000}, refused("read_rows", "the argument limit must be at most 1,000, not 5000")},
		{"an empty columns list", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "columns": []any{}}, refused("read_rows", "the argument columns must hold at least 1 entry")},
		{"an op that is not in the list", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("==", 1)}, refused("read_rows", `the argument where[0].op must be one of "=", "<>", "<", "<=", ">", ">=", "in", "not in", "is null", "is not null", "like" and "ilike", not "=="`)},
		{"a where entry without op", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": []any{map[string]any{"column": "id"}}}, refused("read_rows", "the argument where[0] is missing op")},
		{"a where entry with another key", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": []any{map[string]any{"column": "id", "op": "=", "val": 1}}}, refused("read_rows", `the argument where[0] takes no key "val"`)},
		{"a where entry that is a string", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": []any{"id = 1"}}, refused("read_rows", "the argument where[0] must be an object, not a string")},
		{"an op = without a value", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("=", nil)}, refused("read_rows", "where[0] has op =, which takes a value that is not null. Use op is null to match null")},
		{"an op = with null", "update_rows", map[string]any{"database": "helpdesk", "table": "t", "set": map[string]any{"a": 1}, "where": []any{map[string]any{"column": "id", "op": "=", "value": nil}}}, refused("update_rows", "where[0] has op =, which takes a value that is not null. Use op is null to match null")},
		{"an op in with one value", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("in", 1)}, refused("read_rows", "where[0] has op in, which takes a list of values, and the call passed an integer")},
		{"an op in with an empty list", "delete_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("in", []any{})}, refused("delete_rows", "where[0] has op in, which takes 1 to 1,000 values, not 0")},
		{"an op is null with a value", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "where": where("is null", 1)}, refused("read_rows", "where[0] has op is null, which takes no value")},
		{"a direction that is not in the list", "read_rows", map[string]any{"database": "helpdesk", "table": "t", "order_by": []any{map[string]any{"column": "id", "direction": "up"}}}, refused("read_rows", `the argument order_by[0].direction must be one of "asc" and "desc", not "up"`)},
		{"sum without a column", "aggregate", map[string]any{"database": "helpdesk", "table": "t", "function": "sum"}, refused("aggregate", "the function sum needs the argument column")},
		{"a function that is not in the list", "aggregate", map[string]any{"database": "helpdesk", "table": "t", "function": "median"}, refused("aggregate", `the argument function must be one of "count", "sum", "avg", "min" and "max", not "median"`)},
		{"update without where", "update_rows", map[string]any{"database": "helpdesk", "table": "t", "set": map[string]any{"a": 1}}, refused("update_rows", "the argument where is missing")},
		{"update with an empty where", "update_rows", map[string]any{"database": "helpdesk", "table": "t", "set": map[string]any{"a": 1}, "where": []any{}}, refused("update_rows", "the argument where must hold at least 1 entry")},
		{"update with an empty set", "update_rows", map[string]any{"database": "helpdesk", "table": "t", "set": map[string]any{}, "where": where("=", 1)}, refused("update_rows", "the argument set must name at least 1 column")},
		{"delete without where", "delete_rows", map[string]any{"database": "helpdesk", "table": "t"}, refused("delete_rows", "the argument where is missing")},
		{"delete with an empty where", "delete_rows", map[string]any{"database": "helpdesk", "table": "t", "where": []any{}}, refused("delete_rows", "the argument where must hold at least 1 entry")},
		{"insert without rows", "insert_rows", map[string]any{"database": "helpdesk", "table": "t", "rows": []any{}}, refused("insert_rows", "the argument rows must hold at least 1 entry")},
		{"insert of an empty row", "insert_rows", map[string]any{"database": "helpdesk", "table": "t", "rows": []any{map[string]any{"a": 1}, map[string]any{}}}, refused("insert_rows", "the argument rows[1] must name at least 1 column")},
		{"insert with an empty returning", "insert_rows", map[string]any{"database": "helpdesk", "table": "t", "rows": []any{map[string]any{"a": 1}}, "returning": []any{}}, refused("insert_rows", "the argument returning must hold at least 1 entry")},
		{"empty sql", "query_sql", map[string]any{"database": "helpdesk", "sql": ""}, refused("query_sql", "the argument sql must not be empty")},
		{"sql that is a list", "execute_sql", map[string]any{"database": "helpdesk", "sql": []any{"SELECT 1", "SELECT 2"}}, refused("execute_sql", "the argument sql must be a string, not a list")},
		{"a cursor that is not base64url", "list_schemas", map[string]any{"database": "helpdesk", "cursor": "!!"}, badCursor("list_schemas")},
		{"an empty cursor", "list_tables", map[string]any{"database": "helpdesk", "cursor": ""}, badCursor("list_tables")},
		{"a cursor of list_schemas in list_tables", "list_tables", map[string]any{"database": "helpdesk", "cursor": schemasCursor}, badCursor("list_tables")},
		{"a cursor of another database", "list_schemas", map[string]any{"database": "sales_eu", "cursor": schemasCursor}, badCursor("list_schemas")},
		{"a cursor of another schema filter", "list_tables", map[string]any{"database": "helpdesk", "schema": "audit", "cursor": tablesCursor}, badCursor("list_tables")},
		{"a cursor without its schema filter", "list_tables", map[string]any{"database": "helpdesk", "cursor": tablesCursor}, badCursor("list_tables")},
		{"a cursor with its schema filter passes", "list_tables", map[string]any{"database": "helpdesk", "schema": "public", "cursor": tablesCursor}, notStarted("list_tables", "helpdesk")},
		{"a catalog call without its required argument", "tickets_by_day", map[string]any{"database": "helpdesk"}, refused("tickets_by_day", "the argument day is missing")},
		{"a catalog argument of the wrong type", "tickets_by_day", map[string]any{"database": "helpdesk", "day": 20261001}, refused("tickets_by_day", "the argument day must be a string, not an integer")},
		{"a catalog argument longer than its maxLength", "tickets_by_day", map[string]any{"database": "helpdesk", "day": "2026-10-01", "queue": "operations"}, refused("tickets_by_day", "the argument queue must be at most 8 characters long, not 10")},
		{"a catalog argument that does not match its pattern", "tickets_by_day", map[string]any{"database": "helpdesk", "day": "2026-10-01", "queue": "Ops"}, refused("tickets_by_day", "the argument queue must match the pattern ^[a-z]+$")},
		{"a catalog argument the tool does not take", "tickets_by_day", map[string]any{"database": "helpdesk", "day": "2026-10-01", "region": "north"}, refused("tickets_by_day", `there is no argument "region"`)},
		{"a catalog call on a database outside its entry", "revenue_eu", map[string]any{"database": "helpdesk", "month": "2026-09-01"}, refused("revenue_eu", `the argument database must be one of "sales_eu", not "helpdesk"`)},
		{"a catalog number below a fractional minimum", "revenue_eu", map[string]any{"database": "sales_eu", "month": "2026-09-01", "min_share": 0.25}, refused("revenue_eu", "the argument min_share must be at least 0.5, not 0.25")},
		{"a catalog number above a fractional maximum", "revenue_eu", map[string]any{"database": "sales_eu", "month": "2026-09-01", "min_share": 99.75}, refused("revenue_eu", "the argument min_share must be at most 99.5, not 99.75")},
		{"a catalog call that passes", "tickets_by_day", map[string]any{"database": "helpdesk", "day": "2026-10-01", "queue": "ops"}, notStarted("tickets_by_day", "helpdesk")},
	}
	s := withCatalogTools(t, New(dataConfig(config.Straza, config.FreeSQLReadWrite), slog.New(slog.DiscardHandler)))
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
