// Package core turns catalog entries into MCP tools.
package core

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
)

// nameRule is the snake_case form of every tool name and argument name.
var nameRule = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// maxName is the longest tool name or argument name.
const maxName = 128

// databaseArg is the argument every data tool names its database in.
const databaseArg = "database"

// argType is the JSON Schema type and format an agent sees for a Postgres type.
// An empty jsonType takes any JSON value.
type argType struct {
	jsonType string
	format   string
}

// argTypes maps each Postgres type a catalog parameter may have to its JSON
// Schema form. Timestamp has no format, because date-time carries an offset
// that a timestamp without time zone drops.
var argTypes = map[string]argType{
	"bool":        {jsonType: "boolean"},
	"int2":        {jsonType: "integer"},
	"int4":        {jsonType: "integer"},
	"int8":        {jsonType: "integer"},
	"float4":      {jsonType: "number"},
	"float8":      {jsonType: "number"},
	"numeric":     {jsonType: "number"},
	"text":        {jsonType: "string"},
	"varchar":     {jsonType: "string"},
	"bpchar":      {jsonType: "string"},
	"uuid":        {jsonType: "string", format: "uuid"},
	"date":        {jsonType: "string", format: "date"},
	"timestamp":   {jsonType: "string"},
	"timestamptz": {jsonType: "string", format: "date-time"},
	"json":        {},
	"jsonb":       {},
}

// Tool builds the MCP tool for a catalog entry, or returns a sentence that
// says why the entry cannot be a tool and what to fix.
func Tool(e catalog.Entry) (*mcp.Tool, error) {
	at := e.Source + " statement " + e.Name
	if len(e.Name) > maxName || !nameRule.MatchString(e.Name) {
		return nil, fmt.Errorf("%s does not have a valid tool name. A tool name is 1 to 128 lowercase letters, digits and underscores that starts with a letter, so rename it in its -- name: line.", at)
	}
	if strings.TrimSpace(e.Description) == "" {
		return nil, fmt.Errorf("%s has no description in its index. Write one that says what the tool does, what it returns and its limits.", at)
	}
	if e.Mode != catalog.Read && e.Mode != catalog.Write {
		return nil, fmt.Errorf("%s has no mode of read or write in its index. Set mode to read or write.", at)
	}
	if len(e.Databases) == 0 {
		return nil, fmt.Errorf("%s names no databases in its index. List the databases it runs on.", at)
	}
	for i, db := range e.Databases {
		if slices.Contains(e.Databases[:i], db) {
			return nil, fmt.Errorf("%s lists the database %s twice in its index. List each database once.", at, db)
		}
	}

	in := &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{databaseArg: databaseSchema(e.Databases)},
		Required:             []string{databaseArg},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
		PropertyOrder:        []string{databaseArg},
	}
	for i, p := range e.Params {
		s, err := paramSchema(at, i+1, p)
		if err != nil {
			return nil, err
		}
		if _, dup := in.Properties[p.Name]; dup {
			return nil, fmt.Errorf("%s has more than one parameter named %s. Give each parameter its own name in the index.", at, p.Name)
		}
		in.Properties[p.Name] = s
		in.PropertyOrder = append(in.PropertyOrder, p.Name)
		if p.Required {
			in.Required = append(in.Required, p.Name)
		}
	}

	return &mcp.Tool{
		Name:         e.Name,
		Description:  strings.TrimSpace(e.Description),
		InputSchema:  in,
		OutputSchema: outputSchema(e.Mode),
		Annotations:  annotations(e.Mode),
		Meta:         mcp.Meta{"ai.straza/object": map[string]any{"type": "database", "argument": databaseArg}},
	}, nil
}

// databaseSchema is the required database argument, limited to the entry's databases.
func databaseSchema(names []string) *jsonschema.Schema {
	enum := make([]any, len(names))
	for i, n := range names {
		enum[i] = n
	}
	return &jsonschema.Schema{Type: "string", Description: "The database to run on, by its config name.", Enum: enum}
}

