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
	return &jsonschema.Schema{Type: "string", MinLength: atLeast(1), Description: "One SQL statement, sent to Postgres as it is. A text with more than one statement is refused."}
}

// changedSQL reads how many rows the backend of the connection has
// inserted, updated and deleted, in counts that Postgres has not flushed
// yet, also through functions, triggers and data-modifying WITH queries.
// Postgres flushes the counts only between transactions, so the change of a
// count across one statement in a transaction is what that statement
// changed. The first count is of the tables of the database, without the
// materialized views, whose rows change only when REFRESH replaces them. The
// second is of the system catalogs without the TOAST schemas, which a change
// of structure writes, such as TRUNCATE or ALTER TABLE inside a function, and
// which inserts, updates, deletes and nextval leave alone. It also says
// whether Postgres counts at all.
const changedSQL = `SELECT
  (SELECT coalesce(sum(s.n_tup_ins + s.n_tup_upd + s.n_tup_del), 0) FROM pg_stat_xact_user_tables s JOIN pg_class c ON c.oid = s.relid WHERE c.relkind <> 'm')::int8,
  (SELECT coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del), 0) FROM pg_stat_xact_sys_tables WHERE schemaname !~ '^pg_toast')::int8,
  current_setting('track_counts')::bool`

// existingSQL lists the tables of the database before a statement, so that
// execute_ddl can leave out the rows written into a relation the statement
// itself creates, which cannot touch data that existed before.
const existingSQL = `SELECT coalesce(array_agg(relid), '{}')::oid[] FROM pg_stat_xact_user_tables`

// changedInSQL counts as the first count of changedSQL does, in the tables of $1 only.
const changedInSQL = `SELECT coalesce(sum(s.n_tup_ins + s.n_tup_upd + s.n_tup_del), 0)::int8
FROM pg_stat_xact_user_tables s JOIN pg_class c ON c.oid = s.relid
WHERE c.relkind <> 'm' AND s.relid IN (SELECT unnest($1::oid[]))`

// structureChanged is the refusal of a statement with a row tag that changed the system catalogs, with what to do.
func (c *call) structureChanged(tag pgconn.CommandTag, advice string) error {
	return refuse(c.tool + " on " + c.database + " ran a statement whose command tag is " + tag.String() + " and that changed structure, as the system catalogs show, so it was rolled back and nothing was changed. " + advice)
}

// counts are what changedSQL reads.
type counts struct {
	// rows counts the row changes in the tables of the database, and catalog those in the system catalogs.
	rows, catalog int64
	tracked       bool
}

// changedCount reads changedSQL on tx, and refuses the call when Postgres does not count.
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
		return n, refuse(c.tool + " on " + c.database + " cannot count the rows a statement changes, because track_counts is off, so the call is refused and nothing was changed. Ask an administrator to turn track_counts on, which is its default.")
	}
	return n, nil
}

// querySQLTool is query_sql as tools/list shows it.
func (s *Server) querySQLTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{"sql": sqlProp()}, []string{"sql"}, "sql")
	description := "Runs one SQL statement of the caller's own in a read-only transaction on the read login, so it reads and changes nothing. " +
		"Its limit is the grants of that login. It answers columns and rows, each row an array in column order, at most " +
		count(int64(s.cfg.Limits.Rows)) + " rows and " + count(int64(s.cfg.Limits.Bytes)) + " bytes, and truncated is true when more rows follow. " +
		"A statement may run for " + s.cfg.Limits.Timeout.String() + "."
	return &dataTool{tool: readTool(querySQLName, description, in), free: true, run: s.querySQL}
}

// executeSQLTool is execute_sql as tools/list shows it.
func (s *Server) executeSQLTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{"sql": sqlProp()}, []string{"sql"}, "sql")
	description := "Runs one SQL statement of the caller's own that Postgres answers as INSERT, UPDATE, DELETE, MERGE, SELECT, CALL, DO, EXPLAIN or COPY, in a read-write transaction on the write login. " +
		"Any other statement, such as TRUNCATE or a change of structure, is rolled back and refused, because the write cap cannot count it, and execute_ddl runs it. " +
		"Its limit is the grants of that login. A statement that changes more than " + count(int64(s.cfg.Limits.WriteRows)) +
		" rows, counted in every table, is rolled back and refused. It answers rowsAffected, and the columns and rows the statement answers, at most " +
		count(int64(s.cfg.Limits.Rows)) + " rows. A statement may run for " + s.cfg.Limits.Timeout.String() + "."
	return &dataTool{tool: writeTool(executeSQLName, description, in), write: true, free: true, run: s.executeSQL}
}

// querySQL answers query_sql. The statement goes to Postgres as it is, on
// the extended protocol, which refuses more than one statement. The
// connection closes afterwards, because the statement may have changed its
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
		text := "query_sql answered " + plural(int64(len(a.rows)), "row") + " from " + c.database + "."
		return readAnswer(a, text+c.stopped(a, s.cfg.Limits.Rows, "Add a LIMIT, or select fewer columns.")), err
	})
}

