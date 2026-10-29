package catalog

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// prepareTimeout bounds the prepare and plan of one statement on one
// database, including the wait for a connection.
const prepareTimeout = 30 * time.Second

// explainPrefix plans a statement without running it. The explicit ANALYZE
// false keeps the statement text from turning it into EXPLAIN ANALYZE.
const explainPrefix = "EXPLAIN (ANALYZE false) "

const (
	// insufficientPrivilege is the SQLSTATE of a missing grant.
	insufficientPrivilege = "42501"
	// syntaxError is the SQLSTATE that EXPLAIN reports for a statement it
	// cannot plan, such as CALL.
	syntaxError = "42601"
)

// nullClasses are the SQLSTATE classes of failures that a NULL parameter can
// cause while Postgres plans: data exceptions, integrity constraint
// violations, PL/pgSQL errors such as RAISE, and SQL and external routine
// exceptions. A plan that fails with one of them is only reported, because a
// real call binds real values.
var nullClasses = []string{"22", "23", "P0", "2F", "38", "39"}

// errRollback ends the transaction of a check that succeeded, so Run rolls
// it back and nothing the plan did, such as a notification, is committed.
var errRollback = errors.New("the catalog check rolls its transaction back")

// typeNamesSQL reads the type name of each OID in $1. A domain reads as its
// base type, through every domain it is built on, because a domain parameter
// takes the values of its base type. A type outside pg_catalog carries its
// schema, so a user type never passes for a built-in type of the same name.
const typeNamesSQL = `WITH RECURSIVE chain (param, type) AS (
  SELECT t.oid, t.oid FROM pg_type t WHERE t.oid::int8 = ANY($1)
  UNION ALL
  SELECT c.param, t.typbasetype FROM chain c JOIN pg_type t ON t.oid = c.type WHERE t.typtype = 'd'
)
SELECT c.param::int8,
  CASE WHEN n.nspname = 'pg_catalog' THEN t.typname::text ELSE n.nspname::text || '.' || t.typname::text END
FROM chain c JOIN pg_type t ON t.oid = c.type JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE t.typtype <> 'd'`

// Check is the catalog part of the start checks. On each database of each
// entry, it prepares the statement on the entry's login in a read-only
// transaction through pg.Tx.Describe, then plans it with EXPLAIN, which
// checks the grants of the plan without running it.
//
// Check refuses an entry whose statement does not prepare, whose parameter
// count differs from tools.yaml, whose plan needs a grant the login lacks or
// fails for a reason a NULL parameter cannot cause, or whose parameter types
// differ between its databases. It returns the entries that passed, with
// each Param.Type filled from Postgres, the refusals, and a report for each
// statement whose grants EXPLAIN could not check, which still passes.
//
// ready holds the logins whose own start checks ran. A login not in ready is
// skipped, because its own refusal already stops the start, and an entry
// that runs on it is not returned.
func Check(ctx context.Context, pools *pg.Pools, timeout time.Duration, entries []Entry, ready map[pg.Login]bool) ([]Entry, []string, []string) {
	var passed []Entry
	var refusals, reports []string
	for _, e := range entries {
		typed, refused, reported := checkEntry(ctx, pools, timeout, e, ready)
		refusals = append(refusals, refused...)
		reports = append(reports, reported...)
		if typed != nil {
			passed = append(passed, *typed)
		}
	}
	return passed, refusals, reports
}

// checkEntry checks one entry on each of its databases. It returns the entry
// with its parameter types, or nil and the refusals, and the reports either
// way.
func checkEntry(ctx context.Context, pools *pg.Pools, timeout time.Duration, e Entry, ready map[pg.Login]bool) (*Entry, []string, []string) {
	at := e.Source + " statement " + e.Name
	var refusals, reports []string
	var types []string
	firstDB, whole := "", true
	for _, db := range e.Databases {
		l := pg.Login{Database: db, Role: e.Login}
		if !ready[l] {
			whole = false
			continue
		}
		got, err := describe(ctx, pools, timeout, l, e.SQL, len(e.Params))
		if errors.As(err, new(*planError)) {
			refusal, report := planFinding(pools, at, db, "the "+e.Login+" login of "+db, err)
			if refusal != "" {
				refusals = append(refusals, refusal)
				whole = false
				continue
			}
			reports = append(reports, report)
			err = nil
		}
		if err != nil {
			refusals = append(refusals, at+" does not prepare on "+db+": "+pools.Reason(err)+". Fix the statement, or remove "+db+" from its databases.")
			whole = false
			continue
		}
		if len(got) != len(e.Params) {
			listed := "lists none"
			if len(e.Params) > 0 {
				listed = "lists " + strconv.Itoa(len(e.Params))
			}
			return nil, append(refusals, at+" takes "+parameters(len(got))+" on "+db+", but tools.yaml "+listed+". List each parameter in params, in $1 order."), reports
		}
		if types == nil {
			types, firstDB = got, db
			continue
		}
		for i, typ := range got {
			if typ != types[i] {
				n := strconv.Itoa(i + 1)
				refusals = append(refusals, at+" has parameter $"+n+" ("+e.Params[i].Name+") as "+types[i]+" on "+firstDB+" and as "+typ+" on "+db+
					". Make the types match, or cast it in the statement, such as $"+n+"::"+types[i]+".")
				whole = false
			}
		}
	}
	if !whole || types == nil {
		return nil, refusals, reports
	}
	e.Params = slices.Clone(e.Params)
	for i := range e.Params {
		e.Params[i].Type = types[i]
	}
	return &e, nil, reports
}