// paramSchema builds the argument schema for parameter $n, or says why the
// parameter cannot be an argument.
func paramSchema(at string, n int, p catalog.Param) (*jsonschema.Schema, error) {
	if len(p.Name) > maxName || !nameRule.MatchString(p.Name) {
		return nil, fmt.Errorf("%s has parameter $%d named %q, which is not a valid argument name. Use 1 to 128 lowercase letters, digits and underscores that start with a letter.", at, n, p.Name)
	}
	if p.Name == databaseArg {
		return nil, fmt.Errorf("%s has parameter $%d named database, which every tool already uses for the database to run on. Rename the parameter in its index.", at, n)
	}
	if strings.TrimSpace(p.Description) == "" {
		return nil, fmt.Errorf("%s has no description for parameter $%d %s in its index. Describe what the agent passes in it.", at, n, p.Name)
	}
	t, ok := argTypes[p.Type]
	if !ok {
		return nil, fmt.Errorf("%s has parameter $%d %s of Postgres type %q, which a tool argument cannot carry. Cast it in the statement to one of %s, such as $%d::text.", at, n, p.Name, p.Type, strings.Join(slices.Sorted(maps.Keys(argTypes)), ", "), n)
	}
	misplaced := func(constraint string) error {
		return fmt.Errorf("%s sets %s on parameter $%d %s of type %s, where it does not apply. Use enum, pattern and maxLength on string types such as text, and minimum and maximum on number types such as int4.", at, constraint, n, p.Name, p.Type)
	}
	text, number := t.jsonType == "string", t.jsonType == "integer" || t.jsonType == "number"
	switch {
	case !text && len(p.Enum) > 0:
		return nil, misplaced("enum")
	case !text && p.Pattern != "":
		return nil, misplaced("pattern")
	case !text && p.MaxLength != nil:
		return nil, misplaced("maxLength")
	case !number && p.Minimum != nil:
		return nil, misplaced("minimum")
	case !number && p.Maximum != nil:
		return nil, misplaced("maximum")
	}
	if p.MaxLength != nil && *p.MaxLength < 0 {
		return nil, fmt.Errorf("%s sets maxLength %d on parameter $%d %s. Set it to 0 or more.", at, *p.MaxLength, n, p.Name)
	}
	if p.Pattern != "" {
		if _, err := regexp.Compile(p.Pattern); err != nil {
			return nil, fmt.Errorf("%s has a pattern for parameter $%d %s that does not compile: %v. Fix the pattern in its index.", at, n, p.Name, err)
		}
	}

	s := &jsonschema.Schema{
		Type:        t.jsonType,
		Format:      t.format,
		Description: strings.TrimSpace(p.Description),
		Pattern:     p.Pattern,
		Minimum:     p.Minimum,
		Maximum:     p.Maximum,
		MaxLength:   p.MaxLength,
	}
	for _, v := range p.Enum {
		s.Enum = append(s.Enum, v)
	}
	return s, nil
}

// annotations are the hints of a catalog tool. A read changes nothing and
// repeats safely, a write may change rows, and both stay inside the database.
func annotations(m catalog.Mode) *mcp.ToolAnnotations {
	read := m == catalog.Read
	destructive, openWorld := !read, false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    read,
		DestructiveHint: &destructive,
		IdempotentHint:  read,
		OpenWorldHint:   &openWorld,
	}
}

// outputSchema is the shape of every answer of a tool in mode m. Rows are
// arrays in column order, because Postgres allows two columns with one name.
func outputSchema(m catalog.Mode) *jsonschema.Schema {
	columns := &jsonschema.Schema{
		Type:        "array",
		Description: "The result columns in order, each with its name and Postgres type.",
		Items: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": {Type: "string", Description: "The column name."},
				"type": {Type: "string", Description: "The Postgres type name, such as int4."},
			},
			Required:      []string{"name", "type"},
			PropertyOrder: []string{"name", "type"},
		},
	}
	rows := &jsonschema.Schema{
		Type:        "array",
		Description: "The rows the statement answers, each an array of values in column order.",
		Items:       &jsonschema.Schema{Type: "array"},
	}
	out := &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"columns": columns, "rows": rows},
	}
	if m == catalog.Write {
		out.Properties["rowsAffected"] = &jsonschema.Schema{Type: "integer", Description: "The number of rows the statement changed."}
		out.PropertyOrder = []string{"rowsAffected", "columns", "rows"}
	} else {
		out.Properties["truncated"] = &jsonschema.Schema{Type: "boolean", Description: "True when the answer stopped at the row or byte cap."}
		out.PropertyOrder = []string{"columns", "rows", "truncated"}
	}
	out.Required = out.PropertyOrder
	return out
}