// executeSQL answers execute_sql. The statement goes to Postgres as it is,
// and a statement whose command tag does not work on rows, such as TRUNCATE
// or a change of structure, is rolled back and refused, because the write
// cap cannot count what it changes. So is a statement with a row tag that
// changed the system catalogs, such as a DO block, a procedure or a
// function that truncates or alters a table. The rows it changed are the larger of what its command tag says and
// what Postgres counted in every table across the statement, so a change
// inside a function or a WITH query counts against write_rows too. The
// connection closes afterwards, because the statement may have changed its
// session.
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
			return nil, refuse("execute_sql on " + c.database + " ran a statement whose command tag is " + a.tag.String() + ", which the write cap cannot count, so it was rolled back and nothing was changed. Use execute_ddl for TRUNCATE and for statements that change structure.")
		}
		after, err := c.changedCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		if after.catalog > before.catalog {
			return nil, c.structureChanged(a.tag, "Use execute_ddl for TRUNCATE and for statements that change structure.")
		}
		changed := max(tagChanged(a.tag), after.rows-before.rows)
		if err := c.writeCap(changed, "Narrow the statement"); err != nil {
			return nil, err
		}
		text := "execute_sql changed " + plural(changed, "row") + " on " + c.database + " and answered " + plural(int64(len(a.rows)), "row") + "."
		if a.more {
			text += " The answer stopped at the row or byte cap, and more rows follow."
		}
		return writeAnswer(changed, a, text), nil
	})
}

// rowTags are the command tags of the statements that work on rows, or can
// change rows, which the write cap counts and which execute_sql runs. CALL,
// DO, EXPLAIN and COPY are among them, because a procedure, a DO block, an
// EXPLAIN ANALYZE and a COPY can change rows. execute_ddl runs every other
// statement.
var rowTags = []string{"INSERT", "UPDATE", "DELETE", "MERGE", "SELECT", "CALL", "DO", "EXPLAIN", "COPY"}

// onRows reports whether a command tag is one of rowTags, by its first word.
// The tag that Postgres answers is the only test, and the text of the
// statement is never read.
func onRows(tag pgconn.CommandTag) bool {
	verb, _, _ := strings.Cut(tag.String(), " ")
	return slices.Contains(rowTags, verb)
}

// executeDDLTool is execute_ddl as tools/list shows it.
func (s *Server) executeDDLTool() *dataTool {
	in := s.input(map[string]*jsonschema.Schema{"sql": sqlProp()}, []string{"sql"}, "sql")
	description := "Runs one SQL statement of the caller's own that changes structure, such as CREATE, ALTER or DROP, or a TRUNCATE, in a read-write transaction on the write login. " +
		"No row cap applies to TRUNCATE and to changes of structure, so behind Straza a policy can hold execute_ddl for approval on its own. " +
		"Rows that the statement changes in tables that existed before it, such as through a function in a materialized view, count against the cap of " + count(int64(s.cfg.Limits.WriteRows)) + " rows. " +
		"A statement that Postgres answers as INSERT, UPDATE, DELETE, MERGE, SELECT, CALL, DO, EXPLAIN or COPY and that changes no structure is rolled back and refused, because execute_sql and query_sql run those, so CREATE TABLE AS, SELECT INTO and CREATE MATERIALIZED VIEW run here. " +
		"Its limit is the grants of that login. It answers rowsAffected, and the columns and rows the statement answers, at most " +
		count(int64(s.cfg.Limits.Rows)) + " rows. A statement may run for " + s.cfg.Limits.Timeout.String() + "."
	return &dataTool{tool: writeTool(executeDDLName, description, in), write: true, free: true, run: s.executeDDL}
}

// executeDDL answers execute_ddl. The statement goes to Postgres as it is,
// on the extended protocol. A statement whose command tag works on rows and
// that changed no system catalog is pure row work, which is rolled back and
// refused, so the write cap cannot be got around through it. A statement
// with a row tag that changed the catalogs, such as CREATE TABLE AS, is a
// change of structure and runs. The rows that the statement changes in
// tables that existed before it, such as through a function that a REFRESH
// runs, count against write_rows, and rows written into a relation the
// statement creates do not. TRUNCATE and DROP change no counted rows, so no
// cap limits them. The connection closes afterwards, because the statement
// may have changed its session.
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
			return nil, refuse("execute_ddl on " + c.database + " ran a statement whose command tag is " + a.tag.String() + ", which works on rows and changed no structure, so it was rolled back and nothing was changed. Use execute_sql to change rows under the write cap, or query_sql to read them.")
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
		text := "execute_ddl ran " + a.tag.String() + " on " + c.database + " and answered " + plural(int64(len(a.rows)), "row") + "."
		return writeAnswer(changed, a, text), nil
	})
}

// tagChanged is the number of rows a command tag says the statement
// changed: the count of an INSERT, UPDATE, DELETE or MERGE, and 0 for any
// other statement, whose count is of the rows it answered.
func tagChanged(tag pgconn.CommandTag) int64 {
	if tag.Insert() || tag.Update() || tag.Delete() || strings.HasPrefix(tag.String(), "MERGE ") {
		return tag.RowsAffected()
	}
	return 0
}
