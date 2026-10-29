package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// listDatabasesName is the name of the built-in tool that lists the databases.
const listDatabasesName = "list_databases"

// listPage is the page size of list_databases.
const listPage = 25

// listedDatabase is one database on a page of list_databases, in the shape
// that the Straza object marks reference.
type listedDatabase struct {
	// Value is the config name, which every other tool takes in its database
	// argument.
	Value string `json:"value"`
	// ID is the database's id, read on its read login.
	ID string `json:"id"`
	// Title is the name people read.
	Title string `json:"title"`
}

// listedDatabases is the structured content of a page of list_databases.
type listedDatabases struct {
	Objects    []listedDatabase `json:"objects"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// listDatabasesTool is list_databases as tools/list shows it. It takes no
// database, reaches every database, and carries the listing mark, so Straza
// calls it to learn the databases and their ids.
func listDatabasesTool() *mcp.Tool {
	destructive, openWorld := false, false
	str := func(description string) *jsonschema.Schema {
		return &jsonschema.Schema{Type: "string", Description: description}
	}
	object := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"value": str("The database name that the other tools take as database."),
			"id":    str("The cluster system identifier, the database OID and the config name, joined by colons. It changes when the database is dropped and created again."),
			"title": str("The display name, which is the config name."),
		},
		Required:      []string{"value", "id", "title"},
		PropertyOrder: []string{"value", "id", "title"},
	}
	return &mcp.Tool{
		Name: listDatabasesName,
		Description: "Lists the databases the caller may use, " + strconv.Itoa(listPage) +
			" per page. Pass an object's value as database to the other tools.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cursor": map[string]any{"type": "string", "description": "The nextCursor of the previous page. Omit for the first page."},
			},
			"required":             []string{},
			"additionalProperties": false,
		},
		OutputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"objects":    {Type: "array", Description: "The databases on this page.", Items: object},
				"nextCursor": str("Set when more follow. Pass it as cursor for the next page."),
			},
			Required:      []string{"objects"},
			PropertyOrder: []string{"objects", "nextCursor"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive, IdempotentHint: true, OpenWorldHint: &openWorld},
		Meta: mcp.Meta{
			"ai.straza/object":        map[string]any{"type": "database", "reach": "all"},
			"ai.straza/lists-objects": map[string]any{"type": "database"},
		},
	}
}

// listDatabases returns one page of list_databases: the databases the caller
// may use, by config name, each with its id read on its read login. In
// standalone mode access.yaml must list the tool for the caller, which is
// checked before the arguments are read. A database whose id cannot be read
// fails the whole page, because an item without its id would let a database
// dropped and created again pass as the old one.
func (s *Server) listDatabases(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	caller, _ := callerFrom(ctx)
	if err := s.authorizeTool(caller, listDatabasesName, false); err != nil {
		return toolError(err), nil
	}
	cursor, given, err := listArgs(req.Params.Arguments)
	if err != nil {
		return toolError(err), nil
	}
	names := s.databasesOf(caller)
	start := 0
	if given {
		if start, err = decodeCursor(cursor, names); err != nil {
			return toolError(err), nil
		}
	}
	end := min(start+s.page, len(names))
	out := listedDatabases{Objects: []listedDatabase{}}
	for _, name := range names[start:end] {
		id, err := s.readIDOn(ctx, pg.Login{Database: name, Role: "read"})
		if err != nil {
			reason := err.Error()
			if s.pools != nil {
				reason = s.pools.Reason(err)
			}
			return toolError(errors.New("list_databases cannot read the id of " + name + " on its read login, because " + reason + ". Try again, or ask an administrator to run adaptive-pg-mcp check.")), nil
		}
		out.Objects = append(out.Objects, listedDatabase{Value: name, ID: id, Title: name})
	}
	if end < len(names) {
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(names[end]))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: listText(out)}}, StructuredContent: out}, nil
}

// databasesOf returns the config names of the databases caller may use,
// sorted: every database in straza mode, and those access.yaml lists for
// caller in standalone mode.
func (s *Server) databasesOf(caller string) []string {
	if s.cfg.Mode == config.Straza {
		return slices.Sorted(maps.Keys(s.cfg.Databases))
	}
	for _, c := range s.cfg.Callers {
		if c.Name == caller {
			return slices.Compact(slices.Sorted(slices.Values(c.Databases)))
		}
	}
	return nil
}

// listArgs reads the arguments of list_databases: an optional string cursor
// and nothing else.
func listArgs(raw json.RawMessage) (cursor string, given bool, err error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", false, nil
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return "", false, errors.New(`the arguments of list_databases are not a JSON object. Pass {} for the first page, or {"cursor": "..."} for the next.`)
	}
	for _, key := range slices.Sorted(maps.Keys(args)) {
		if key != "cursor" {
			return "", false, errors.New("list_databases takes only the argument cursor. Leave " + strconv.Quote(key) + " out.")
		}
	}
	v, ok := args["cursor"]
	if !ok {
		return "", false, nil
	}
	if string(bytes.TrimSpace(v)) == "null" || json.Unmarshal(v, &cursor) != nil {
		return "", false, errors.New("the cursor of list_databases must be a string. Pass the nextCursor of the previous page unchanged, or leave cursor out for the first page.")
	}
	return cursor, true, nil
}

// decodeCursor returns where in names the page of cursor starts. A cursor
// holds the name its page starts with, and one that does not decode to a
// name of this listing is refused.
func decodeCursor(cursor string, names []string) (int, error) {
	name, err := base64.RawURLEncoding.DecodeString(cursor)
	i, found := slices.BinarySearch(names, string(name))
	if err != nil || cursor == "" || !found {
		return 0, errors.New("the cursor of list_databases is not from this listing. Pass the nextCursor of the previous page unchanged, or leave cursor out for the first page.")
	}
	return i, nil
}

// listText is the short text of a page, for people.
func listText(out listedDatabases) string {
	names := make([]string, len(out.Objects))
	for i, o := range out.Objects {
		names[i] = o.Value
	}
	var text string
	switch len(names) {
	case 0:
		text = "list_databases found no database the caller may use."
	case 1:
		text = "list_databases found 1 database: " + names[0] + "."
	default:
		text = "list_databases found " + strconv.Itoa(len(names)) + " databases: " + joinAnd(names) + "."
	}
	if out.NextCursor != "" {
		text += " More follow: pass nextCursor as cursor."
	}
	return text
}

// toolError returns a tool result with isError set and err as its text.
func toolError(err error) *mcp.CallToolResult {
	res := &mcp.CallToolResult{}
	res.SetError(err)
	return res
}
