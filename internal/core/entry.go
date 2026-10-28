package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// builtinNames are the names of every built-in tool, whether free_sql serves it or not, which no catalog entry may take.
var builtinNames = []string{
	listDatabasesName, listSchemasName, listTablesName, describeTableName, readRowsName, aggregateName,
	insertRowsName, updateRowsName, deleteRowsName, querySQLName, executeSQLName, executeDDLName,
}

// checkCatalog is the catalog part of the start checks of docs/DESIGN.md
// section 6. It loads the catalog folder of server.yaml, reports each
// statement that no index lists, prepares every indexed statement on each of
// its databases through catalog.Check, and serves the tool of each entry
// that passes. A catalog that does not load refuses the start, and so do the
// refusals of catalog.Check and an entry that Tool cannot turn into a tool.
// ready holds the logins whose own checks ran. In mode standalone it then
// reports each tool that an access.yaml entry lists and the server does not
// serve, unless the catalog did not load, since its tool names are then unknown.
func (s *Server) checkCatalog(ctx context.Context, ready map[pg.Login]bool) []Finding {
	var findings []Finding
	var names []string
	if s.cfg.Catalog != "" {
		entries, reports, err := catalog.Load(s.cfg.Catalog, s.cfg.Databases, builtinNames)
		if err != nil {
			return []Finding{{Refuses: true, Text: err.Error()}}
		}
		for _, r := range reports {
			findings = append(findings, Finding{Text: r})
		}
		typed, refusals := catalog.Check(ctx, s.pools, s.cfg.Limits.Timeout, entries, ready)
		for _, r := range refusals {
			findings = append(findings, Finding{Refuses: true, Text: r})
		}
		for _, e := range typed {
			if err := s.addEntry(e); err != nil {
				findings = append(findings, Finding{Refuses: true, Text: err.Error()})
			}
		}
		for _, e := range entries {
			names = append(names, e.Name)
		}
	}
	return append(findings, s.unserved(names)...)
}

// addEntry serves the tool of a catalog entry whose parameter types
// catalog.Check filled in, or returns the sentence of Tool that says why the
// entry cannot be a tool. Its calls take the same path as the built-in
// tools, on the entry's login and under the entry's limits over those of
// server.yaml.
func (s *Server) addEntry(e catalog.Entry) error {
	tool, err := Tool(e)
	if err != nil {
		return err
	}
	limits := entryLimits(s.cfg.Limits, e.Limits)
	s.addTool(&dataTool{tool: tool, write: e.Mode == catalog.Write, login: e.Login, limits: &limits, fromCatalog: true, run: func(ctx context.Context, c *call, args map[string]any) *mcp.CallToolResult {
		return c.entry(ctx, e, args)
	}})
	return nil
}

// entryLimits returns the limits of server.yaml with each field that the entry's own limits set replaced.
func entryLimits(server config.Limits, own *catalog.Limits) config.Limits {
	if own == nil {
		return server
	}
	if own.Rows > 0 {
		server.Rows = own.Rows
	}
	if own.Bytes > 0 {
		server.Bytes = own.Bytes
	}
	if own.Timeout > 0 {
		server.Timeout = own.Timeout
	}
	if own.WriteRows > 0 {
		server.WriteRows = own.WriteRows
	}
	return server
}

// narrowEntry is the advice of a catalog read that stopped at its row or byte cap.
func narrowEntry(tool string) string {
	return "Narrow the call through its arguments, or ask an administrator to raise the limits of " + tool + " in the catalog."
}

