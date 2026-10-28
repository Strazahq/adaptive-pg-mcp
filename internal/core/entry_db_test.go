//go:build pg

package core

import (
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// salesUS is the database sales_us with one login.
const salesUS = "  sales_us:\n    login: { dsn_env: SALES_US_DSN }\n"

// writeFiles writes each file of files under dir, with its folders, and returns dir.
func writeFiles(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// strazaHead is the start of a server.yaml in mode straza with free SQL at freeSQL and the gateway hash of gatewayToken.
func strazaHead(freeSQL string) string {
	h := hashOf(gatewayToken)
	return "version: 1\nmode: straza\nlisten: stdio\nfree_sql: \"" + freeSQL + "\"\nlimits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }\n" +
		"gateway_tokens_sha256: [\"" + hex.EncodeToString(h[:]) + "\"]\n"
}

// standaloneHead is the start of a server.yaml in mode standalone with free SQL at freeSQL and access.yaml beside it.
func standaloneHead(freeSQL string) string {
	return "version: 1\nmode: standalone\nlisten: stdio\nfree_sql: \"" + freeSQL + "\"\nlimits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }\naccess: access.yaml\n"
}

// loadCatalog writes server.yaml from head, the catalog folder and the
// databases section, and access.yaml when access is not empty, and loads
// them with env.
func loadCatalog(t *testing.T, head, catalogDir, databases, access string, env map[string]string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"server.yaml": head + "catalog: " + catalogDir + "\ndatabases:\n" + databases}
	if access != "" {
		files["access.yaml"] = access
	}
	writeFiles(t, dir, files)
	cfg, err := config.Load(filepath.Join(dir, "server.yaml"), func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// inputProp returns the schema of the argument name of tool, as tools/list shows it.
func inputProp(byName map[string]map[string]any, tool, name string) map[string]any {
	in, _ := byName[tool]["inputSchema"].(map[string]any)
	props, _ := in["properties"].(map[string]any)
	p, _ := props[name].(map[string]any)
	return p
}

func TestCatalogStartChecks(t *testing.T) {
	files := map[string]string{
		"finance/revenue.sql": `-- Revenue statements of the finance team.

-- name: revenue_by_region
SELECT region, sum(net) AS net FROM revenue WHERE month = $1 GROUP BY region

-- name: revenue_months
SELECT DISTINCT month FROM revenue ORDER BY month

-- name: revenue_of_region
SELECT sum(net) FROM revenue WHERE region = $1 AND month = $2

-- name: revenue_draft
SELECT 1

-- name: tickets_by_ids
SELECT id FROM tickets WHERE id = ANY($1)

-- name: tickets_by_mood
SELECT id FROM tickets WHERE $1::mood = 'happy'

-- name: tickets_since
SELECT id FROM tickets WHERE id > $1 ORDER BY id
`,
		"finance/tools.yaml": `version: 1
tools:
  revenue_by_region:
    description: Sums net revenue per region for one month. One row per region.
    mode: read
    databases: [sales_eu, sales_us]
    login: read
    params:
      - name: month
        description: The first day of the month, such as 2026-09-01.
        required: true
  revenue_months:
    description: Lists the months that hold revenue. One row per month.
    mode: read
    databases: [sales_eu, sales_us]
    login: read
    limits: { rows: 5000, timeout: 2m, write_rows: 50 }
  revenue_of_region:
    description: Sums net revenue of one region for one month. One row.
    mode: read
    databases: [sales_eu]
    login: read
    params:
      - name: region
        description: The region.
        required: true
  tickets_by_ids:
    description: Lists the tickets of some ids. One row per ticket.
    mode: read
    databases: [sales_eu]
    login: read
    params:
      - name: ids
        description: The ids.
        required: true
  tickets_by_mood:
    description: Lists the tickets when the mood is happy. One row per ticket.
    mode: read
    databases: [sales_eu]
    login: read
    params:
      - name: feeling
        description: The mood.
        required: true
  tickets_since:
    description: Lists the tickets after an id. One row per ticket.
    mode: read
    databases: [sales_eu, sales_us]
    login: read
    params:
      - name: after
        description: The id to start after.
        required: true
`,
	}
	want := []Finding{
		reports("finance/revenue.sql statement revenue_draft is not a tool, because finance/tools.yaml has no entry for it. Add an entry to serve it."),
		reports("finance/tools.yaml entry revenue_months raises limits.rows to 5,000, above the 1,000 that server.yaml sets, so its calls may go past the cap of the built-in tools. Lower it in the entry if that is not intended."),
		reports("finance/tools.yaml entry revenue_months raises limits.timeout to 2m0s, above the 30s that server.yaml sets, so its calls may go past the cap of the built-in tools. Lower it in the entry if that is not intended."),
		refuses(`finance/revenue.sql statement revenue_by_region does not prepare on sales_us: the server answered: column "net" does not exist (SQLSTATE 42703). Fix the statement or remove sales_us from its databases.`),
		refuses("finance/revenue.sql statement revenue_of_region takes 2 parameters on sales_eu, and its index lists 1. List each parameter of the statement in params, in $1 order."),
		refuses("finance/revenue.sql statement tickets_since has parameter $1 after of type int4 on sales_eu and of type int8 on sales_us, and a tool argument takes one type. Make the types match, or cast the parameter in the statement, such as $1::int4."),
		refuses(`finance/revenue.sql statement tickets_by_ids has parameter $1 ids of Postgres type "_int4", which a tool argument cannot carry. Cast it in the statement to one of bool, bpchar, date, float4, float8, int2, int4, int8, json, jsonb, numeric, text, timestamp, timestamptz, uuid, varchar, such as $1::text.`),
		refuses(`finance/revenue.sql statement tickets_by_mood has parameter $1 feeling of Postgres type "public.mood", which a tool argument cannot carry. Cast it in the statement to one of bool, bpchar, date, float4, float8, int2, int4, int8, json, jsonb, numeric, text, timestamp, timestamptz, uuid, varchar, such as $1::text.`),
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			eu, us := pgtest.NewDatabase(t, s), pgtest.NewDatabase(t, s)
			run(t, eu, l, "CREATE TABLE revenue (region text, month date, net numeric)", "CREATE TABLE tickets (id int4 PRIMARY KEY)", "CREATE TYPE mood AS ENUM ('happy', 'sad')", "GRANT SELECT ON revenue, tickets TO {login}")
			run(t, us, l, "CREATE TABLE revenue (region text, month date)", "CREATE TABLE tickets (id int8 PRIMARY KEY)", "GRANT SELECT ON revenue, tickets TO {login}")
			env := map[string]string{"SALES_EU_DSN": l.DSN(t, eu), "SALES_US_DSN": l.DSN(t, us)}
			cfg := loadCatalog(t, strazaHead("off"), writeFiles(t, t.TempDir(), files), single+salesUS, "", env)
			srv := New(cfg, slog.New(slog.DiscardHandler))
			got := srv.Check(t.Context())
			t.Cleanup(srv.Close)
			if !sameFindings(got, want) {
				t.Errorf("Check found:\n%swant:\n%s", lines(got), lines(want))
			}
			_, byName := toolsOf(t, overMemory(t, srv, gatewayCaller, ""))
			if _, ok := byName["revenue_months"]; !ok {
				t.Errorf("tools/list does not serve revenue_months, whose statement passed its start check")
			}
			for _, name := range []string{"revenue_by_region", "revenue_of_region", "revenue_draft", "tickets_by_ids", "tickets_by_mood", "tickets_since"} {
				if _, ok := byName[name]; ok {
					t.Errorf("tools/list serves %s, which is not a tool or whose start check refused", name)
				}
			}
		})
	}
}

// helpdeskCatalog is a catalog of tools on helpdesk for the call tests. Its
// statements read and write tickets, whose note column has a domain over a
// domain over text, and ledger, which only the login finance_reader reads.
var helpdeskCatalog = map[string]string{
	"helpdesk/tickets.sql": `-- name: tickets_by_status
SELECT id, status, note FROM tickets WHERE status = $1 AND ($2::int4 IS NULL OR id <= $2) ORDER BY id

-- name: add_note
UPDATE tickets SET note = $2 WHERE id = $1 RETURNING id, note

-- name: notes_equal
SELECT id FROM tickets WHERE note = $1::tiny_note ORDER BY id

-- name: close_tickets
UPDATE tickets SET status = 'closed' WHERE status = 'open' AND id <= $1

-- name: ledger_total
SELECT current_user::text AS login, sum(amount) AS total FROM ledger

-- name: reopen_in_a_read
UPDATE tickets SET status = 'open' WHERE id = $1

-- name: wipe_ledger
SELECT wipe_ledger()

-- name: ticket_page
SELECT id, status FROM tickets ORDER BY id

-- name: slow_count
SELECT pg_sleep(5), count(*) FROM tickets

-- name: rename_session
SELECT set_config('application_name', 'renamed_by_catalog', false) AS name

-- name: session_name
SELECT current_setting('application_name') AS name
`,
	"helpdesk/tools.yaml": `version: 1
tools:
  tickets_by_status:
    description: Lists the tickets of one status, up to an id. Answers id, status and note per ticket, at most 5 rows.
    mode: read
    databases: [helpdesk]
    login: read
    params:
      - name: status
        description: The status, open, closed or pending.
        required: true
        enum: [open, closed, pending]
      - name: max_id
        description: The highest id to answer, or none for every id.
    limits: { rows: 5 }
  add_note:
    description: Sets the note of one ticket. Answers the ticket's id and note.
    mode: write
    databases: [helpdesk]
    login: write
    params:
      - name: id
        description: The id of the ticket.
        required: true
      - name: note
        description: The note, at most 10 characters.
        required: true
  notes_equal:
    description: Lists the tickets whose note is the given one. Answers their ids.
    mode: read
    databases: [helpdesk]
    login: read
    params:
      - name: note
        description: The note.
        required: true
  close_tickets:
    description: Closes the open tickets up to an id. Answers rowsAffected, and changes at most 10 rows.
    mode: write
    databases: [helpdesk]
    login: write
    params:
      - name: up_to
        description: The highest id to close.
        required: true
        minimum: 1
    limits: { write_rows: 10 }
  ledger_total:
    description: Sums the ledger. Answers the login it ran on and the total.
    mode: read
    databases: [helpdesk]
    login: finance_reader
  reopen_in_a_read:
    description: Reopens one ticket in mode read, which Postgres refuses.
    mode: read
    databases: [helpdesk]
    login: write
    params:
      - name: id
        description: The id of the ticket.
        required: true
  wipe_ledger:
    description: Calls a function that truncates the ledger, which a catalog tool may not do.
    mode: write
    databases: [helpdesk]
    login: write
  ticket_page:
    description: Lists the tickets by id. Answers id and status per ticket, at most 30 bytes.
    mode: read
    databases: [helpdesk]
    login: read
    limits: { bytes: 30 }
  slow_count:
    description: Counts the tickets after a sleep that passes its own timeout. Answers one row.
    mode: read
    databases: [helpdesk]
    login: read
    limits: { timeout: 200ms }
  rename_session:
    description: Changes the application name of its session. Answers the new name.
    mode: read
    databases: [helpdesk]
    login: read
  session_name:
    description: Reads the application name of its session. Answers the name.
    mode: read
    databases: [helpdesk]
    login: read
`,
}

func TestCatalogTools(t *testing.T) {
	refusals := []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"a value its domain refuses", "add_note", map[string]any{"id": 2, "note": "fifteen letters"}, `add_note on helpdesk failed, because the server answered: value for domain tiny_note violates check constraint "tiny_note_check" (SQLSTATE 23514), so nothing was changed. Correct the call as the reason says, and call it again.`},
		{"a write over its own cap", "close_tickets", map[string]any{"up_to": 50}, "close_tickets on helpdesk would change 50 rows and the cap is 10, so nothing was changed. Narrow the call through its arguments, or ask an administrator to raise limits.write_rows of close_tickets in the catalog."},
		{"a read past its own timeout", "slow_count", map[string]any{}, "slow_count on helpdesk failed, because the server answered: canceling statement due to statement timeout (SQLSTATE 57014), so nothing was read. A statement may run for 200ms, so narrow the call, or ask an administrator to raise limits.timeout of slow_count in the catalog."},
		{"mode read runs read-only on any login", "reopen_in_a_read", map[string]any{"id": 1}, "reopen_in_a_read on helpdesk failed, because the server answered: cannot execute UPDATE in a read-only transaction (SQLSTATE 25006), so nothing was read. Correct the call as the reason says, and call it again."},
		{"a write that changes structure", "wipe_ledger", map[string]any{}, "wipe_ledger on helpdesk ran a statement whose command tag is SELECT 1 and that changed structure, as the system catalogs show, so it was rolled back and nothing was changed. A catalog tool may change rows only, so ask an administrator to change the statement in the catalog."},
		{"an argument outside its enum", "tickets_by_status", map[string]any{"status": "lost"}, `tickets_by_status refused the call, because the argument status must be one of "open", "closed" and "pending", and the call passed "lost". Correct the arguments as the input schema of tickets_by_status describes them, and call it again.`},
		{"an argument below its minimum", "close_tickets", map[string]any{"up_to": 0}, "close_tickets refused the call, because the argument up_to must be at least 1, and the call passed 0. Correct the arguments as the input schema of close_tickets describes them, and call it again."},
	}
	firstFive := `[[1,"open",null],[2,"open",null],[3,"open",null],[4,"open",null],[5,"open",null]]`
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			reader := pgtest.NewLogin(t, s, loginPassword)
			writer := pgtest.NewLogin(t, s, loginPassword)
			finance := pgtest.NewLogin(t, s, loginPassword)
			d, other := pgtest.NewDatabase(t, s), pgtest.NewDatabase(t, s)
			run(t, d, writer,
				"CREATE DOMAIN short_note AS text CHECK (length(VALUE) <= 20)",
				"CREATE DOMAIN tiny_note AS short_note CHECK (length(VALUE) <= 10)",
				"CREATE TABLE tickets (id int PRIMARY KEY, status text NOT NULL DEFAULT 'open', note tiny_note)",
				"INSERT INTO tickets (id) SELECT generate_series(1, 60)",
				"CREATE TABLE ledger (amount numeric)",
				"INSERT INTO ledger VALUES (10.5), (4.5)",
				"CREATE FUNCTION wipe_ledger() RETURNS int LANGUAGE plpgsql SECURITY DEFINER AS 'BEGIN TRUNCATE ledger; RETURN 1; END'",
				"GRANT SELECT, INSERT, UPDATE ON tickets TO {login}")
			run(t, d, reader, "GRANT SELECT ON tickets TO {login}")
			run(t, d, finance, "GRANT SELECT ON ledger TO {login}")
			env := map[string]string{"HELPDESK_READ_DSN": reader.DSN(t, d), "HELPDESK_WRITE_DSN": writer.DSN(t, d), "HELPDESK_FINANCE_DSN": finance.DSN(t, d)}
			cfg := loadCatalog(t, strazaHead("off"), writeFiles(t, t.TempDir(), helpdeskCatalog), extra, "", env)
			cs := overStdio(t, New(cfg, slog.New(slog.DiscardHandler)), gatewayToken, "")

			tools, byName := toolsOf(t, cs)
			lintTools(t, tools)
			for name, write := range map[string]bool{"tickets_by_status": false, "add_note": true, "notes_equal": false, "close_tickets": true, "ledger_total": false, "reopen_in_a_read": false, "wipe_ledger": true, "ticket_page": false, "slow_count": false, "rename_session": false, "session_name": false} {
				lintDataTool(t, byName[name], write, []any{"helpdesk"})
			}
			for _, c := range []struct {
				tool, arg, want string
			}{
				{"add_note", "id", "integer"}, {"add_note", "note", "string"}, {"notes_equal", "note", "string"},
				{"close_tickets", "up_to", "integer"}, {"tickets_by_status", "status", "string"}, {"tickets_by_status", "max_id", "integer"},
			} {
				if got := inputProp(byName, c.tool, c.arg)["type"]; got != c.want {
					t.Errorf("%s takes %s as %v, want %s from the type Postgres reports", c.tool, c.arg, got, c.want)
				}
			}
			if got := inputProp(byName, "close_tickets", "up_to")["minimum"]; got != 1.0 {
				t.Errorf("close_tickets takes up_to with the minimum %v, want 1", got)
			}
			in, _ := byName["tickets_by_status"]["inputSchema"].(map[string]any)
			if got := required(in); !slices.Equal(got, []string{"database", "status"}) {
				t.Errorf("tickets_by_status requires %v, want database and status", got)
			}

			out := outputsOf(t, cs)
			for _, c := range refusals {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(c.args), nil)
					if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("%s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
				})
			}
			t.Run("the id check runs on the call's own transaction", func(t *testing.T) {
				res := callTool(t, cs, "close_tickets", helpdesk(map[string]any{"up_to": 9}), mcp.Meta{objectIDKey: idOf(t, other)})
				want := "helpdesk was dropped and created again after Straza listed it, so the call is refused. Try again once Straza lists the databases again, or ask an administrator to refresh the list."
				if got := textOf(t, res); !res.IsError || got != want {
					t.Errorf("close_tickets with the id of another database answered %v:\n got %s\nwant %s", res.IsError, got, want)
				}
			})
			answers := []struct {
				name, tool string
				args       map[string]any
				meta       mcp.Meta
				rows, text string
				// truncated is the truncated of a read, and changed the rowsAffected of a write.
				truncated, changed any
			}{
				{"a read with every argument", "tickets_by_status", map[string]any{"status": "open", "max_id": 3}, nil,
					`[[1,"open",null],[2,"open",null],[3,"open",null]]`, "tickets_by_status answered 3 rows from helpdesk.", false, nil},
				{"a read without its optional argument stops at its own row cap", "tickets_by_status", map[string]any{"status": "open"}, nil,
					firstFive, "tickets_by_status answered 5 rows from helpdesk. More rows follow, and the answer stopped at the row cap of 5. Narrow the call through its arguments, or ask an administrator to raise the limits of tickets_by_status in the catalog.", true, nil},
				{"a write through a parameter of a domain type", "add_note", map[string]any{"id": 1, "note": "printer"}, nil,
					`[[1,"printer"]]`, "add_note changed 1 row on helpdesk and answered 1 row.", nil, 1.0},
				{"a read through a parameter cast to a domain", "notes_equal", map[string]any{"note": "printer"}, nil, `[[1]]`, "", false, nil},
				{"a read stops at its own byte cap", "ticket_page", map[string]any{}, nil, `[[1,"open"],[2,"open"],[3,"open"]]`,
					"ticket_page answered 3 rows from helpdesk. More rows follow, and the answer stopped before the next row, because it would pass the byte cap of 30 bytes. Narrow the call through its arguments, or ask an administrator to raise the limits of ticket_page in the catalog.", true, nil},
				{"a write under its own cap, with the right id", "close_tickets", map[string]any{"up_to": 4}, mcp.Meta{objectIDKey: idOf(t, d)},
					`[]`, "close_tickets changed 4 rows on helpdesk and answered 0 rows.", nil, 4.0},
				{"a read on a login other than read or write", "ledger_total", map[string]any{}, nil, `[["` + finance.Name + `",15]]`, "", false, nil},
			}
			for _, c := range answers {
				t.Run(c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(c.args), c.meta)
					m := out.check(t, c.tool, res)
					if got := rowsJSON(t, m); got != c.rows {
						t.Errorf("rows:\n got %s\nwant %s", got, c.rows)
					}
					if c.truncated != nil && m["truncated"] != c.truncated {
						t.Errorf("truncated is %v, want %v", m["truncated"], c.truncated)
					}
					if c.changed != nil && m["rowsAffected"] != c.changed {
						t.Errorf("rowsAffected is %v, want %v", m["rowsAffected"], c.changed)
					}
					if got := textOf(t, res); c.text != "" && got != c.text {
						t.Errorf("text:\n got %s\nwant %s", got, c.text)
					}
				})
			}
			t.Run("a session change does not reach the next call", func(t *testing.T) {
				if got := rowsJSON(t, out.check(t, "rename_session", callTool(t, cs, "rename_session", helpdesk(map[string]any{}), nil))); got != `[["renamed_by_catalog"]]` {
					t.Fatalf("rename_session answered %s", got)
				}
				if got := rowsJSON(t, out.check(t, "session_name", callTool(t, cs, "session_name", helpdesk(map[string]any{}), nil))); got == `[["renamed_by_catalog"]]` {
					t.Errorf("session_name answered %s, so the next call ran on the connection that rename_session changed", got)
				}
			})
			var closed, ledger int
			var note string
			if err := d.Connect(t).QueryRow(t.Context(), "SELECT (SELECT count(*) FROM tickets WHERE status = 'closed'), (SELECT note FROM tickets WHERE id = 1), (SELECT count(*) FROM ledger)").Scan(&closed, &note, &ledger); err != nil {
				t.Fatal(err)
			}
			if closed != 4 || note != "printer" || ledger != 2 {
				t.Errorf("helpdesk holds %d closed tickets, the note %q and %d ledger rows, want 4, printer and 2, so a refused call changed it or a passing one did not", closed, note, ledger)
			}
		})
	}
}

