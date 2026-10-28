package catalog

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// prepareTimeout bounds the prepare of one statement on one database, the wait for a connection included.
const prepareTimeout = 30 * time.Second

// typeNamesSQL reads the type name of each OID in $1, with a domain read as
// its base type through every domain it is built on, because a parameter of
// a domain takes the values of its base type. A type outside pg_catalog
// carries its schema, so that a type of the database's own never passes for
// a built-in type of the same name.
const typeNamesSQL = `WITH RECURSIVE chain (param, type) AS (
  SELECT t.oid, t.oid FROM pg_type t WHERE t.oid::int8 = ANY($1)
  UNION ALL
  SELECT c.param, t.typbasetype FROM chain c JOIN pg_type t ON t.oid = c.type WHERE t.typtype = 'd'
)
SELECT c.param::int8,
  CASE WHEN n.nspname = 'pg_catalog' THEN t.typname::text ELSE n.nspname::text || '.' || t.typname::text END
FROM chain c JOIN pg_type t ON t.oid = c.type JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE t.typtype <> 'd'`

// Check is the catalog check of the start checks. It prepares the statement
// of every entry on each of its databases, on the entry's login, through
// pg.Tx.Describe in a read-only transaction, so no statement runs. It refuses
// an entry whose statement does not prepare on one of its databases, whose
// parameter count differs from its index, or whose parameter types differ
// between two of its databases. It returns the entries that passed, with each
// Param's Type filled from what Postgres reports, and a sentence for each
// refusal. ready holds the logins whose own start checks ran. A login that is
// not ready is skipped, because its own refusal already stops the start, and
// an entry that runs on it is not returned.
func Check(ctx context.Context, pools *pg.Pools, timeout time.Duration, entries []Entry, ready map[pg.Login]bool) ([]Entry, []string) {
	var passed []Entry
	var refusals []string
	for _, e := range entries {
		typed, refused := checkEntry(ctx, pools, timeout, e, ready)
		refusals = append(refusals, refused...)
		if typed != nil {
			passed = append(passed, *typed)
		}
	}
	return passed, refusals
}

// checkEntry checks one entry on each of its databases, and returns it with
// its parameter types, or nil and the refusals.
func checkEntry(ctx context.Context, pools *pg.Pools, timeout time.Duration, e Entry, ready map[pg.Login]bool) (*Entry, []string) {
	at := e.Source + " statement " + e.Name
	var refusals []string
	var types []string
	firstDB, whole := "", true
	for _, db := range e.Databases {
		l := pg.Login{Database: db, Role: e.Login}
		if !ready[l] {
			whole = false
			continue
		}
		got, err := describe(ctx, pools, timeout, l, e.SQL)
		if err != nil {
			refusals = append(refusals, at+" does not prepare on "+db+": "+pools.Reason(err)+". Fix the statement or remove "+db+" from its databases.")
			whole = false
			continue
		}
		if len(got) != len(e.Params) {
			listed := "lists none"
			if len(e.Params) > 0 {
				listed = "lists " + strconv.Itoa(len(e.Params))
			}
			return nil, append(refusals, at+" takes "+parameters(len(got))+" on "+db+", and its index "+listed+". List each parameter of the statement in params, in $1 order.")
		}
		if types == nil {
			types, firstDB = got, db
			continue
		}
		for i, typ := range got {
			if typ != types[i] {
				n := strconv.Itoa(i + 1)
				refusals = append(refusals, at+" has parameter $"+n+" "+e.Params[i].Name+" of type "+types[i]+" on "+firstDB+" and of type "+typ+" on "+db+
					", and a tool argument takes one type. Make the types match, or cast the parameter in the statement, such as $"+n+"::"+types[i]+".")
				whole = false
			}
		}
	}
	if !whole || types == nil {
		return nil, refusals
	}
	e.Params = slices.Clone(e.Params)
	for i := range e.Params {
		e.Params[i].Type = types[i]
	}
	return &e, nil
}

// parameters writes a count of parameters, such as 1 parameter or no parameters.
func parameters(n int) string {
	switch n {
	case 0:
		return "no parameters"
	case 1:
		return "1 parameter"
	}
	return strconv.Itoa(n) + " parameters"
}

// describe prepares sql on login l in a read-only transaction of its own,
// and returns the type names of its parameters in $1 order.
func describe(ctx context.Context, pools *pg.Pools, timeout time.Duration, l pg.Login, sql string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()
	types := []string{}
	err := pools.Run(ctx, l, pg.ReadOnly, timeout, func(tx *pg.Tx) error {
		oids, err := tx.Describe(ctx, sql)
		if err != nil || len(oids) == 0 {
			return err
		}
		types, err = typeNames(ctx, tx, oids)
		return err
	})
	return types, err
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
