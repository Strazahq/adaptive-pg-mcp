package core

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
)

// revenue is a valid read entry that each case changes one field of.
func revenue() catalog.Entry {
	return catalog.Entry{
		Name:        "revenue_by_region",
		Description: "Sums net revenue per region for one month. Answers one row per region, at most the row cap.",
		Mode:        catalog.Read,
		Databases:   []string{"sales_eu", "sales_us"},
		Login:       "read",
		SQL:         "select region, sum(net) from revenue where month = $1 and ($2::text is null or region = $2) group by region",
		Params: []catalog.Param{
			{Name: "month", Description: "The first day of the month, such as 2026-09-01.", Type: "date", Required: true},
			{Name: "region", Description: "One region, or none for all.", Type: "text", Enum: []string{"north", "south"}},
		},
		Source: "finance/revenue.sql",
	}
}

func ptr[T any](v T) *T { return &v }

func TestToolBuildsLintCleanTools(t *testing.T) {
	cases := []struct {
		name   string
		change func(*catalog.Entry)
	}{
		{"read with params", func(e *catalog.Entry) {}},
		{"write without params", func(e *catalog.Entry) {
			e.Name, e.Mode, e.Params = "close_stale_tickets", catalog.Write, nil
		}},
		{"number bounds", func(e *catalog.Entry) {
			e.Params = []catalog.Param{{Name: "min_net", Description: "The smallest net amount.", Type: "numeric", Minimum: ptr(0.0), Maximum: ptr(1e9)}}
		}},
		{"string limits", func(e *catalog.Entry) {
			e.Params = []catalog.Param{{Name: "code", Description: "A region code.", Type: "varchar", Pattern: "^[A-Z]{2}$", MaxLength: ptr(2)}}
		}},
		{"json value", func(e *catalog.Entry) {
			e.Params = []catalog.Param{{Name: "filter", Description: "A JSON filter document.", Type: "jsonb", Required: true}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := revenue()
			c.change(&e)
			tool, err := Tool(e)
			if err != nil {
				t.Fatalf("Tool: %v", err)
			}
			lintTools(t, []*mcp.Tool{tool})
		})
	}
}

func TestToolHintsAndOutput(t *testing.T) {
	cases := []struct {
		mode                              catalog.Mode
		readOnly, destructive, idempotent bool
		output                            []string
	}{
		{catalog.Read, true, false, true, []string{"columns", "rows", "truncated"}},
		{catalog.Write, false, true, false, []string{"rowsAffected", "columns", "rows"}},
	}
	for _, c := range cases {
		e := revenue()
		e.Mode = c.mode
		tool, err := Tool(e)
		if err != nil {
			t.Fatalf("mode %d: Tool: %v", c.mode, err)
		}
		a := tool.Annotations
		if a.ReadOnlyHint != c.readOnly || *a.DestructiveHint != c.destructive || a.IdempotentHint != c.idempotent || *a.OpenWorldHint {
			t.Errorf("mode %d: hints readOnly %v destructive %v idempotent %v openWorld %v", c.mode, a.ReadOnlyHint, *a.DestructiveHint, a.IdempotentHint, *a.OpenWorldHint)
		}
		if got := tool.OutputSchema.(*jsonschema.Schema).Required; !slices.Equal(got, c.output) {
			t.Errorf("mode %d: output requires %v, want %v", c.mode, got, c.output)
		}
	}
}

func TestToolInputSchema(t *testing.T) {
	tool, err := Tool(revenue())
	if err != nil {
		t.Fatalf("Tool: %v", err)
	}
	in := tool.InputSchema.(*jsonschema.Schema)
	if got := in.Properties["database"].Enum; !slices.Equal(got, []any{"sales_eu", "sales_us"}) {
		t.Errorf("database enum %v, want the entry's databases", got)
	}
	if !slices.Equal(in.Required, []string{"database", "month"}) {
		t.Errorf("required %v, want database and month", in.Required)
	}
	if m := in.Properties["month"]; m.Type != "string" || m.Format != "date" {
		t.Errorf("month is %s with format %q, want string with format date", m.Type, m.Format)
	}
	raw, _ := json.Marshal(in)
	s := string(raw)
	if i, j, k := strings.Index(s, `"database":{`), strings.Index(s, `"month":{`), strings.Index(s, `"region":{`); i < 0 || i > j || j > k {
		t.Errorf("properties are not in database, $1, $2 order: %s", s)
	}
}

func TestToolRefusesBadEntries(t *testing.T) {
	const at = "finance/revenue.sql statement revenue_by_region"
	param := func(e *catalog.Entry) *catalog.Param { return &e.Params[1] }
	cases := []struct {
		name   string
		change func(*catalog.Entry)
		want   string
	}{
		{"name with capitals", func(e *catalog.Entry) { e.Name = "Revenue_By_Region" },
			"finance/revenue.sql statement Revenue_By_Region does not have a valid tool name. A tool name is 1 to 128 lowercase letters, digits and underscores that starts with a letter, so rename it in its -- name: line."},
		{"name too long", func(e *catalog.Entry) { e.Name = strings.Repeat("a", 129) },
			"finance/revenue.sql statement " + strings.Repeat("a", 129) + " does not have a valid tool name. A tool name is 1 to 128 lowercase letters, digits and underscores that starts with a letter, so rename it in its -- name: line."},
		{"no description", func(e *catalog.Entry) { e.Description = "  " },
			at + " has no description in its index. Write one that says what the tool does, what it returns and its limits."},
		{"no mode", func(e *catalog.Entry) { e.Mode = 0 },
			at + " has no mode of read or write in its index. Set mode to read or write."},
		{"no databases", func(e *catalog.Entry) { e.Databases = nil },
			at + " names no databases in its index. List the databases it runs on."},
		{"database twice", func(e *catalog.Entry) { e.Databases = []string{"sales_eu", "sales_eu"} },
			at + " lists the database sales_eu twice in its index. List each database once."},
		{"parameter name", func(e *catalog.Entry) { param(e).Name = "Region" },
			at + ` has parameter $2 named "Region", which is not a valid argument name. Use 1 to 128 lowercase letters, digits and underscores that start with a letter.`},
		{"parameter named database", func(e *catalog.Entry) { param(e).Name = "database" },
			at + " has parameter $2 named database, which every tool already uses for the database to run on. Rename the parameter in its index."},
		{"parameter twice", func(e *catalog.Entry) { param(e).Name = "month" },
			at + " has more than one parameter named month. Give each parameter its own name in the index."},
		{"parameter description", func(e *catalog.Entry) { param(e).Description = "" },
			at + " has no description for parameter $2 region in its index. Describe what the agent passes in it."},
		{"parameter type", func(e *catalog.Entry) { param(e).Type = "tsvector" },
			at + ` has parameter $2 region of Postgres type "tsvector", which a tool argument cannot carry. Cast it in the statement to one of bool, bpchar, date, float4, float8, int2, int4, int8, json, jsonb, numeric, text, timestamp, timestamptz, uuid, varchar, such as $2::text.`},
		{"enum on a number", func(e *catalog.Entry) { param(e).Type = "int4" },
			at + " sets enum on parameter $2 region of type int4, where it does not apply. Use enum, pattern and maxLength on string types such as text, and minimum and maximum on number types such as int4."},
		{"minimum on text", func(e *catalog.Entry) { param(e).Enum, param(e).Minimum = nil, ptr(1.0) },
			at + " sets minimum on parameter $2 region of type text, where it does not apply. Use enum, pattern and maxLength on string types such as text, and minimum and maximum on number types such as int4."},
		{"negative maxLength", func(e *catalog.Entry) { param(e).MaxLength = ptr(-1) },
			at + " sets maxLength -1 on parameter $2 region. Set it to 0 or more."},
		{"pattern does not compile", func(e *catalog.Entry) { param(e).Pattern = "(north" },
			at + " has a pattern for parameter $2 region that does not compile: error parsing regexp: missing closing ): `(north`. Fix the pattern in its index."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := revenue()
			c.change(&e)
			tool, err := Tool(e)
			if err == nil {
				t.Fatalf("Tool built %s, want the refusal: %s", tool.Name, c.want)
			}
			if err.Error() != c.want {
				t.Errorf("refusal:\n got %s\nwant %s", err, c.want)
			}
		})
	}
}