func TestCatalogToolsInStandaloneMode(t *testing.T) {
	files := map[string]string{
		"helpdesk/tickets.sql": "-- name: open_tickets\nSELECT id FROM tickets WHERE status = 'open' ORDER BY id\n",
		"helpdesk/tools.yaml":  "version: 1\ntools:\n  open_tickets:\n    description: Lists the open tickets. Answers one row per ticket.\n    mode: read\n    databases: [helpdesk]\n    login: read\n",
	}
	a, b := hashOf(tokenA), hashOf(tokenB)
	access := "version: 1\ncallers:\n" +
		"  - name: support_bot\n    token_sha256: \"" + hex.EncodeToString(a[:]) + "\"\n    tools: [open_tickets, open_ticket, execute_sql, read_rows, open_ticket]\n    databases: [helpdesk]\n    tables: [public.tickets]\n" +
		"  - name: ops_bot\n    token_sha256: \"" + hex.EncodeToString(b[:]) + "\"\n    tools: [read_rows]\n    databases: [helpdesk]\n"
	want := []Finding{
		reports("support_bot lists open_ticket in its tools in access.yaml, and adaptive-pg-mcp serves no built-in tool or catalog tool of that name, so the entry gives it nothing. Correct the name, or remove it from the tools of support_bot."),
		reports("support_bot lists execute_sql in its tools in access.yaml, and free_sql read leaves execute_sql out, so the entry gives it nothing. Set free_sql to read-write to serve execute_sql, or remove it from the tools of support_bot."),
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, l, "CREATE TABLE tickets (id int PRIMARY KEY, status text NOT NULL)", "INSERT INTO tickets VALUES (1, 'open'), (2, 'closed'), (3, 'open')", "GRANT SELECT ON tickets TO {login}")
			cfg := loadCatalog(t, standaloneHead("read"), writeFiles(t, t.TempDir(), files), helpdeskSingle, access, map[string]string{"HELPDESK_DSN": l.DSN(t, d)})
			srv := New(cfg, slog.New(slog.DiscardHandler))
			got := srv.Check(t.Context())
			srv.Close()
			if !sameFindings(got, want) {
				t.Errorf("Check found:\n%swant:\n%s", lines(got), lines(want))
			}

			support := overStdio(t, srv, tokenA, "")
			if _, byName := toolsOf(t, support); byName["open_tickets"] == nil {
				t.Errorf("support_bot, whose entry lists open_tickets, does not see it in tools/list")
			}
			res := callTool(t, support, "open_tickets", map[string]any{"database": "helpdesk"}, nil)
			if res.IsError || rowsJSON(t, structured(t, res)) != `[[1],[3]]` {
				t.Errorf("support_bot's open_tickets answered %v %s, want the rows 1 and 3", res.IsError, textOf(t, res))
			}

			ops := overStdio(t, srv, tokenB, "")
			if _, byName := toolsOf(t, ops); byName["open_tickets"] != nil {
				t.Errorf("ops_bot, whose entry does not list open_tickets, sees it in tools/list")
			}
			res = callTool(t, ops, "open_tickets", map[string]any{"database": "helpdesk"}, nil)
			refused := "ops_bot may not use open_tickets, because its entry in access.yaml does not list that tool, so the call is refused. Use a tool that its entry lists, or ask an administrator to add open_tickets to its tools."
			if got := textOf(t, res); !res.IsError || got != refused {
				t.Errorf("ops_bot's open_tickets answered %v:\n got %s\nwant %s", res.IsError, got, refused)
			}
		})
	}
}

