package core

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// freeSQLArgs are the arguments of query_sql and execute_sql.
type freeSQLArgs struct {
	Database string `json:"database"`
	SQL      string `json:"sql"`
}

// sqlProp is the statement of query_sql and execute_sql.
func sqlProp() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", MinLength: atLeast(1), Description: "One SQL statement, sent to Postgres unchanged. More than one statement is refused."}
}

// changedSQL reads the rows the backend has inserted, updated and deleted in
// the current transaction, also through functions, triggers and
// data-modifying WITH queries. Postgres flushes these counts only between
// transactions, so their change across one statement is what that statement
// changed. The first count covers the user tables without the materialized
// views, whose rows change only when REFRESH replaces them. The second
// covers the system catalogs outside the TOAST schemas, which a change of
// structure writes, such as TRUNCATE or ALTER TABLE inside a function, and
// which row writes and nextval leave alone. The third column is
// track_counts, without which Postgres counts nothing.
const changedSQL = `SELECT
  (SELECT coalesce(sum(s.n_tup_ins + s.n_tup_upd + s.n_tup_del), 0) FROM pg_stat_xact_user_tables s JOIN pg_class c ON c.oid = s.relid WHERE c.relkind <> 'm')::int8,
  (SELECT coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del), 0) FROM pg_stat_xact_sys_tables WHERE schemaname !~ '^pg_toast')::int8,
  current_setting('track_counts')::bool`

// existingSQL lists the user tables before a statement, so that execute_ddl
// can leave out the rows written into a relation the statement creates.
const existingSQL = `SELECT coalesce(array_agg(relid), '{}')::oid[] FROM pg_stat_xact_user_tables`

// changedInSQL counts as the first count of changedSQL does, in the tables of
// $1 only.
const changedInSQL = `SELECT coalesce(sum(s.n_tup_ins + s.n_tup_upd + s.n_tup_del), 0)::int8
FROM pg_stat_xact_user_tables s JOIN pg_class c ON c.oid = s.relid
WHERE c.relkind <> 'm' AND s.relid IN (SELECT unnest($1::oid[]))`

// structureChanged is the refusal of a statement with a row tag that changed
// the system catalogs, followed by advice.
func (c *call) structureChanged(tag pgconn.CommandTag, advice string) error {
	return refuse(c.tool + " on " + c.database + " rolled back the statement with command tag " + tag.String() + ", because it changed the system catalogs. " + advice)
}

// counts are what changedSQL reads.
type counts struct {
	// rows counts the row changes in the user tables, and catalog those in the
	// system catalogs.
	rows, catalog int64
	tracked       bool
}

// changedCount reads changedSQL on tx, and refuses the call when track_counts
// is off.
func (c *call) changedCount(ctx context.Context, tx *pg.Tx) (counts, error) {
	rows, err := tx.Query(ctx, changedSQL)
	if err != nil {
		return counts{}, err
	}
	var n counts
	if err := exactlyOne(rows, &n.rows, &n.catalog, &n.tracked); err != nil {
		return n, err
	}
	if !n.tracked {
		return n, refuse(c.tool + " on " + c.database + " changed nothing, because track_counts is off and the write cap cannot count rows. Ask an administrator to turn track_counts on, its default.")
	}
	return n, nil
}

// querySQLTool is query_sql as tools/list shows it.
func (s *Server) querySQLTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{"sql": sqlProp()}, []string{"sql"}, "sql")
	description := "Runs one SQL statement in a read-only transaction, so it cannot change data. " +
		"Returns at most " + count(int64(s.cfg.Limits.Rows)) + " rows and " + count(int64(s.cfg.Limits.Bytes)) + " bytes. " +
		"Times out after " + s.cfg.Limits.Timeout.String() + "."
	return &dataTool{tool: readTool(querySQLName, description, in), free: true, run: s.querySQL}
}

// executeSQLTool is execute_sql as tools/list shows it.
func (s *Server) executeSQLTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{"sql": sqlProp()}, []string{"sql"}, "sql")
	description := "Runs one SQL statement that works on rows, such as INSERT, UPDATE, DELETE, MERGE, CALL or DO. " +
		"Rolls back TRUNCATE and changes of structure, which need execute_ddl, and a statement that changes more than " +
		count(int64(s.cfg.Limits.WriteRows)) + " rows across all tables. Returns at most " + count(int64(s.cfg.Limits.Rows)) + " rows. " +
		"Times out after " + s.cfg.Limits.Timeout.String() + "."
	return &dataTool{tool: writeTool(executeSQLName, description, in), write: true, free: true, run: s.executeSQL}
}

// querySQL runs query_sql. The statement goes to Postgres unchanged on the
// extended protocol, which refuses more than one statement. The connection
// is discarded afterwards, because the statement may have changed its
// session.
func (s *Server) querySQL(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args freeSQLArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		tx.Discard()
		a, err := c.read(ctx, tx, s.cfg.Limits.Rows, args.SQL)
		if err != nil && !errors.Is(err, errCut) {
			return nil, err
		}
		text := "query_sql returned " + plural(int64(len(a.rows)), "row") + " from " + c.database + "."
		return readAnswer(a, text+c.stopped(a, s.cfg.Limits.Rows, "Add a LIMIT, or select fewer columns.")), err
	})
}

