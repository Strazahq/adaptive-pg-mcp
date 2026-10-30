package core

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// builtinNames are the names of every built-in tool, whether free_sql serves
// it or not. No catalog tool may take one.
var builtinNames = []string{
	listDatabasesName, listSchemasName, listTablesName, describeTableName, readRowsName, aggregateName,
	insertRowsName, updateRowsName, deleteRowsName, querySQLName, executeSQLName, executeDDLName,
}

// checkCatalog is the catalog part of the start checks of docs/DESIGN.md
// section 6. It loads the catalog folder that server.yaml names and reports
// each statement that no tools.yaml lists and each cap a catalog tool raises.
// catalog.Check then prepares and plans every listed statement on each of its
// databases, and checkCatalog reports each statement whose grants the plan
// could not check and serves each catalog tool that passes. ready holds the
// logins whose own checks ran. A catalog that does not load refuses the
// start, and so does each refusal of catalog.Check and each tool that Tool
// cannot build. When the catalog loads, in standalone mode, it also reports
// each tool that access.yaml lists and the server does not serve.
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
		for _, e := range entries {
			findings = append(findings, raisedLimits(s.cfg.Limits, e)...)
		}
		typed, refusals, unchecked := catalog.Check(ctx, s.pools, s.cfg.Limits.Timeout, entries, ready)
		for _, r := range refusals {
			findings = append(findings, Finding{Refuses: true, Text: r})
		}
		for _, r := range unchecked {
			findings = append(findings, Finding{Text: r})
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

// addEntry serves catalog tool e, whose parameter types catalog.Check filled
// in, or returns the error of Tool. Its calls take the path of the built-in
// tools, on the login e names and under its own caps where it sets them.
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

// entryLimits returns the caps of server.yaml with each cap that own sets
// replaced.
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

// raisedLimits reports each cap that catalog tool e raises above server, the
// caps of server.yaml, so the operator sees which tools go past the caps of
// the built-in tools.
func raisedLimits(server config.Limits, e catalog.Entry) []Finding {
	if e.Limits == nil {
		return nil
	}
	at := path.Join(path.Dir(e.Source), "tools.yaml") + ": the catalog tool " + e.Name + " raises limits."
	var findings []Finding
	raise := func(field, to, from string) {
		findings = append(findings, Finding{Text: at + field + " to " + to + ", above the " + from + " in server.yaml. Lower it if that is not intended."})
	}
	if e.Limits.Rows > server.Rows {
		raise("rows", count(int64(e.Limits.Rows)), count(int64(server.Rows)))
	}
	if e.Limits.Bytes > server.Bytes {
		raise("bytes", count(int64(e.Limits.Bytes)), count(int64(server.Bytes)))
	}
	if e.Limits.Timeout > server.Timeout {
		raise("timeout", e.Limits.Timeout.String(), server.Timeout.String())
	}
	if e.Limits.WriteRows > server.WriteRows {
		raise("write_rows", count(int64(e.Limits.WriteRows)), count(int64(server.WriteRows)))
	}
	return findings
}

// narrowEntry is the advice for a catalog read that stopped at its row or
// byte cap.
func narrowEntry(tool string) string {
	return "Narrow the call through its arguments, or ask an administrator to raise the caps of " + tool + " in the catalog."
}

// entry runs a call of catalog tool e: its statement with the arguments
// bound in $1 order, each sent as text that Postgres reads as the
// parameter's type, and NULL for an optional parameter the call leaves out.
// A read stops at its row or byte cap with truncated set. A write is refused
// and rolled back when it changes more rows than its write_rows cap, counted
// as execute_sql counts them, or when it changes the system catalogs.
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
			text := c.tool + " returned " + plural(int64(len(a.rows)), "row") + " from " + c.database + "."
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
			return nil, c.structureChanged(a.tag, "A catalog tool may change rows only. Ask an administrator to change the statement.")
		}
		changed := max(tagChanged(a.tag), after.rows-before.rows)
		if err := c.writeCap(changed, "Narrow the call through its arguments"); err != nil {
			return nil, err
		}
		text := c.tool + " changed " + plural(changed, "row") + " on " + c.database + " and returned " + plural(int64(len(a.rows)), "row") + "."
		if a.more {
			text += " The result stopped at the row or byte cap, and more rows follow."
		}
		return writeAnswer(changed, a, text), nil
	})
}

// unserved reports, in standalone mode, each tool that a caller in
// access.yaml lists and that is neither a built-in tool the server serves nor
// in catalogNames, once per caller and name, since such a name gives the
// caller nothing and is often a typo.
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

// servesBuiltin reports whether name is a built-in tool this server serves.
// For the free SQL tools, free_sql decides.
func (s *Server) servesBuiltin(name string) bool {
	switch name {
	case querySQLName:
		return s.cfg.FreeSQL != config.FreeSQLOff
	case executeSQLName, executeDDLName:
		return s.cfg.FreeSQL == config.FreeSQLReadWrite
	}
	return slices.Contains(builtinNames, name)
}

// unservedText is the report for a tool that caller lists and the server
// does not serve.
func (s *Server) unservedText(caller, tool string) string {
	lists := caller + " lists " + tool + " in access.yaml"
	serve := ""
	switch tool {
	case querySQLName:
		serve = "read or read-write"
	case executeSQLName, executeDDLName:
		serve = "read-write"
	}
	if serve != "" {
		return lists + ", but free_sql is " + string(s.cfg.FreeSQL) + ", which does not serve it. Set free_sql to " + serve + ", or remove " + tool + " from the tools of " + caller + "."
	}
	return lists + ", but no built-in or catalog tool has that name. Correct the name, or remove it from the tools of " + caller + "."
}