func TestTheExampleCatalogPrepares(t *testing.T) {
	example, err := filepath.Abs(filepath.Join("..", "..", "examples", "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			reader := pgtest.NewLogin(t, s, loginPassword)
			writer := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, writer,
				"CREATE TABLE tickets (id int PRIMARY KEY, status text NOT NULL, opened_on date NOT NULL, updated_on date NOT NULL)",
				"GRANT SELECT, UPDATE ON tickets TO {login}")
			run(t, d, reader, "GRANT SELECT ON tickets TO {login}")
			env := map[string]string{"HELPDESK_READ_DSN": reader.DSN(t, d), "HELPDESK_WRITE_DSN": writer.DSN(t, d)}
			srv := New(loadCatalog(t, strazaHead("off"), example, separate, "", env), slog.New(slog.DiscardHandler))
			if got := srv.Check(t.Context()); len(got) > 0 {
				t.Errorf("the example catalog does not pass the start checks:\n%s", lines(got))
			}
			t.Cleanup(srv.Close)
			tools, byName := toolsOf(t, overMemory(t, srv, gatewayCaller, ""))
			lintTools(t, tools)
			if p := inputProp(byName, "tickets_by_status", "since"); p["type"] != "string" || p["format"] != "date" {
				t.Errorf("tickets_by_status takes since as %v, want a string of format date", p)
			}
			if p := inputProp(byName, "close_stale_tickets", "days"); p["type"] != "integer" || p["minimum"] != 7.0 || p["maximum"] != 90.0 {
				t.Errorf("close_stale_tickets takes days as %v, want an integer from 7 to 90", p)
			}
		})
	}
}
