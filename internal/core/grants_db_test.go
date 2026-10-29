//go:build pg

package core

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// grantsFixture is the SQL of the grant checks. Every trigger on notes
// raises, so a check that fired one would report it. secret_score is STRICT
// and only its owner may run it, must_be_set raises on NULL, hold_lock
// takes a session advisory lock each time it runs, and notify_plan sends a
// notification on the channel grant_check, which a commit delivers.
var grantsFixture = []string{
	"CREATE TABLE tickets (id int PRIMARY KEY, status text NOT NULL DEFAULT 'open', note text)",
	"INSERT INTO tickets (id) SELECT generate_series(1, 5)",
	"CREATE TABLE ledger (amount numeric)",
	"CREATE TABLE notes (id int PRIMARY KEY, body text)",
	"INSERT INTO notes VALUES (1, 'printer'), (2, 'screen')",
	"CREATE TABLE serials (id serial PRIMARY KEY, v int)",
	"CREATE FUNCTION refuse_trigger() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'a trigger on notes fired'; END$$",
	"CREATE TRIGGER notes_before_statement BEFORE INSERT OR UPDATE OR DELETE ON notes FOR EACH STATEMENT EXECUTE FUNCTION refuse_trigger()",
	"CREATE TRIGGER notes_before_row BEFORE INSERT OR UPDATE OR DELETE ON notes FOR EACH ROW EXECUTE FUNCTION refuse_trigger()",
	"CREATE TRIGGER notes_after_statement AFTER INSERT OR UPDATE OR DELETE ON notes FOR EACH STATEMENT EXECUTE FUNCTION refuse_trigger()",
	"CREATE TRIGGER notes_after_row AFTER INSERT OR UPDATE OR DELETE ON notes FOR EACH ROW EXECUTE FUNCTION refuse_trigger()",
	"CREATE FUNCTION secret_score(id int) RETURNS int LANGUAGE sql STRICT IMMUTABLE AS 'SELECT id * 2'",
	"REVOKE EXECUTE ON FUNCTION secret_score(int) FROM PUBLIC",
	"CREATE FUNCTION must_be_set(id int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$BEGIN IF id IS NULL THEN RAISE EXCEPTION 'the id must be set'; END IF; RETURN id; END$$",
	"CREATE FUNCTION hold_lock(id int) RETURNS int LANGUAGE plpgsql STABLE AS $$BEGIN PERFORM pg_advisory_lock(4242); RETURN id; END$$",
	"CREATE PROCEDURE close_month(month int) LANGUAGE plpgsql AS $$BEGIN UPDATE tickets SET status = 'closed' WHERE id <= month; END$$",
	"CREATE FUNCTION notify_plan(id int) RETURNS int LANGUAGE plpgsql STABLE AS $$BEGIN PERFORM pg_notify('grant_check', 'planned'); RETURN id; END$$",
}

// grantCase is one catalog entry of the grant checks and what its start check finds.
type grantCase struct {
	name, mode, login, sql string
	// params are the names of the statement's parameters in $1 order.
	params []string
	// since is the oldest Postgres major that takes the statement, or 0 for every one.
	since int
	// want is the finding about the entry, or the zero Finding when it passes without one.
	want Finding
}

