// Package core serves the built-in tools and the catalog tools of
// adaptive-pg-mcp over stdio and Streamable HTTP, and checks the token of
// every caller.
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

// maxName is the longest a tool name or argument name may be.
const maxName = 128

// databaseArg is the argument that names the database of a call.
const databaseArg = "database"

// argType is the JSON Schema type and format of a Postgres type. An empty
// jsonType takes any JSON value.
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

// Tool builds the MCP tool for a catalog statement, or returns an error that
// says what to fix.
func Tool(e catalog.Entry) (*mcp.Tool, error) {
	at := e.Source + " statement " + e.Name
	if len(e.Name) > maxName || !nameRule.MatchString(e.Name) {
		return nil, fmt.Errorf("%s has an invalid tool name. In its -- name: line, use snake_case of 1 to 128 characters that starts with a letter.", at)
	}
	if strings.TrimSpace(e.Description) == "" {
		return nil, fmt.Errorf("%s has no description in tools.yaml. Say what the tool does, what it returns and its caps.", at)
	}
	if e.Mode != catalog.Read && e.Mode != catalog.Write {
		return nil, fmt.Errorf("%s needs mode read or write in tools.yaml.", at)
	}
	if len(e.Databases) == 0 {
		return nil, fmt.Errorf("%s names no databases in tools.yaml. List the databases it runs on.", at)
	}
	for i, db := range e.Databases {
		if slices.Contains(e.Databases[:i], db) {
			return nil, fmt.Errorf("%s lists the database %s twice in tools.yaml. List it once.", at, db)
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
			return nil, fmt.Errorf("%s has more than one parameter named %s in tools.yaml. Give each parameter a unique name.", at, p.Name)
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

// databaseSchema is the required database argument, limited to names.
func databaseSchema(names []string) *jsonschema.Schema {
	enum := make([]any, len(names))
	for i, n := range names {
		enum[i] = n
	}
	return &jsonschema.Schema{Type: "string", Description: "The database to run on.", Enum: enum}
}

// paramSchema builds the argument schema for parameter $n, or returns an
// error that says what to fix.
func paramSchema(at string, n int, p catalog.Param) (*jsonschema.Schema, error) {
	if len(p.Name) > maxName || !nameRule.MatchString(p.Name) {
		return nil, fmt.Errorf("%s has parameter $%d named %q, an invalid argument name. Use snake_case of 1 to 128 characters that starts with a letter.", at, n, p.Name)
	}
	if p.Name == databaseArg {
		return nil, fmt.Errorf("%s has parameter $%d named database, which every tool reserves for the database to run on. Rename it in tools.yaml.", at, n)
	}
	if strings.TrimSpace(p.Description) == "" {
		return nil, fmt.Errorf("%s has no description for parameter $%d %s in tools.yaml. Say what the agent passes in it.", at, n, p.Name)
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
			return nil, fmt.Errorf("%s has a pattern for parameter $%d %s that does not compile: %v. Fix the pattern in tools.yaml.", at, n, p.Name, err)
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

// annotations returns the hints of a tool in mode m. A read is read-only and
// idempotent, a write is destructive, and neither reaches outside the
// database.
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

// outputSchema returns the output schema of a tool in mode m. Rows are arrays
// in column order, because a result may hold two columns with one name.
func outputSchema(m catalog.Mode) *jsonschema.Schema {
	columns := &jsonschema.Schema{
		Type:        "array",
		Description: "The result columns, in order.",
		Items: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": {Type: "string", Description: "The column name."},
				"type": {Type: "string", Description: "The Postgres type, such as int4."},
			},
			Required:      []string{"name", "type"},
			PropertyOrder: []string{"name", "type"},
		},
	}
	rows := &jsonschema.Schema{
		Type:        "array",
		Description: "The result rows, each an array in column order.",
		Items:       &jsonschema.Schema{Type: "array"},
	}
	out := &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"columns": columns, "rows": rows},
	}
	if m == catalog.Write {
		out.Properties["rowsAffected"] = &jsonschema.Schema{Type: "integer", Description: "The number of rows changed."}
		out.PropertyOrder = []string{"rowsAffected", "columns", "rows"}
	} else {
		out.Properties["truncated"] = &jsonschema.Schema{Type: "boolean", Description: "True when the row cap, the byte cap or limit cut off more rows."}
		out.PropertyOrder = []string{"columns", "rows", "truncated"}
	}
	out.Required = out.PropertyOrder
	return out
}
