package core

import (
	"maps"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
)

// closed is the additionalProperties of an object that takes no key beyond its properties.
func closed() *jsonschema.Schema { return &jsonschema.Schema{Not: &jsonschema.Schema{}} }

// atLeast returns a pointer to n, for the minimum keywords of a schema.
func atLeast(n int) *int { return &n }

// identifier is a schema for a non-empty identifier, such as a column name.
func identifier(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", MinLength: atLeast(1), Description: description}
}

// identifiers is a schema for a list of identifiers with at least min entries.
func identifiers(description, item string, min int) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: identifier(item), MinItems: atLeast(min)}
}

// databaseProp is the required database argument of every data tool, limited to the configured databases.
func (s *Server) databaseProp() *jsonschema.Schema {
	var enum []any
	for _, n := range slices.Sorted(maps.Keys(s.cfg.Databases)) {
		enum = append(enum, n)
	}
	return &jsonschema.Schema{Type: "string", Description: "The database to run on, by its config name, as list_databases answers it.", Enum: enum}
}

// schemaProp is the optional schema of the table a tool names.
func schemaProp() *jsonschema.Schema {
	return identifier("The schema of the table, by its exact name. Leave it out for public.")
}

// tableProp is the required table a tool names.
func tableProp(what string) *jsonschema.Schema {
	return identifier("The table " + what + ", by its exact name in the schema. A view works where Postgres allows it.")
}

// whereOps are the ops of a where entry, in the order docs/DESIGN.md section 4 lists them.
var whereOps = []string{"=", "<>", "<", "<=", ">", ">=", "in", "not in", "is null", "is not null", "like", "ilike"}

// whereProp is a where filter: a list of {column, op, value}, all of which a row must match.
func whereProp(min int, description string) *jsonschema.Schema {
	ops := make([]any, len(whereOps))
	for i, op := range whereOps {
		ops[i] = op
	}
	entry := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"column": identifier("The column to compare."),
			"op":     {Type: "string", Description: "How to compare: =, <>, <, <=, >, >=, in, not in, is null, is not null, like or ilike.", Enum: ops},
			"value":  {Description: "The value to compare with, which travels as a bind parameter. in and not in take a list, and is null and is not null take none."},
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

// returningProp is the optional list of columns a write answers from each row it changed.
func returningProp() *jsonschema.Schema {
	return identifiers("The columns to answer from each row the call changed. Leave it out to answer no rows.", "A column name.", 1)
}

// cursorProp is the optional cursor of a listing.
func cursorProp(tool string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: "The nextCursor of the previous page of " + tool + ", exactly as it came. Leave it out for the first page."}
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

// dataMeta is the Straza mark of every data tool: it reaches the database its database argument names.
func dataMeta() mcp.Meta {
	return mcp.Meta{"ai.straza/object": map[string]any{"type": "database", "argument": databaseArg}}
}

// readTool is a read tool with the read hints, the read output shape and the Straza mark.
func readTool(name, description string, in *jsonschema.Schema) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: in, OutputSchema: outputSchema(catalog.Read), Annotations: annotations(catalog.Read), Meta: dataMeta()}
}

// writeTool is a write tool with the write hints, the write output shape and the Straza mark.
func writeTool(name, description string, in *jsonschema.Schema) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: in, OutputSchema: outputSchema(catalog.Write), Annotations: annotations(catalog.Write), Meta: dataMeta()}
}

// listingTool is a read tool that pages: its output adds nextCursor to the read shape.
func listingTool(name, description string, in *jsonschema.Schema) *mcp.Tool {
	t := readTool(name, description, in)
	out := t.OutputSchema.(*jsonschema.Schema)
	out.Properties["nextCursor"] = &jsonschema.Schema{Type: "string", Description: "Set when more follow. Pass it as cursor to read the next page."}
	out.PropertyOrder = append(slices.Clone(out.PropertyOrder), "nextCursor")
	return t
}
