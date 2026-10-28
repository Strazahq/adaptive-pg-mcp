//go:build pg

package core

import (
	"runtime"
	"testing"
	"time"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

func TestAReadStopsAtTheCapWithoutReadingTheRest(t *testing.T) {
	// Each row is larger than the server's send buffer, so a row reaches the
	// client once the next one is made. The answer reads the third row to
	// know that more follow, and every row after the fourth sleeps.
	// The enum column has a type that pgx does not know, so its name is read after the cut.
	const slow = "SELECT g, 'happy'::mood AS feeling, CASE WHEN g > 4 THEN pg_sleep(2) END AS slow, repeat('x', 9000) AS filler FROM generate_series(1, 20) g"
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, "CREATE TYPE mood AS ENUM ('happy')")
			cs, out := served(t, l, d, "read", "{ rows: 2, bytes: 1048576, timeout: 5s, write_rows: 100 }")
			start := time.Now()
			res := callTool(t, cs, "query_sql", helpdesk(map[string]any{"sql": slow}), nil)
			took := time.Since(start)
			m := out.check(t, "query_sql", res)
			rows, _ := m["rows"].([]any)
			if len(rows) != 2 || m["truncated"] != true {
				t.Errorf("query_sql at a row cap of 2 answered %d rows and truncated %v, want 2 rows and true", len(rows), m["truncated"])
			}
			if got, want := jsonOf(t, m["columns"]), `[{"name":"g","type":"int4"},{"name":"feeling","type":"mood"},{"name":"slow","type":"void"},{"name":"filler","type":"text"}]`; got != want {
				t.Errorf("the columns are\n%s\nwant\n%s", got, want)
			}
			if took > 1500*time.Millisecond {
				t.Errorf("query_sql took %v, so it read past the row cap into the rows that sleep", took)
			}
			t.Logf("query_sql answered the capped rows in %v", took.Round(time.Millisecond))
		})
	}
}

func TestARowOverTheByteCapIsNotDecoded(t *testing.T) {
	const size = 16 << 20
	// pgx receives the row into a pooled buffer of the next power of two, 32 MiB here, and decoding it would add at least 16 MiB more.
	const bound = 40 << 20
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			cs, out := served(t, l, d, "read", "")
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			res := callTool(t, cs, "query_sql", helpdesk(map[string]any{"sql": "SELECT repeat('x', 16777216) AS big"}), nil)
			runtime.ReadMemStats(&after)
			m := out.check(t, "query_sql", res)
			if rows, _ := m["rows"].([]any); len(rows) != 0 || m["truncated"] != true {
				t.Errorf("query_sql of a 16 MiB value at a byte cap of 1 MiB answered %d rows and truncated %v, want none and true", len(rows), m["truncated"])
			}
			want := "query_sql answered 0 rows from helpdesk. More rows follow, and the answer stopped before the next row, because it would pass the byte cap of 1,048,576 bytes. Add a LIMIT, or select fewer columns."
			if got := textOf(t, res); got != want {
				t.Errorf("text:\n got %s\nwant %s", got, want)
			}
			alloc := after.TotalAlloc - before.TotalAlloc
			if alloc > bound {
				t.Errorf("the call allocated %d MiB for one value of 16 MiB, so the row over the byte cap was decoded", alloc>>20)
			}
			t.Logf("the call allocated %d MiB for one value of 16 MiB", alloc>>20)
		})
	}
}