// parameters writes a count of parameters, such as 1 parameter or no
// parameters.
func parameters(n int) string {
	switch n {
	case 0:
		return "no parameters"
	case 1:
		return "1 parameter"
	}
	return strconv.Itoa(n) + " parameters"
}

// planFinding returns the refusal or the report for a plan of the statement
// at, on database db and login, that failed with err. A missing grant is a
// refusal. A statement that EXPLAIN cannot plan, or a failure that a NULL
// parameter can cause, is a report, and the tool is served. Any other
// failure, such as a timeout, a lock wait or a dropped connection, is a
// refusal, because a check that cannot run proves nothing.
func planFinding(pools *pg.Pools, at, db, login string, err error) (refusal, report string) {
	reason := pools.Reason(err)
	served := " The tool is served, and its first call fails if " + login + " lacks a grant."
	code := sqlState(err)
	switch {
	case code == insufficientPrivilege:
		return at + " needs a grant that " + login + " lacks: " + reason + ". Grant it, or remove " + db + " from its databases.", ""
	case code == syntaxError:
		return "", at + " cannot be planned by EXPLAIN, so its grants are not checked." + served + " Check its grants by hand."
	case slices.ContainsFunc(nullClasses, func(class string) bool { return strings.HasPrefix(code, class) }):
		return "", at + " does not plan on " + db + " with NULL parameters: " + reason + "." + served + " Fix the statement if real calls fail the same way."
	}
	return at + " could not be planned on " + db + ": " + reason + ". The grants of " + login + " cannot be checked without a plan. Fix the cause, and try again.", ""
}

// describe prepares sql on login l in its own read-only transaction and
// returns its parameter type names in $1 order. When the statement takes
// want parameters, describe also plans it there, and a failed plan is a
// *planError with the types still valid. The transaction always rolls back.
func describe(ctx context.Context, pools *pg.Pools, timeout time.Duration, l pg.Login, sql string, want int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()
	types := []string{}
	err := pools.Run(ctx, l, pg.ReadOnly, timeout, func(tx *pg.Tx) error {
		oids, err := tx.Describe(ctx, sql)
		if err != nil {
			return err
		}
		if len(oids) > 0 {
			if types, err = typeNames(ctx, tx, oids); err != nil {
				return err
			}
		}
		if len(oids) != want {
			return errRollback
		}
		if err := plan(ctx, tx, sql, len(oids)); err != nil {
			return &planError{err: err}
		}
		return errRollback
	})
	if errors.Is(err, errRollback) {
		err = nil
	}
	return types, err
}

// plan sends EXPLAIN (ANALYZE false) of sql on tx with its n parameters
// bound as NULL, and drops the plan rows. Postgres checks the grants of the
// plan while it builds it, and neither runs the statement nor fires a
// trigger, even for a write in a read-only transaction. Planning may
// evaluate a STABLE or IMMUTABLE function with constant arguments, so plan
// makes Run close the connection afterwards, which ends any session-level
// effect of such a function, such as an advisory lock.
func plan(ctx context.Context, tx *pg.Tx, sql string, n int) error {
	tx.Discard()
	rows, err := tx.Query(ctx, explainPrefix+sql, make([]any, n)...)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	return rows.Err()
}

// planError is a failure of EXPLAIN on a statement that prepared.
type planError struct {
	err error
}

// Error returns the text of the failure.
func (e *planError) Error() string { return e.err.Error() }

// Unwrap returns the failure, for errors.As and pg.Pools.Reason.
func (e *planError) Unwrap() error { return e.err }

// sqlState returns the SQLSTATE that Postgres reported in err, or an empty
// string.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// typeNames reads the name of each type in oids on tx, in their order.
func typeNames(ctx context.Context, tx *pg.Tx, oids []uint32) ([]string, error) {
	ids := make([]int64, len(oids))
	for i, oid := range oids {
		ids[i] = int64(oid)
	}
	rows, err := tx.Query(ctx, typeNamesSQL, ids)
	if err != nil {
		return nil, err
	}
	byOID := map[int64]string{}
	var oid int64
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&oid, &name}, func() error {
		byOID[oid] = name
		return nil
	}); err != nil {
		return nil, err
	}
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = byOID[id]
		if names[i] == "" {
			names[i] = "oid " + strconv.FormatInt(id, 10)
		}
	}
	return names, nil
}
