package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// resultColumn is one column of a result, as the output schema names it.
type resultColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// collected is what a statement returned, within the row and byte caps.
type collected struct {
	columns []resultColumn
	rows    [][]any
	// more is true when the statement returned more rows than collected holds.
	more bool
	// bytesFull is true when the byte cap stopped the result, and false when
	// the row cap did.
	bytesFull bool
	// tag is the command tag, which says how many rows a write changed. A read
	// that stopped early has none.
	tag pgconn.CommandTag
}

// scan reads at most maxRows rows within the byte cap, and one row more to
// learn whether more exist. A row counts against the byte cap by the larger
// of its size on the wire and its JSON. The wire size is checked before the
// row is decoded, so a row over the cap is never decoded, though pgx still
// receives it whole.
func (c *call) scan(rows pgx.Rows, maxRows int) (collected, error) {
	fields := rows.FieldDescriptions()
	types := rows.Conn().TypeMap()
	a := collected{rows: [][]any{}}
	size, limit := 0, c.limits.Bytes
	for rows.Next() {
		if len(a.rows) >= maxRows {
			a.more = true
			break
		}
		raw := rows.RawValues()
		wire := 0
		for _, v := range raw {
			wire += len(v)
		}
		if size+wire > limit {
			a.more, a.bytesFull = true, true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return a, err
		}
		row := make([]any, len(values))
		for i, v := range values {
			if row[i], err = jsonValue(types, fields[i], raw[i], v); err != nil {
				return a, err
			}
		}
		b, err := json.Marshal(row)
		if err != nil {
			return a, err
		}
		if size+max(wire, len(b)) > limit {
			a.more, a.bytesFull = true, true
			break
		}
		size += max(wire, len(b))
		a.rows = append(a.rows, row)
	}
	return a, nil
}

// collect reads the rows of a write within the caps, and then the rest of
// the result, because a write needs its command tag and its statement must
// finish before the transaction commits.
func (c *call) collect(ctx context.Context, tx *pg.Tx, rows pgx.Rows, maxRows int) (collected, error) {
	defer rows.Close()
	a, err := c.scan(rows, maxRows)
	if err != nil {
		return a, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, err
	}
	a.tag = rows.CommandTag()
	cols, unknown := knownTypes(rows.Conn().TypeMap(), rows.FieldDescriptions())
	a.columns = cols
	return a, nameTypes(ctx, tx, cols, unknown)
}

// errCut ends the transaction of a read that stopped its statement at the
// cap. The transaction rolls back, which changes nothing for a read, and the
// result stands.
var errCut = errors.New("the read stopped its statement at the row or byte cap")

// read sends a read statement and reads its rows within the caps. When a
// cap stops the result, read cancels the statement instead of reading the
// rest. pgx cancels a statement only by closing its connection, so the
// connection is discarded and read returns the rows with errCut. The type
// names that need a query are then read after the transaction.
func (c *call) read(ctx context.Context, tx *pg.Tx, maxRows int, sql string, args ...any) (collected, error) {
	statement, stop := context.WithCancel(ctx)
	defer stop()
	rows, err := tx.Query(statement, sql, args...)
	if err != nil {
		return collected{}, err
	}
	defer rows.Close()
	a, err := c.scan(rows, maxRows)
	if err != nil {
		return a, err
	}
	cols, unknown := knownTypes(rows.Conn().TypeMap(), rows.FieldDescriptions())
	a.columns = cols
	if a.more {
		tx.Discard()
		stop()
		rows.Close()
		c.untyped = unknown
		c.typed = cols
		return a, errCut
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, err
	}
	return a, nameTypes(ctx, tx, cols, unknown)
}

// jsonValue turns one value of a result into its JSON value.
// Booleans, integers, finite floats and numerics are JSON numbers and
// booleans, json and jsonb are their JSON as Postgres holds it, dates and
// timestamps are ISO 8601 strings, with timestamptz in UTC, and every other
// type is its Postgres text form, such as a uuid or an interval.
func jsonValue(types *pgtype.Map, f pgconn.FieldDescription, raw []byte, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.DataTypeOID {
	case pgtype.JSONOID, pgtype.JSONBOID:
		if f.DataTypeOID == pgtype.JSONBOID && f.Format == pgtype.BinaryFormatCode && len(raw) > 0 {
			raw = raw[1:]
		}
		return json.RawMessage(bytes.Clone(raw)), nil
	case pgtype.NumericOID:
		if n, ok := v.(pgtype.Numeric); ok {
			b, err := n.MarshalJSON()
			return json.RawMessage(b), err
		}
	}
	switch x := v.(type) {
	case bool, int16, int32, int64, uint32, string:
		return x, nil
	case float32:
		return finite(float64(x)), nil
	case float64:
		return finite(x), nil
	case pgtype.InfinityModifier:
		return x.String(), nil
	case time.Time:
		switch f.DataTypeOID {
		case pgtype.DateOID:
			return x.Format(time.DateOnly), nil
		case pgtype.TimestampOID:
			return x.Format("2006-01-02T15:04:05.999999"), nil
		}
		return x.UTC().Format(time.RFC3339Nano), nil
	}
	b, err := types.Encode(f.DataTypeOID, pgtype.TextFormatCode, v, nil)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// finite is f as a JSON number, or as the text Postgres writes for NaN and the
// infinities, which JSON has no number for.
func finite(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return f
}

// typeNamesSQL reads the type names of OIDs that pgx does not know, such as an
// enum.
const typeNamesSQL = `SELECT t.oid::int8, t.typname::text FROM pg_type t WHERE t.oid::int8 = ANY($1)`

// untypedColumn is a result column whose type pgx does not know, by its
// place and its type OID.
type untypedColumn struct {
	at  int
	oid int64
}

// knownTypes names each result column, and its Postgres type when pgx knows
// it, and lists the columns whose type needs a query.
func knownTypes(types *pgtype.Map, fields []pgconn.FieldDescription) ([]resultColumn, []untypedColumn) {
	cols := make([]resultColumn, len(fields))
	var unknown []untypedColumn
	for i, f := range fields {
		cols[i].Name = f.Name
		if t, ok := types.TypeForOID(f.DataTypeOID); ok {
			cols[i].Type = t.Name
		} else {
			unknown = append(unknown, untypedColumn{at: i, oid: int64(f.DataTypeOID)})
		}
	}
	return cols, unknown
}

// nameTypes reads the names of the unknown types on tx into cols, and writes
// oid and the number for a type that pg_type does not hold.
func nameTypes(ctx context.Context, tx *pg.Tx, cols []resultColumn, unknown []untypedColumn) error {
	if len(unknown) == 0 {
		return nil
	}
	oids := make([]int64, len(unknown))
	for i, u := range unknown {
		oids[i] = u.oid
	}
	rows, err := tx.Query(ctx, typeNamesSQL, oids)
	if err != nil {
		return err
	}
	byOID := map[int64]string{}
	var oid int64
	var typeName string
	if _, err := pgx.ForEachRow(rows, []any{&oid, &typeName}, func() error {
		byOID[oid] = typeName
		return nil
	}); err != nil {
		return err
	}
	for _, u := range unknown {
		cols[u.at].Type = byOID[u.oid]
		if cols[u.at].Type == "" {
			cols[u.at].Type = "oid " + strconv.FormatInt(u.oid, 10)
		}
	}
	return nil
}

// jsonText writes v as JSON.
func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
