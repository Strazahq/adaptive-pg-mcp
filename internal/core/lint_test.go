package core

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// lintTools fails t for every tool that breaks the tool rules in CLAUDE.md. It
// reads each tool as JSON, the way a client sees it in tools/list.
func lintTools(t *testing.T, tools []*mcp.Tool) {
	t.Helper()
	seen := map[string]bool{}
	for _, tool := range tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Errorf("tool %s does not marshal: %v", tool.Name, err)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("tool %s does not unmarshal: %v", tool.Name, err)
			continue
		}
		name, _ := m["name"].(string)
		if len(name) == 0 || len(name) > maxName || !nameRule.MatchString(name) {
			t.Errorf("tool %q: the name is not 1 to 128 characters of snake_case", name)
		}
		if seen[name] {
			t.Errorf("tool %s: the name is listed twice", name)
		}
		seen[name] = true
		if d, _ := m["description"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("tool %s: no description", name)
		}
		in, _ := m["inputSchema"].(map[string]any)
		lintInput(t, name, in)
		if out, _ := m["outputSchema"].(map[string]any); out == nil || out["type"] != "object" {
			t.Errorf("tool %s: no output schema of type object", name)
		}
		ann, _ := m["annotations"].(map[string]any)
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := ann[hint].(bool); !ok {
				t.Errorf("tool %s: annotation %s is not set", name, hint)
			}
		}
		meta, _ := m["_meta"].(map[string]any)
		if name == listDatabasesName {
			lintListing(t, name, meta, ann, in)
			continue
		}
		mark, _ := meta["ai.straza/object"].(map[string]any)
		if mark["type"] != "database" || mark["argument"] != databaseArg {
			t.Errorf("tool %s: the mark ai.straza/object is %v, want type database and argument database", name, mark)
		}
		props, _ := in["properties"].(map[string]any)
		arg, _ := props[databaseArg].(map[string]any)
		if arg["type"] != "string" || !slices.Contains(required(in), databaseArg) {
			t.Errorf("tool %s: the database argument is not a required string", name)
		}
	}
}

// lintListing fails t when list_databases breaks docs/DESIGN.md section 4: it
// takes an optional cursor and no database, its mark reaches all databases,
// and it carries the listing mark and readOnlyHint, as Straza's listing needs.
func lintListing(t *testing.T, name string, meta, ann, in map[string]any) {
	t.Helper()
	mark, _ := meta["ai.straza/object"].(map[string]any)
	if len(mark) != 2 || mark["type"] != "database" || mark["reach"] != "all" {
		t.Errorf("tool %s: the mark ai.straza/object is %v, want type database and reach all", name, mark)
	}
	lists, _ := meta["ai.straza/lists-objects"].(map[string]any)
	if len(lists) != 1 || lists["type"] != "database" {
		t.Errorf("tool %s: the mark ai.straza/lists-objects is %v, want type database", name, lists)
	}
	if ann["readOnlyHint"] != true {
		t.Errorf("tool %s: the listing tool does not set readOnlyHint", name)
	}
	props, _ := in["properties"].(map[string]any)
	if _, ok := props[databaseArg]; ok {
		t.Errorf("tool %s: the listing tool takes a database argument", name)
	}
	cursor, _ := props["cursor"].(map[string]any)
	if cursor["type"] != "string" || slices.Contains(required(in), "cursor") {
		t.Errorf("tool %s: cursor is not an optional string", name)
	}
}

// lintInput fails t when an input schema is not a closed object whose
// properties are all described and whose required names are all properties.
func lintInput(t *testing.T, name string, in map[string]any) {
	t.Helper()
	if in == nil || in["type"] != "object" {
		t.Errorf("tool %s: the input schema is not of type object", name)
		return
	}
	if in["additionalProperties"] != false {
		t.Errorf("tool %s: the input schema does not set additionalProperties to false", name)
	}
	if _, ok := in["required"].([]any); !ok {
		t.Errorf("tool %s: the input schema does not list required", name)
	}
	props, _ := in["properties"].(map[string]any)
	for prop, v := range props {
		p, _ := v.(map[string]any)
		if d, _ := p["description"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("tool %s: property %s has no description", name, prop)
		}
	}
	for _, r := range required(in) {
		if _, ok := props[r]; !ok {
			t.Errorf("tool %s: required %s is not a property", name, r)
		}
	}
	raw, _ := json.Marshal(in)
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Errorf("tool %s: the input schema does not parse: %v", name, err)
		return
	}
	if _, err := s.Resolve(nil); err != nil {
		t.Errorf("tool %s: the input schema does not resolve: %v", name, err)
	}
}

// required returns the required names of a schema read as JSON.
func required(schema map[string]any) []string {
	list, _ := schema["required"].([]any)
	var names []string
	for _, r := range list {
		if s, ok := r.(string); ok {
			names = append(names, s)
		}
	}
	return names
}