// grantCases are the entries of the grant checks, whose findings come in the
// order of their names, refusals first. The read login may read
// the columns id and status of tickets only, and the write login may read
// tickets and serials, change notes, and insert into serials without USAGE
// on its sequence.
var grantCases = []grantCase{
	{name: "ticket_statuses", mode: "read", login: "read", params: []string{"status"},
		sql: "SELECT id, status FROM tickets WHERE status = $1 ORDER BY id"},
	{name: "ledger_total", mode: "read", login: "read",
		sql:  "SELECT sum(amount) AS total FROM ledger",
		want: refuses("helpdesk/tickets.sql statement ledger_total needs a grant that the read login of helpdesk lacks: Postgres said: permission denied for table ledger (SQLSTATE 42501). Grant it, or remove helpdesk from its databases.")},
	{name: "ticket_notes", mode: "read", login: "read",
		sql:  "SELECT id, note FROM tickets ORDER BY id",
		want: refuses("helpdesk/tickets.sql statement ticket_notes needs a grant that the read login of helpdesk lacks: Postgres said: permission denied for table tickets (SQLSTATE 42501). Grant it, or remove helpdesk from its databases.")},
	{name: "ticket_scores", mode: "read", login: "read",
		sql:  "SELECT id, secret_score(id) AS score FROM tickets ORDER BY id",
		want: refuses("helpdesk/tickets.sql statement ticket_scores needs a grant that the read login of helpdesk lacks: Postgres said: permission denied for function secret_score (SQLSTATE 42501). Grant it, or remove helpdesk from its databases.")},
	{name: "open_ticket", mode: "write", login: "write", params: []string{"id"},
		sql:  "INSERT INTO tickets (id) VALUES ($1)",
		want: refuses("helpdesk/tickets.sql statement open_ticket needs a grant that the write login of helpdesk lacks: Postgres said: permission denied for table tickets (SQLSTATE 42501). Grant it, or remove helpdesk from its databases.")},
	{name: "close_tickets", mode: "write", login: "write", params: []string{"up_to"},
		sql:  "UPDATE tickets SET status = 'closed' WHERE id <= $1",
		want: refuses("helpdesk/tickets.sql statement close_tickets needs a grant that the write login of helpdesk lacks: Postgres said: permission denied for table tickets (SQLSTATE 42501). Grant it, or remove helpdesk from its databases.")},
	{name: "drop_ticket", mode: "write", login: "write", params: []string{"id"},
		sql:  "DELETE FROM tickets WHERE id = $1",
		want: refuses("helpdesk/tickets.sql statement drop_ticket needs a grant that the write login of helpdesk lacks: Postgres said: permission denied for table tickets (SQLSTATE 42501). Grant it, or remove helpdesk from its databases.")},
	{name: "add_note", mode: "write", login: "write", params: []string{"id", "body"},
		sql: "INSERT INTO notes (id, body) VALUES ($1, $2)"},
	{name: "edit_note", mode: "write", login: "write", params: []string{"id", "body"},
		sql: "UPDATE notes SET body = $2 WHERE id = $1"},
	{name: "clear_notes", mode: "write", login: "write",
		sql: "DELETE FROM notes"},
	{name: "merge_note", mode: "write", login: "write", params: []string{"id", "body"}, since: 15,
		sql: "MERGE INTO notes USING (SELECT $1::int4 AS id, $2::text AS body) s ON notes.id = s.id WHEN MATCHED THEN UPDATE SET body = s.body WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.body)"},
	{name: "close_month", mode: "write", login: "write", params: []string{"month"},
		sql:  "CALL close_month($1)",
		want: reports("helpdesk/tickets.sql statement close_month cannot be planned by EXPLAIN, so its grants are not checked. The tool is served, and its first call fails if the write login of helpdesk lacks a grant. Check its grants by hand.")},
	{name: "tickets_after", mode: "read", login: "read", params: []string{"after"},
		sql:  "SELECT id FROM tickets WHERE id > must_be_set($1) ORDER BY id",
		want: reports("helpdesk/tickets.sql statement tickets_after does not plan on helpdesk with NULL parameters: Postgres said: the id must be set (SQLSTATE P0001). The tool is served, and its first call fails if the read login of helpdesk lacks a grant. Fix the statement if real calls fail the same way.")},
	{name: "locked_tickets", mode: "read", login: "read",
		sql: "SELECT id FROM tickets WHERE id = hold_lock(1)"},
	{name: "notified_tickets", mode: "read", login: "read",
		sql: "SELECT id FROM tickets WHERE id = notify_plan(1)"},
	{name: "add_serial", mode: "write", login: "write", params: []string{"v"},
		sql: "INSERT INTO serials (v) VALUES ($1)"},
	{name: "score_of", mode: "read", login: "read", params: []string{"id"},
		sql: "SELECT secret_score($1) AS score"},
}

// grantsCatalog writes the catalog of the cases that Postgres major takes, as helpdesk/tickets.sql and helpdesk/tools.yaml, and returns its folder.
func grantsCatalog(t *testing.T, major int) string {
	t.Helper()
	var sql, index strings.Builder
	index.WriteString("version: 1\ntools:\n")
	for _, c := range grantCases {
		if major < c.since {
			continue
		}
		sql.WriteString("-- name: " + c.name + "\n" + c.sql + "\n\n")
		index.WriteString("  " + c.name + ":\n    description: Runs the grant check case " + c.name + ". Returns its rows.\n    mode: " + c.mode + "\n    databases: [helpdesk]\n    login: " + c.login + "\n")
		if len(c.params) > 0 {
			index.WriteString("    params:\n")
		}
		for _, p := range c.params {
			index.WriteString("      - name: " + p + "\n        description: The value of " + p + ".\n        required: true\n")
		}
	}
	return writeFiles(t, t.TempDir(), map[string]string{"helpdesk/tickets.sql": sql.String(), "helpdesk/tools.yaml": index.String()})
}

