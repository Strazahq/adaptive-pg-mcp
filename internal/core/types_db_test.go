//go:build pg

package core

import (
	"testing"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

func TestValuesBindForCommonTypes(t *testing.T) {
	first := map[string]any{
		"id": 1, "i2": 7, "i8": 1234567890123, "n": 12345.678, "f": 1.5, "t": `héllo 'quoted' \ "text"`, "b": true,
		"d": "2026-10-28", "ts": "2026-10-28T12:34:56.789", "tstz": "2026-10-28T14:34:56+02:00",
		"u": "0b9e2f2a-5d1c-4c47-9a39-6a0f8a3a0c11", "j": map[string]any{"b": "x", "a": []any{1, 2}}, "jb": map[string]any{"k": []any{true, nil}},
	}
	firstJSON := `[[1,7,1234567890123,12345.678,1.5,"héllo 'quoted' \\ \"text\"",true,"2026-10-28","2026-10-28T12:34:56.789","2026-10-28T12:34:56Z","0b9e2f2a-5d1c-4c47-9a39-6a0f8a3a0c11",{"a":[1,2],"b":"x"},{"k":[true,null]}]]`
	second := map[string]any{"id": 2, "i8": int64(9007199254740993), "n": "12345678901234567890.123456789", "jb": "plain", "d": "infinity"}
	third := map[string]any{"id": 3, "t": nil, "jb": nil, "b": false}
	// Each column of the first row matches its own value, given as the agent would send it.
	matches := []struct {
		column string
		value  any
	}{
		{"i2", 7}, {"i8", 1234567890123}, {"n", 12345.678}, {"n", "12345.6780"}, {"f", 1.5}, {"t", `héllo 'quoted' \ "text"`}, {"b", true},
		{"d", "2026-10-28"}, {"ts", "2026-10-28 12:34:56.789"}, {"tstz", "2026-10-28T12:34:56Z"}, {"tstz", "2026-10-28T14:34:56+02:00"},
		{"u", "0B9E2F2A-5D1C-4C47-9A39-6A0F8A3A0C11"}, {"jb", map[string]any{"k": []any{true, nil}}},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, readFixture...)
			cs, out := served(t, l, d, "off", "")

			m := out.check(t, "insert_rows", callTool(t, cs, "insert_rows", helpdesk(map[string]any{"table": "kinds", "rows": []any{first, second, third}, "returning": []any{"id"}}), nil))
			if m["rowsAffected"] != float64(3) || rowsJSON(t, m) != `[[1],[2],[3]]` {
				t.Fatalf("insert_rows answered %v", m)
			}
			m = out.check(t, "read_rows", callTool(t, cs, "read_rows", helpdesk(map[string]any{"table": "kinds", "where": []any{map[string]any{"column": "id", "op": "=", "value": 1}}}), nil))
			if got := rowsJSON(t, m); got != firstJSON {
				t.Errorf("the first row reads back as\n%s\nwant\n%s", got, firstJSON)
			}
			wantTypes := `[{"name":"id","type":"int4"},{"name":"i2","type":"int2"},{"name":"i8","type":"int8"},{"name":"n","type":"numeric"},{"name":"f","type":"float8"},{"name":"t","type":"text"},{"name":"b","type":"bool"},{"name":"d","type":"date"},{"name":"ts","type":"timestamp"},{"name":"tstz","type":"timestamptz"},{"name":"u","type":"uuid"},{"name":"j","type":"json"},{"name":"jb","type":"jsonb"}]`
			if got := jsonOf(t, m["columns"]); got != wantTypes {
				t.Errorf("the column types are\n%s\nwant\n%s", got, wantTypes)
			}
			for _, c := range matches {
				t.Run("where "+c.column, func(t *testing.T) {
					m := out.check(t, "read_rows", callTool(t, cs, "read_rows", helpdesk(map[string]any{"table": "kinds", "columns": []any{"id"}, "where": []any{map[string]any{"column": c.column, "op": "=", "value": c.value}}}), nil))
					if got := rowsJSON(t, m); got != `[[1]]` {
						t.Errorf("where %s = %v matched %s, want the first row", c.column, c.value, got)
					}
				})
			}
			m = out.check(t, "read_rows", callTool(t, cs, "read_rows", helpdesk(map[string]any{"table": "kinds", "columns": []any{"t", "b", "jb", "d"}, "where": []any{map[string]any{"column": "id", "op": "in", "value": []any{2, 3}}}, "order_by": []any{map[string]any{"column": "id"}}}), nil))
			if got, want := rowsJSON(t, m), `[[null,null,"plain","infinity"],[null,false,null,null]]`; got != want {
				t.Errorf("the second and third rows read back as %s, want %s", got, want)
			}
			// The values that a JSON number in a float could not carry reach Postgres exactly.
			var exact bool
			if err := d.Connect(t).QueryRow(t.Context(), "SELECT i8 = 9007199254740993 AND n = '12345678901234567890.123456789'::numeric AND jb = '\"plain\"'::jsonb AND d = 'infinity'::date FROM kinds WHERE id = 2").Scan(&exact); err != nil || !exact {
				t.Errorf("the second row does not hold the exact values it was sent (%v)", err)
			}
			m = out.check(t, "update_rows", callTool(t, cs, "update_rows", helpdesk(map[string]any{"table": "kinds", "set": map[string]any{"n": 1.25, "d": "2027-01-01", "jb": []any{1, "two"}, "u": nil}, "where": []any{map[string]any{"column": "id", "op": "=", "value": 1}}, "returning": []any{"n", "d", "jb", "u"}}), nil))
			if got, want := rowsJSON(t, m), `[[1.25,"2027-01-01",[1,"two"],null]]`; got != want || m["rowsAffected"] != float64(1) {
				t.Errorf("update_rows answered %s and %v rows, want %s and 1 row", got, m["rowsAffected"], want)
			}
		})
	}
}

// jsonOf writes v as JSON.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	s, err := jsonText(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
