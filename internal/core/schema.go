package core

import (
	"maps"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
)

// closed returns the additionalProperties schema that allows no key beyond the
// listed properties.
func closed() *jsonschema.Schema { return &jsonschema.Schema{Not: &jsonschema.Schema{}} }

// atLeast returns a pointer to n, for the minimum keywords of a schema.
func atLeast(n int) *int { return &n }

// identifier returns a schema for a non-empty identifier, such as a column name.
func identifier(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", MinLength: atLeast(1), Description: description}
}

// identifiers returns a schema for a list of at least min identifiers.
func identifiers(description, item string, min int) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: identifier(item), MinItems: atLeast(min)}
}

// databaseProp is the required database argument of a data tool, limited to
// the configured databases.
func (s *Server) databaseProp() *jsonschema.Schema {
	var enum []any
	for _, n := range slices.Sorted(maps.Keys(s.cfg.Databases)) {
		enum = append(enum, n)
	}
	return &jsonschema.Schema{Type: "string", Description: "The database to run on.", Enum: enum}
}

// schemaProp is the optional schema of the table a tool names.
func schemaProp() *jsonschema.Schema {
	return identifier("The schema, by exact name. Defaults to public.")
}

// tableProp is the required table a tool names.
func tableProp(what string) *jsonschema.Schema {
	return identifier("The table or view " + what + ", by exact name.")
}

// whereOps are the ops of a where entry, in the order docs/DESIGN.md section 4
// lists them.
var whereOps = []string{"=", "<>", "<", "<=", ">", ">=", "in", "not in", "is null", "is not null", "like", "ilike"}

// whereProp is a where filter: a list of {column, op, value} conditions joined
// by AND.
func whereProp(min int, description string) *jsonschema.Schema {
	ops := make([]any, len(whereOps))
	for i, op := range whereOps {
		ops[i] = op
	}
	entry := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"column": identifier("The column to compare."),
			"op":     {Type: "string", Description: "The operator.", Enum: ops},
			"value":  {Description: "The value, sent as a bind parameter. A list for in and not in, none for is null and is not null."},
		},
		Required:             []string{"column", "op"},
		AdditionalProperties: closed(),
		PropertyOrder:        []string{"column", "op", "value"},
	}
	list := &jsonschema.Schema{Type: "array", Description: description, Items: entry}
	if min > 0 {
		list.MinItems = atLeast(min)
	}
	return list
}

// returningProp is the optional list of columns a write returns from each row
// it changed.
func returningProp() *jsonschema.Schema {
	return identifiers("Columns to return from each changed row. Omit to return none.", "A column name.", 1)
}

// cursorProp is the optional cursor of a listing.
func cursorProp(tool string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: "The nextCursor of the previous " + tool + " page. Omit for the first page."}
}

// input is the input schema of a data tool: database first and required,
// then the other properties in order, and no key beyond them.
func (s *Server) input(props map[string]*jsonschema.Schema, order []string, required ...string) *jsonschema.Schema {
	props[databaseArg] = s.databaseProp()
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           props,
		Required:             append([]string{databaseArg}, required...),
		AdditionalProperties: closed(),
		PropertyOrder:        append([]string{databaseArg}, order...),
	}
}

// dataMeta is the Straza mark of every data tool: it reaches the database its
// database argument names.
func dataMeta() mcp.Meta {
	return mcp.Meta{"ai.straza/object": map[string]any{"type": "database", "argument": databaseArg}}
}

// readTool returns a read tool with the read hints, the read output schema and
// the Straza mark.
func readTool(name, description string, in *jsonschema.Schema) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: in, OutputSchema: outputSchema(catalog.Read), Annotations: annotations(catalog.Read), Meta: dataMeta()}
}

// writeTool returns a write tool with the write hints, the write output schema
// and the Straza mark.
func writeTool(name, description string, in *jsonschema.Schema) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: in, OutputSchema: outputSchema(catalog.Write), Annotations: annotations(catalog.Write), Meta: dataMeta()}
}

// listingTool returns a read tool that pages, whose output schema adds
// nextCursor.
func listingTool(name, description string, in *jsonschema.Schema) *mcp.Tool {
	t := readTool(name, description, in)
	out := t.OutputSchema.(*jsonschema.Schema)
	out.Properties["nextCursor"] = &jsonschema.Schema{Type: "string", Description: "Set when more follow. Pass it as cursor for the next page."}
	out.PropertyOrder = append(slices.Clone(out.PropertyOrder), "nextCursor")
	return t
}