// heldLocks counts the advisory locks that any session holds in the database of conn.
func heldLocks(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// rowsOf reads notes and serials and the state of the sequence of serials, as one text to compare before and after.
func rowsOf(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(t.Context(), "SELECT (SELECT string_agg(id || ':' || body, ',' ORDER BY id) FROM notes) || ' serials ' || (SELECT count(*) FROM serials) || ' sequence ' || (SELECT last_value || ' ' || is_called FROM serials_id_seq) || ' closed ' || (SELECT count(*) FROM tickets WHERE status = 'closed')").Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCatalogGrantChecks(t *testing.T) {
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			reader := pgtest.NewLogin(t, s, loginPassword)
			writer := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			run(t, d, writer, append(grantsFixture,
				"GRANT SELECT ON tickets, serials TO {login}",
				"GRANT SELECT, INSERT, UPDATE, DELETE ON notes TO {login}",
				"GRANT INSERT ON serials TO {login}")...)
			run(t, d, reader, "GRANT SELECT (id, status) ON tickets TO {login}")
			admin := d.Connect(t)
			before := rowsOf(t, admin)
			listen := d.Connect(t)
			if _, err := listen.Exec(t.Context(), "LISTEN grant_check"); err != nil {
				t.Fatal(err)
			}

			env := map[string]string{"HELPDESK_READ_DSN": reader.DSN(t, d), "HELPDESK_WRITE_DSN": writer.DSN(t, d)}
			srv := New(loadCatalog(t, strazaHead("off"), grantsCatalog(t, s.Major), separate, "", env), slog.New(slog.DiscardHandler))
			got := srv.Check(t.Context())
			t.Cleanup(srv.Close)

			byEntry := slices.SortedFunc(slices.Values(grantCases), func(a, b grantCase) int { return strings.Compare(a.name, b.name) })
			var want []Finding
			for _, refusal := range []bool{true, false} {
				for _, c := range byEntry {
					if s.Major >= c.since && c.want.Text != "" && c.want.Refuses == refusal {
						want = append(want, c.want)
					}
				}
			}
			if !sameFindings(got, want) {
				t.Errorf("Check found:\n%swant:\n%s", lines(got), lines(want))
			}

			deadline := time.Now().Add(5 * time.Second)
			for heldLocks(t, admin) > 0 {
				if time.Now().After(deadline) {
					t.Fatalf("a session still holds the advisory lock that hold_lock took while EXPLAIN planned locked_tickets, 5s after the check returned, so the check left a session-level effect on a pooled connection")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if after := rowsOf(t, admin); after != before {
				t.Errorf("the check changed helpdesk from %s to %s, where EXPLAIN without ANALYZE changes nothing", before, after)
			}
			wait, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if n, err := listen.WaitForNotification(wait); err == nil {
				t.Errorf("the notification %q of notify_plan reached a listener, so the check committed what its plan did, where it rolls back", n.Payload)
			}

			cs := overMemory(t, srv, gatewayCaller, "")
			_, byName := toolsOf(t, cs)
			for _, c := range grantCases {
				if s.Major < c.since {
					continue
				}
				if _, ok := byName[c.name]; ok == c.want.Refuses {
					t.Errorf("tools/list serves %s %v, and its start check refuses it %v", c.name, ok, c.want.Refuses)
				}
			}

			firstCalls := []struct {
				name, tool string
				args       map[string]any
				want       string
			}{
				{"USAGE on a sequence that a column default calls", "add_serial", map[string]any{"v": 1},
					"add_serial on helpdesk changed nothing, because Postgres said: permission denied for sequence serials_id_seq (SQLSTATE 42501). Correct the call and try again."},
				{"EXECUTE on a STRICT function that a NULL parameter folds away", "score_of", map[string]any{"id": 3},
					"score_of on helpdesk failed, because Postgres said: permission denied for function secret_score (SQLSTATE 42501). Correct the call and try again."},
			}
			for _, c := range firstCalls {
				t.Run("a grant the plan does not show: "+c.name, func(t *testing.T) {
					res := callTool(t, cs, c.tool, helpdesk(c.args), nil)
					if got := textOf(t, res); !res.IsError || got != c.want {
						t.Errorf("the first call of %s answered %v:\n got %s\nwant %s", c.tool, res.IsError, got, c.want)
					}
				})
			}
		})
	}
}

func TestCatalogPlanThatEnds(t *testing.T) {
	cases := []struct {
		name, entry, sql string
		// cancel is true when the test cancels the check's context once the plan runs.
		cancel bool
		// timeout is the limits.timeout of server.yaml, or empty for 30s.
		timeout string
		// holdOpen is SQL that another transaction runs and holds open through the check, or empty.
		holdOpen string
		want     string
	}{
		{name: "a backend terminated while it plans", entry: "ticket_count", sql: "SELECT count(*) AS n FROM tickets WHERE id > end_session(0)",
			want: "helpdesk/tickets.sql statement ticket_count could not be planned on helpdesk: Postgres said: terminating connection due to administrator command (SQLSTATE 57P01). The grants of the read login of helpdesk cannot be checked without a plan. Fix the cause, and try again."},
		{name: "a check cancelled while it plans", entry: "slow_count", sql: "SELECT count(*) AS n FROM tickets WHERE id > slow_plan(0)", cancel: true,
			want: "helpdesk/tickets.sql statement slow_count could not be planned on helpdesk: the call was cancelled before Postgres replied. The grants of the read login of helpdesk cannot be checked without a plan. Fix the cause, and try again."},
		{name: "a plan past its statement timeout", entry: "timed_count", sql: "SELECT count(*) AS n FROM tickets WHERE id > slow_plan(0)", timeout: "1s",
			want: "helpdesk/tickets.sql statement timed_count could not be planned on helpdesk: Postgres said: canceling statement due to statement timeout (SQLSTATE 57014). The grants of the read login of helpdesk cannot be checked without a plan. Fix the cause, and try again."},
		{name: "a plan that waits for a lock on an index", entry: "ticket_one", sql: "SELECT id FROM tickets WHERE id = 1", timeout: "1s", holdOpen: "REINDEX INDEX tickets_pkey",
			want: "helpdesk/tickets.sql statement ticket_one could not be planned on helpdesk: Postgres said: canceling statement due to statement timeout (SQLSTATE 57014). The grants of the read login of helpdesk cannot be checked without a plan. Fix the cause, and try again."},
	}
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					l := pgtest.NewLogin(t, s, loginPassword)
					d := pgtest.NewDatabase(t, s)
					run(t, d, l, "CREATE TABLE tickets (id int PRIMARY KEY)",
						"CREATE FUNCTION end_session(id int) RETURNS int LANGUAGE plpgsql STABLE AS $$BEGIN PERFORM pg_terminate_backend(pg_backend_pid()); PERFORM pg_sleep(1); RETURN id; END$$",
						"CREATE FUNCTION slow_plan(id int) RETURNS int LANGUAGE plpgsql STABLE AS $$BEGIN PERFORM pg_sleep(5); RETURN id; END$$",
						"GRANT SELECT ON tickets TO {login}")
					files := map[string]string{
						"helpdesk/tickets.sql": "-- name: " + c.entry + "\n" + c.sql + "\n",
						"helpdesk/tools.yaml":  "version: 1\ntools:\n  " + c.entry + ":\n    description: Counts the tickets. Returns one row.\n    mode: read\n    databases: [helpdesk]\n    login: read\n",
					}
					head := strazaHead("off")
					if c.timeout != "" {
						head = strings.Replace(head, "timeout: 30s", "timeout: "+c.timeout, 1)
					}
					if c.holdOpen != "" {
						tx, err := d.Connect(t).Begin(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback(context.Background())
						if _, err := tx.Exec(t.Context(), c.holdOpen); err != nil {
							t.Fatalf("%s: %v", c.holdOpen, err)
						}
					}
					srv := New(loadCatalog(t, head, writeFiles(t, t.TempDir(), files), helpdeskSingle, "", map[string]string{"HELPDESK_DSN": l.DSN(t, d)}), slog.New(slog.DiscardHandler))
					t.Cleanup(srv.Close)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					if c.cancel {
						watch := d.Connect(t)
						go func() {
							defer cancel()
							for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
								var n int
								if err := watch.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND state = 'active' AND query LIKE 'EXPLAIN (ANALYZE false) %'").Scan(&n); err != nil || n > 0 {
									return
								}
							}
						}()
					}
					want := []Finding{refuses(c.want)}
					if got := srv.Check(ctx); !sameFindings(got, want) {
						t.Errorf("Check found:\n%swant:\n%s", lines(got), lines(want))
					}
				})
			}
		})
	}
}
