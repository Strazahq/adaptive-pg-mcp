package core

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// callerEntry returns the caller of that name in access.yaml, or nil.
func (s *Server) callerEntry(caller string) *config.Caller {
	for i := range s.cfg.Callers {
		if s.cfg.Callers[i].Name == caller {
			return &s.cfg.Callers[i]
		}
	}
	return nil
}

// authorizeTool is the tool part of step 2 in docs/DESIGN.md section 5. In
// standalone mode access.yaml must list tool for the caller, and a caller
// narrowed to tables may not use free SQL, which can reach any table. In
// straza mode, Straza decides.
func (s *Server) authorizeTool(caller, tool string, free bool) error {
	if s.cfg.Mode != config.Standalone {
		return nil
	}
	c := s.callerEntry(caller)
	if c == nil {
		return errors.New("access.yaml has no caller named " + caller + ". This is a bug in adaptive-pg-mcp. Report it with the log.")
	}
	if !slices.Contains(c.Tools, tool) {
		return errors.New("access.yaml does not list " + tool + " for " + caller + ". Use a listed tool, or ask an administrator to add it.")
	}
	if free && len(c.Tables) > 0 {
		return errors.New("access.yaml narrows " + caller + " to tables, and " + tool + " can reach any table. Use a table tool such as read_rows, or ask an administrator to remove the tables of " + caller + ".")
	}
	return nil
}

// authorizeDatabase is the database part of step 2: in standalone mode
// access.yaml must list database for the caller.
func (s *Server) authorizeDatabase(caller, database string) error {
	if s.cfg.Mode != config.Standalone {
		return nil
	}
	if c := s.callerEntry(caller); c == nil || !slices.Contains(c.Databases, database) {
		return errors.New("access.yaml does not list " + database + " for " + caller + ". Use a database that list_databases returns, or ask an administrator to add it.")
	}
	return nil
}

// authorizeTable refuses a table outside the tables that access.yaml lists
// for the caller, when it lists any. A table is schema.table, split at its
// first dot, and names compare exactly, as Postgres compares quoted
// identifiers.
func (s *Server) authorizeTable(caller, database, schema, table string) error {
	if s.cfg.Mode != config.Standalone {
		return nil
	}
	c := s.callerEntry(caller)
	if c == nil || len(c.Tables) == 0 {
		return nil
	}
	for _, entry := range c.Tables {
		if sch, tab, ok := strings.Cut(entry, "."); ok && sch == schema && tab == table {
			return nil
		}
	}
	named, tables, those := schema+"."+table, "the tables "+joinAnd(c.Tables), "one of those tables"
	if len(c.Tables) == 1 {
		tables, those = "the table "+c.Tables[0], "that table"
	}
	return errors.New("access.yaml narrows " + caller + " to " + tables + ", so it may not use " + named + " on " + database + ". Name " + those + ", or ask an administrator to add " + named + ".")
}

// tablesOf returns the schemas and the names of the tables that access.yaml
// narrows caller to, split as authorizeTable splits them, in two parallel
// lists. It returns nil and nil for a caller that is not narrowed, and in
// straza mode. A name without a dot is skipped, because it names no table.
func (s *Server) tablesOf(caller string) (schemas, tables []string) {
	if s.cfg.Mode != config.Standalone {
		return nil, nil
	}
	c := s.callerEntry(caller)
	if c == nil || len(c.Tables) == 0 {
		return nil, nil
	}
	schemas, tables = []string{}, []string{}
	for _, entry := range c.Tables {
		if schema, table, ok := strings.Cut(entry, "."); ok {
			schemas, tables = append(schemas, schema), append(tables, table)
		}
	}
	return schemas, tables
}

// freeTools are the free SQL tools, which a caller narrowed to tables may not
// use.
var freeTools = []string{querySQLName, executeSQLName, executeDDLName}

// filterTools is receiving middleware that, in standalone mode, cuts
// tools/list to the tools access.yaml lists for the caller, without the free
// SQL tools for a caller narrowed to tables, and cuts the database enum of
// each tool to the caller's databases. A tool left with no database is left
// out. The caller is the one identify named for this request. The list
// differs per caller, so its cache scope is private. In straza mode every
// caller gets the same list, because Straza decides.
func (s *Server) filterTools(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		list, ok := res.(*mcp.ListToolsResult)
		if err != nil || !ok || s.cfg.Mode != config.Standalone {
			return res, err
		}
		caller, _ := callerFrom(ctx)
		filtered := *list
		filtered.CacheScope = "private"
		filtered.Tools = []*mcp.Tool{}
		entry := s.callerEntry(caller)
		if entry == nil {
			return &filtered, nil
		}
		for _, tool := range list.Tools {
			if !slices.Contains(entry.Tools, tool.Name) || (len(entry.Tables) > 0 && slices.Contains(freeTools, tool.Name)) {
				continue
			}
			if cut, ok := forDatabases(tool, entry.Databases); ok {
				filtered.Tools = append(filtered.Tools, cut)
			}
		}
		return &filtered, nil
	}
}

// forDatabases returns a copy of tool with the enum of its database argument
// cut to databases, or false when no database is left. A tool without a
// database argument, such as list_databases, is returned as it is.
func forDatabases(tool *mcp.Tool, databases []string) (*mcp.Tool, bool) {
	in, ok := tool.InputSchema.(*jsonschema.Schema)
	if !ok || in.Properties[databaseArg] == nil {
		return tool, true
	}
	db := *in.Properties[databaseArg]
	db.Enum = nil
	for _, v := range in.Properties[databaseArg].Enum {
		if name, _ := v.(string); slices.Contains(databases, name) {
			db.Enum = append(db.Enum, v)
		}
	}
	if len(db.Enum) == 0 {
		return nil, false
	}
	cutIn := *in
	cutIn.Properties = maps.Clone(in.Properties)
	cutIn.Properties[databaseArg] = &db
	cut := *tool
	cut.InputSchema = &cutIn
	return &cut, true
}