// executeSQL runs execute_sql. The statement goes to Postgres unchanged. A
// statement whose command tag is not in rowTags, such as TRUNCATE or a change
// of structure, is rolled back and refused, because the write cap cannot
// count what it changes. So is a statement with a row tag that changed the
// system catalogs, such as a DO block or a function that truncates or alters
// a table. The rows changed are the larger of the command tag count and what
// Postgres counted in every table, so a change inside a function or a WITH
// query counts against write_rows too. The connection is discarded
// afterwards, because the statement may have changed its session.
func (s *Server) executeSQL(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args freeSQLArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		tx.Discard()
		before, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, args.SQL)
		if err != nil {
			return nil, err
		}
		a, err := c.collect(ctx, tx, rows, s.cfg.Limits.Rows)
		if err != nil {
			return nil, err
		}
		if !onRows(a.tag) {
			return nil, refuse("execute_sql on " + c.database + " rolled back the statement with command tag " + a.tag.String() + ", which the write cap cannot count. Use execute_ddl for TRUNCATE and changes of structure.")
		}
		after, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		if after.catalog > before.catalog {
			return nil, c.structureChanged(a.tag, "Use execute_ddl for TRUNCATE and changes of structure.")
		}
		changed := max(tagChanged(a.tag), after.rows-before.rows)
		if err := c.writeCap(changed, "Narrow the statement"); err != nil {
			return nil, err
		}
		text := "execute_sql changed " + plural(changed, "row") + " on " + c.database + " and returned " + plural(int64(len(a.rows)), "row") + "."
		if a.more {
			text += " More rows exist beyond the row or byte cap."
		}
		return writeAnswer(changed, a, text), nil
	})
}

// rowTags are the first words of the command tags that execute_sql runs and
// the write cap counts. CALL, DO, EXPLAIN and COPY are among them, because a
// procedure, a DO block, EXPLAIN ANALYZE and COPY FROM can change rows.
// execute_ddl runs every other statement.
var rowTags = []string{"INSERT", "UPDATE", "DELETE", "MERGE", "SELECT", "CALL", "DO", "EXPLAIN", "COPY"}

// onRows reports whether the first word of tag is one of rowTags. The command
// tag that Postgres reports is the only test, and the statement text is never
// read.
func onRows(tag pgconn.CommandTag) bool {
	verb, _, _ := strings.Cut(tag.String(), " ")
	return slices.Contains(rowTags, verb)
}

// executeDDLTool is execute_ddl as tools/list shows it.
func (s *Server) executeDDLTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{"sql": sqlProp()}, []string{"sql"}, "sql")
	description := "Runs one DDL statement, such as CREATE, ALTER or DROP, or a TRUNCATE. " +
		"Rolls back a statement that only works on rows, which needs execute_sql, and one that changes more than " +
		count(int64(s.cfg.Limits.WriteRows)) + " rows in tables that existed before it. Returns at most " + count(int64(s.cfg.Limits.Rows)) + " rows. " +
		"Times out after " + s.cfg.Limits.Timeout.String() + "."
	return &dataTool{tool: writeTool(executeDDLName, description, in), write: true, free: true, run: s.executeDDL}
}

// executeDDL runs execute_ddl. The statement goes to Postgres unchanged on
// the extended protocol. A statement with a row tag that changed no system
// catalog only works on rows, and is rolled back and refused, so the write
// cap applies to it through execute_sql. A statement with a row tag that
// changed the catalogs, such as CREATE TABLE AS, is a change of structure and
// runs. Rows the statement changes in tables that existed before it, such as
// through a function that a REFRESH runs, count against write_rows, and rows
// written into a relation the statement creates do not. TRUNCATE and DROP
// change no counted rows, so no cap applies to them. The connection is
// discarded afterwards, because the statement may have changed its session.
func (s *Server) executeDDL(ctx context.Context, c *call, raw map[string]any) *mcp.CallToolResult {
	var args freeSQLArgs
	if err := c.decode(raw, &args); err != nil {
		return toolError(err)
	}
	return c.run(ctx, func(tx *pg.Tx) (*mcp.CallToolResult, error) {
		tx.Discard()
		before, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		var existing []uint32
		list, err := tx.Query(ctx, existingSQL)
		if err != nil {
			return nil, err
		}
		if err := exactlyOne(list, &existing); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, args.SQL)
		if err != nil {
			return nil, err
		}
		a, err := c.collect(ctx, tx, rows, s.cfg.Limits.Rows)
		if err != nil {
			return nil, err
		}
		after, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		if onRows(a.tag) && after.catalog == before.catalog {
			return nil, refuse("execute_ddl on " + c.database + " rolled back the statement with command tag " + a.tag.String() + ", because it changed no structure. Use execute_sql to change rows, or query_sql to read them.")
		}
		var inExisting int64
		counted, err := tx.Query(ctx, changedInSQL, existing)
		if err != nil {
			return nil, err
		}
		if err := exactlyOne(counted, &inExisting); err != nil {
			return nil, err
		}
		changed := inExisting - before.rows
		if err := c.writeCap(changed, "Run row changes through execute_sql"); err != nil {
			return nil, err
		}
		text := "execute_ddl ran " + a.tag.String() + " on " + c.database + " and returned " + plural(int64(len(a.rows)), "row") + "."
		return writeAnswer(changed, a, text), nil
	})
}

// tagChanged returns the row count of an INSERT, UPDATE, DELETE or MERGE
// command tag, and 0 for any other tag, whose count is of the rows returned.
func tagChanged(tag pgconn.CommandTag) int64 {
	if tag.Insert() || tag.Update() || tag.Delete() || strings.HasPrefix(tag.String(), "MERGE ") {
		return tag.RowsAffected()
	}
	return 0
}
