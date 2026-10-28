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

// callerEntry returns the access.yaml entry of caller, or nil when no entry has that name.
func (s *Server) callerEntry(caller string) *config.Caller {
	for i := range s.cfg.Callers {
		if s.cfg.Callers[i].Name == caller {
			return &s.cfg.Callers[i]
		}
	}
	return nil
}

// authorizeTool is step 2 of every call in docs/DESIGN.md section 5 for
// what the tool alone decides: in mode standalone, the caller's entry in
// access.yaml lists tool, and a caller narrowed to tables uses no free SQL,
// because free SQL can reach any table. In mode straza, Straza decides.
func (s *Server) authorizeTool(caller, tool string, free bool) error {
	if s.cfg.Mode != config.Standalone {
		return nil
	}
	c := s.callerEntry(caller)
	if c == nil {
		return errors.New("the caller " + caller + " has no entry in access.yaml, so the call is refused. Serve it with adaptive-pg-mcp serve, which names the caller from its token.")
	}
	if !slices.Contains(c.Tools, tool) {
		return errors.New(caller + " may not use " + tool + ", because its entry in access.yaml does not list that tool, so the call is refused. Use a tool that its entry lists, or ask an administrator to add " + tool + " to its tools.")
	}
	if free && len(c.Tables) > 0 {
		return errors.New(caller + " may not use " + tool + ", because its entry in access.yaml narrows it to tables, and free SQL can reach any table, so the call is refused. Use the table tools, such as read_rows, on its tables, or ask an administrator to remove tables from its entry.")
	}
	return nil
}

// authorizeDatabase is the rest of step 2 for the database a call names: in
// mode standalone, the caller's entry in access.yaml lists it.
func (s *Server) authorizeDatabase(caller, database string) error {
	if s.cfg.Mode != config.Standalone {
		return nil
	}
	if c := s.callerEntry(caller); c == nil || !slices.Contains(c.Databases, database) {
		return errors.New(caller + " may not use " + database + ", because its entry in access.yaml does not list that database, so the call is refused. Name a database that list_databases answers, or ask an administrator to add " + database + " to its databases.")
	}
	return nil
}

// authorizeTable narrows a caller whose entry in access.yaml lists tables to
// those tables. An entry names a table as schema.table, split at its first
// dot, and names compare exactly, as Postgres compares quoted identifiers.
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
	return errors.New(caller + " may not use " + named + " on " + database + ", because its entry in access.yaml narrows it to " + tables + ", so the call is refused. Name " + those + ", or ask an administrator to add " + named + " to its tables.")
}

// tablesOf returns the schemas and the names of the tables that a caller's
// access.yaml entry narrows it to, split at the first dot as authorizeTable
// splits them, in two lists of one entry per table. It returns nil and nil
// for a caller that is not narrowed, and in mode straza. An entry without a
// dot names no table.
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

// freeTools are the free SQL tools, which a caller narrowed to tables may not use.
var freeTools = []string{querySQLName, executeSQLName, executeDDLName}

// filterTools is receiving middleware that, in mode standalone, answers
// tools/list with only the tools that the caller's access.yaml entry lists,
// leaving out the free SQL tools for a caller narrowed to tables, and cuts
// the database enum of each to the caller's databases. A tool whose
// cut enum is empty is left out, because the caller can use it on no
// database. The caller is the one identify named for this very request. The
// list differs per caller, so its cache scope is private. In mode straza
// the list is the same for every caller, because Straza decides.
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

// forDatabases returns tool with the enum of its database argument cut to
// databases, a copy that leaves the served tool as it is, and false when no
// database is left. A tool without a database argument, such as
// list_databases, is returned as it is.
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