// entry answers a call of a catalog tool: the entry's statement with the
// arguments as its bind parameters in $1 order, each sent as text that
// Postgres reads as the parameter's type, and NULL for an optional parameter
// the call leaves out. A read stops at its row or byte cap with truncated. A
// write is refused and rolled back when it changes more rows than its
// write_rows, counted as execute_sql counts them, or when it changes the
// system catalogs, since a catalog tool changes rows only. The connection
// closes after the call, because a statement that is not the server's own
// can change the session that the pool would hand to the next call.
func (c *call) entry(ctx context.Context, e catalog.Entry, args map[string]any) *mcp.CallToolResult {
	binds := make([]any, len(e.Params))
	for i, p := range e.Params {
		v, given := args[p.Name]
		if !given {
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return toolError(argError(c.tool, "the argument "+p.Name+" does not encode as JSON"))
		}
		binds[i] = bindValue(raw, p.Type == "json" || p.Type == "jsonb")
	}
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		tx.Discard()
		if !c.write {
			a, err := c.read(ctx, tx, c.limits.Rows, e.SQL, binds...)
			if err != nil && !errors.Is(err, errCut) {
				return nil, err
			}
			text := c.tool + " answered " + plural(int64(len(a.rows)), "row") + " from " + c.database + "."
			return readAnswer(a, text+c.stopped(a, c.limits.Rows, narrowEntry(c.tool))), err
		}
		before, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, e.SQL, binds...)
		if err != nil {
			return nil, err
		}
		a, err := c.collect(ctx, tx, rows, c.limits.Rows)
		if err != nil {
			return nil, err
		}
		after, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		if after.catalog > before.catalog {
			return nil, c.structureChanged(a.tag, "A catalog tool may change rows only, so ask an administrator to change the statement in the catalog.")
		}
		changed := max(tagChanged(a.tag), after.rows-before.rows)
		if err := c.writeCap(changed, "Narrow the call through its arguments"); err != nil {
			return nil, err
		}
		text := c.tool + " changed " + plural(changed, "row") + " on " + c.database + " and answered " + plural(int64(len(a.rows)), "row") + "."
		if a.more {
			text += " The answer stopped at the row or byte cap, and more rows follow."
		}
		return writeAnswer(changed, a, text), nil
	})
}

// unserved reports, in mode standalone, each tool that a caller's entry in
// access.yaml lists and the server does not serve, once per caller and
// name, because such a name gives the caller nothing and is often a typing
// mistake. catalogNames are the names of the catalog's entries.
func (s *Server) unserved(catalogNames []string) []Finding {
	if s.cfg.Mode != config.Standalone {
		return nil
	}
	var findings []Finding
	for _, c := range s.cfg.Callers {
		named := map[string]bool{}
		for _, tool := range c.Tools {
			if named[tool] || slices.Contains(catalogNames, tool) || s.servesBuiltin(tool) {
				continue
			}
			named[tool] = true
			findings = append(findings, Finding{Text: s.unservedText(c.Name, tool)})
		}
	}
	return findings
}

// servesBuiltin reports whether name is a built-in tool that this server serves, as free_sql decides for the free SQL tools.
func (s *Server) servesBuiltin(name string) bool {
	switch name {
	case querySQLName:
		return s.cfg.FreeSQL != config.FreeSQLOff
	case executeSQLName, executeDDLName:
		return s.cfg.FreeSQL == config.FreeSQLReadWrite
	}
	return slices.Contains(builtinNames, name)
}

// unservedText is the report about a tool that caller's entry lists and the server does not serve.
func (s *Server) unservedText(caller, tool string) string {
	lists := caller + " lists " + tool + " in its tools in access.yaml"
	serve := ""
	switch tool {
	case querySQLName:
		serve = "read or read-write"
	case executeSQLName, executeDDLName:
		serve = "read-write"
	}
	if serve != "" {
		return lists + ", and free_sql " + string(s.cfg.FreeSQL) + " leaves " + tool + " out, so the entry gives it nothing. Set free_sql to " + serve + " to serve " + tool + ", or remove it from the tools of " + caller + "."
	}
	return lists + ", and adaptive-pg-mcp serves no built-in tool or catalog tool of that name, so the entry gives it nothing. Correct the name, or remove it from the tools of " + caller + "."
}
