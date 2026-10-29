package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// testDatabases are the databases of server.yaml that the load tests check entries against.
var testDatabases = map[string]config.Database{
	"helpdesk": {Logins: map[string]config.Login{"read": {Env: "HELPDESK_READ_DSN"}, "write": {Env: "HELPDESK_WRITE_DSN"}}},
	"sales_eu": {Logins: map[string]config.Login{"read": {Env: "SALES_EU_DSN"}, "write": {Env: "SALES_EU_DSN"}}},
	"sales_us": {Logins: map[string]config.Login{"read": {Env: "SALES_US_READ_DSN"}, "write": {Env: "SALES_US_WRITE_DSN"}, "finance_reader": {Env: "SALES_US_FINANCE_DSN"}}},
}

// testBuiltins stand for the names of the built-in tools, which no entry may take.
var testBuiltins = []string{"list_databases", "query_sql"}

// writeTree writes each file of files under dir, with its folders, and returns dir.
func writeTree(t *testing.T, dir string, files map[string]string) string {
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

func ptr[T any](v T) *T { return &v }

func TestLoadReadsTheCatalog(t *testing.T) {
	dir := writeTree(t, t.TempDir(), map[string]string{
		"notes.md":         "The catalog of the example organization.\n",
		"shared.sql":       "\ufeff-- name: ping\nSELECT 1\n",
		"tools.yaml":       "version: 1\ntools:\n  ping:\n    description: Answers one row with the number 1.\n    mode: read\n    databases: [helpdesk]\n    login: read\n",
		"drafts/ideas.sql": "-- name: churn_guess\nSELECT 1\n",
		"finance/revenue.sql": `-- Revenue statements of the finance team.
/* A header in a block comment,
   /* with a nested one */ that ends here. */

-- name: revenue_by_region
SELECT region, sum(net) AS net
FROM revenue
WHERE month = $1 AND ($2::text IS NULL OR region = $2)
GROUP BY region;

-- name: revenue_draft
SELECT 1;
`,
		"finance/margin.sql": "-- name: margin_by_region\r\nSELECT region, sum(margin) FROM margins GROUP BY region\r\n",
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
      - name: region
        description: One region, or none for all.
        enum: [north, south]
        pattern: "^[a-z]+$"
        maxLength: 10
    limits: { rows: 100, timeout: 5s }
  margin_by_region:
    description: Sums the margin per region. One row per region.
    mode: read
    databases: [sales_us]
    login: finance_reader
`,
		"helpdesk/tickets/close.sql": "-- name: close_stale_tickets\nUPDATE tickets SET status = 'closed'\nWHERE status = 'pending' AND updated_on < current_date - $1::int4\n",
		"helpdesk/tickets/tools.yaml": `version: 1
tools:
  close_stale_tickets:
    description: Closes the tickets that waited in pending for more than a number of days.
    mode: write
    databases: [helpdesk]
    login: write
    params:
      - name: days
        description: The days a ticket waits in pending before it closes, from 7 to 90.
        required: true
        minimum: 7
        maximum: 90
    limits: { write_rows: 50, bytes: 2048 }
`,
	})
	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	entries, reports, err := Load(dir, testDatabases, testBuiltins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	linked, linkedReports, err := Load(link, testDatabases, testBuiltins)
	if err != nil || !reflect.DeepEqual(linked, entries) || !reflect.DeepEqual(linkedReports, reports) {
		t.Errorf("Load through a symbolic link to the folder answered %d entries and %d reports (%v), want what the folder itself answers", len(linked), len(linkedReports), err)
	}
	want := []Entry{
		{Name: "ping", Description: "Answers one row with the number 1.", Mode: Read, Databases: []string{"helpdesk"}, Login: "read", SQL: "SELECT 1", Source: "shared.sql"},
		{Name: "margin_by_region", Description: "Sums the margin per region. One row per region.", Mode: Read, Databases: []string{"sales_us"}, Login: "finance_reader",
			SQL: "SELECT region, sum(margin) FROM margins GROUP BY region", Source: "finance/margin.sql"},
		{Name: "revenue_by_region", Description: "Sums net revenue per region for one month. One row per region.", Mode: Read, Databases: []string{"sales_eu", "sales_us"}, Login: "read",
			SQL: "SELECT region, sum(net) AS net\nFROM revenue\nWHERE month = $1 AND ($2::text IS NULL OR region = $2)\nGROUP BY region;",
			Params: []Param{
				{Name: "month", Description: "The first day of the month, such as 2026-09-01.", Required: true},
				{Name: "region", Description: "One region, or none for all.", Enum: []string{"north", "south"}, Pattern: "^[a-z]+$", MaxLength: ptr(10)},
			},
			Limits: &Limits{Rows: 100, Timeout: 5 * time.Second}, Source: "finance/revenue.sql"},
		{Name: "close_stale_tickets", Description: "Closes the tickets that waited in pending for more than a number of days.", Mode: Write, Databases: []string{"helpdesk"}, Login: "write",
			SQL:    "UPDATE tickets SET status = 'closed'\nWHERE status = 'pending' AND updated_on < current_date - $1::int4",
			Params: []Param{{Name: "days", Description: "The days a ticket waits in pending before it closes, from 7 to 90.", Minimum: ptr(7.0), Maximum: ptr(90.0), Required: true}},
			Limits: &Limits{WriteRows: 50, Bytes: 2048}, Source: "helpdesk/tickets/close.sql"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("Load answered the entries\n%#v\nwant\n%#v", entries, want)
	}
	wantReports := []string{
		"drafts/ideas.sql statement churn_guess is not a tool, because drafts/tools.yaml has no entry for it. Add an entry to serve it.",
		"finance/revenue.sql statement revenue_draft is not a tool, because finance/tools.yaml has no entry for it. Add an entry to serve it.",
	}
	if !reflect.DeepEqual(reports, wantReports) {
		t.Errorf("Load reported\n%s\nwant\n%s", strings.Join(reports, "\n"), strings.Join(wantReports, "\n"))
	}
}

// okIndex is a finance/tools.yaml whose one entry joins the statement of okSQL.
const okIndex = `version: 1
tools:
  revenue_by_region:
    description: Sums net revenue per region for one month.
    mode: read
    databases: [sales_eu]
    login: read
    params:
      - name: month
        description: The first day of the month.
`

// okSQL is a finance/revenue.sql with the one statement of okIndex.
const okSQL = "-- name: revenue_by_region\nSELECT $1::date\n"

// edit returns text with old replaced by new once, and fails the test when text does not hold old.
func edit(t *testing.T, text, old, new string) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("the fixture does not hold %q", old)
	}
	return strings.Replace(text, old, new, 1)
}

func TestLoadFailures(t *testing.T) {
	const month = "  revenue_by_month:\n    description: Sums net revenue for one month.\n    mode: read\n    databases: [sales_eu]\n    login: read\n"
	noStatement := "finance/tools.yaml entry revenue_by_month has no statement of that name in the .sql files of its folder, so it cannot be a tool. Add a -- name: revenue_by_month line before its statement in a .sql file next to finance/tools.yaml, or remove the entry."
	at := "finance/tools.yaml entry revenue_by_region"
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"an entry without a statement", map[string]string{"finance/tools.yaml": okIndex + month}, noStatement},
		{"an entry whose statement is in a folder below", map[string]string{"finance/tools.yaml": okIndex + month, "finance/q3/month.sql": "-- name: revenue_by_month\nSELECT 1\n"}, noStatement},
		{"two statements with one name in two files", map[string]string{"sales/revenue.sql": "-- A copy.\n\n-- name: revenue_by_region\nSELECT 2\n"},
			"finance/revenue.sql line 1 and sales/revenue.sql line 3 both name a statement revenue_by_region, and a tool name must be unique in the catalog. Rename one of them in its -- name: line."},
		{"two statements with one name in one file", map[string]string{"finance/revenue.sql": okSQL + "\n-- name: revenue_by_region\nSELECT 2\n"},
			"finance/revenue.sql line 1 and finance/revenue.sql line 4 both name a statement revenue_by_region, and a tool name must be unique in the catalog. Rename one of them in its -- name: line."},
		{"two statements with one name, one of them not indexed", map[string]string{"drafts/old.sql": "-- name: revenue_by_region\nSELECT 3\n"},
			"drafts/old.sql line 1 and finance/revenue.sql line 1 both name a statement revenue_by_region, and a tool name must be unique in the catalog. Rename one of them in its -- name: line."},
		{"the name of a built-in tool", map[string]string{
			"finance/revenue.sql": okSQL + "\n-- name: query_sql\nSELECT 1\n",
			"finance/tools.yaml":  okIndex + "  query_sql:\n    description: Runs SQL.\n    mode: read\n    databases: [sales_eu]\n    login: read\n",
		}, "finance/tools.yaml entry query_sql has the name of a built-in tool of adaptive-pg-mcp, and a tool name must be unique on the server. Rename the entry and its -- name: line in finance/revenue.sql."},
		{"a database that server.yaml does not define", map[string]string{"finance/tools.yaml": edit(t, okIndex, "[sales_eu]", "[sales_eu, sales_apac]")},
			at + " names the database sales_apac, which server.yaml does not define. Remove sales_apac from its databases, or add it to databases in server.yaml."},
		{"a login that one of its databases does not have", map[string]string{"finance/tools.yaml": edit(t, edit(t, okIndex, "[sales_eu]", "[sales_us, sales_eu]"), "login: read", "login: finance_reader")},
			at + " runs on the login finance_reader, which sales_eu does not have in server.yaml. Add finance_reader to the logins of sales_eu, or set login to a login it has, such as read."},
		{"no login", map[string]string{"finance/tools.yaml": edit(t, okIndex, "    login: read\n", "")},
			at + " does not set login, which names the login its statement runs on. Add login: read, login: write, or the name of another login of its databases."},
		{"no mode", map[string]string{"finance/tools.yaml": edit(t, okIndex, "    mode: read\n", "")},
			at + " does not set mode. Set mode to read or write."},
		{"a mode that is neither read nor write", map[string]string{"finance/tools.yaml": edit(t, okIndex, "mode: read", "mode: delete")},
			at + " has mode delete, which is neither read nor write. Set mode to read or write."},
		{"no databases", map[string]string{"finance/tools.yaml": edit(t, okIndex, "[sales_eu]", "[]")},
			at + " names no databases. List the databases it runs on, by their names in server.yaml."},
		{"an entry without keys", map[string]string{"finance/tools.yaml": "version: 1\ntools:\n  revenue_by_region:\n"},
			at + " does not set mode. Set mode to read or write."},
		{"a row limit below 1", map[string]string{"finance/tools.yaml": okIndex + "    limits: { rows: 0 }\n"},
			at + " has limits.rows 0, and it must be 1 or more. Set it to 1 or more, or leave it out to keep the limit of server.yaml."},
		{"a write limit below 1", map[string]string{"finance/tools.yaml": okIndex + "    limits: { write_rows: -5 }\n"},
			at + " has limits.write_rows -5, and it must be 1 or more. Set it to 1 or more, or leave it out to keep the limit of server.yaml."},
		{"a timeout that is not a duration", map[string]string{"finance/tools.yaml": okIndex + "    limits: { timeout: soon }\n"},
			at + " has limits.timeout soon, which is not a duration. Write it with a unit, such as 30s."},
		{"a timeout of zero", map[string]string{"finance/tools.yaml": okIndex + "    limits: { timeout: 0s }\n"},
			at + " has limits.timeout 0s, and it must be longer than zero. Set it to the statement timeout of each call, such as 30s, or leave it out to keep the timeout of server.yaml."},
		{"SQL before the first name line", map[string]string{"finance/revenue.sql": "-- Revenue.\n\nSELECT 1;\n" + okSQL},
			"finance/revenue.sql line 3 holds SQL before the first -- name: line, so it belongs to no tool. Put a -- name: line before it, or remove it."},
		{"SQL in a file without a name line", map[string]string{"finance/notes.sql": "/* notes */ SELECT 1;\n"},
			"finance/notes.sql line 1 holds SQL before the first -- name: line, so it belongs to no tool. Put a -- name: line before it, or remove it."},
		{"a block comment that never closes before the first name line", map[string]string{"finance/revenue.sql": "/* draft\n" + okSQL},
			"finance/revenue.sql line 1 holds SQL before the first -- name: line, so it belongs to no tool. Put a -- name: line before it, or remove it."},
		{"a name line without a name", map[string]string{"finance/revenue.sql": okSQL + "-- name:\nSELECT 2\n"},
			"finance/revenue.sql line 3 has a -- name: line without a name. Write the tool name after it, such as -- name: revenue_by_region."},
		{"a statement without SQL", map[string]string{"finance/revenue.sql": "-- name: revenue_by_region\n-- only a comment\n/* and another */\n\n"},
			"finance/revenue.sql statement revenue_by_region on line 1 holds no SQL, only comments or blank lines. Write the statement below its -- name: line, or remove the line."},
		{"a statement that starts with a semicolon after a comment", map[string]string{"finance/revenue.sql": "-- name: revenue_by_region\n-- the monthly sum\n /* of net */ ;SELECT region FROM revenue\n"},
			"finance/revenue.sql statement revenue_by_region on line 1 starts with a semicolon, which would cut the statement off from the EXPLAIN that the start checks put before it to check its grants. Remove the semicolon at the start of the statement."},
		{"a statement that starts with a semicolon after a comment that ends in a carriage return", map[string]string{"finance/revenue.sql": "-- name: revenue_by_region\n-- the monthly sum\r;SELECT region FROM revenue\n"},
			"finance/revenue.sql statement revenue_by_region on line 1 starts with a semicolon, which would cut the statement off from the EXPLAIN that the start checks put before it to check its grants. Remove the semicolon at the start of the statement."},
		{"an unknown key in an entry", map[string]string{"finance/tools.yaml": edit(t, okIndex, "    login: read\n", "    login: read\n    owner: finance\n")},
			"finance/tools.yaml line 8 has the key owner, which this build does not read. Remove it, or correct its spelling."},
		{"an unknown key in a parameter", map[string]string{"finance/tools.yaml": okIndex + "        default: 2026-09-01\n"},
			"finance/tools.yaml line 11 has the key default, which this build does not read. Remove it, or correct its spelling."},
		{"an unknown key in the limits", map[string]string{"finance/tools.yaml": okIndex + "    limits: { rows: 10, cap: 5 }\n"},
			"finance/tools.yaml line 11 has the key cap, which this build does not read. Remove it, or correct its spelling."},
		{"a minimum that is not a number", map[string]string{"finance/tools.yaml": okIndex + "        minimum: low\n"},
			"finance/tools.yaml line 11 has a value of the wrong type, where a number belongs. Write a number there."},
		{"an entry written twice", map[string]string{"finance/tools.yaml": okIndex + "  revenue_by_region:\n    mode: read\n"},
			"finance/tools.yaml line 11 sets the key revenue_by_region a second time in one place. Keep one of the two."},
		{"another version", map[string]string{"finance/tools.yaml": edit(t, okIndex, "version: 1", "version: 2")},
			"finance/tools.yaml has version 2, and this build reads version 1 only. Write the file in version 1, or run a build that reads version 2."},
		{"a second document", map[string]string{"finance/tools.yaml": okIndex + "---\nversion: 1\n"},
			"finance/tools.yaml holds a second YAML document from line 11, which adaptive-pg-mcp would ignore, because it reads one document per file. Keep one document per file."},
		{"no version", map[string]string{"finance/tools.yaml": edit(t, okIndex, "version: 1\n", "")},
			"finance/tools.yaml does not set version, which names the format the file is written in. Add version: 1."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"finance/revenue.sql": okSQL, "finance/tools.yaml": okIndex}
			for name, text := range c.files {
				files[name] = text
			}
			entries, _, err := Load(writeTree(t, t.TempDir(), files), testDatabases, testBuiltins)
			if err == nil {
				t.Fatalf("Load answered %d entries, want the refusal: %s", len(entries), c.want)
			}
			if err.Error() != c.want {
				t.Errorf("the refusal:\n got %s\nwant %s", err, c.want)
			}
		})
	}
}

func TestLoadRefusesAFolderItCannotRead(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	_, statErr := os.Stat(missing)
	file := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(file, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, dir, want string }{
		{"a folder that does not exist", missing, "adaptive-pg-mcp cannot read the catalog folder at " + missing + ", which server.yaml names in catalog: " + statErr.Error() + ". Create the folder, correct the path in catalog, or set catalog: none."},
		{"a file instead of a folder", file, "the catalog folder at " + file + ", which server.yaml names in catalog, is a file and not a folder. Point catalog at the folder that holds the .sql files and their tools.yaml, or set catalog: none."},
	}
	// readErr is the error of reading rel in the folder root, as the loader reads it.
	readErr := func(root, rel string) string {
		r, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		_, err = r.ReadFile(filepath.FromSlash(rel))
		if err == nil {
			t.Fatalf("%s reads in %s, want an error", rel, root)
		}
		return err.Error()
	}
	outside := writeTree(t, filepath.Join(dir, "outside"), map[string]string{"revenue.sql": okSQL})
	linked := writeTree(t, filepath.Join(dir, "linked"), map[string]string{"finance/tools.yaml": okIndex})
	if err := os.Symlink(filepath.Join(outside, "revenue.sql"), filepath.Join(linked, "finance", "revenue.sql")); err != nil {
		t.Fatal(err)
	}
	absolute := func(rel, dir string) string {
		return rel + " is a link to an absolute path, and adaptive-pg-mcp follows only relative links that stay inside the catalog folder " + dir + ". Make it a relative link to a path inside the folder, or remove it."
	}
	climbs := func(rel, dir string) string {
		return rel + " is a link whose path climbs above the catalog folder " + dir + ", and adaptive-pg-mcp follows only relative links that stay inside it. Make it a relative link to a path inside the folder, or remove it."
	}
	through := func(rel, dir string) string {
		return rel + " is a link that leads through another link to an absolute path or above the catalog folder " + dir + ", and adaptive-pg-mcp follows only relative links that stay inside it. Make each link on the way relative and inside the folder, or remove it."
	}
	cases = append(cases, struct{ name, dir, want string }{"an absolute link to a file outside the folder", linked, absolute("finance/revenue.sql", linked)})
	inner := writeTree(t, filepath.Join(dir, "inner"), map[string]string{"finance/tools.yaml": okIndex, "shared/revenue.sql": okSQL})
	links(t, inner, map[string]string{"finance/revenue.sql": filepath.Join(inner, "shared", "revenue.sql")})
	cases = append(cases, struct{ name, dir, want string }{"an absolute link to a file inside the folder", inner, absolute("finance/revenue.sql", inner)})
	away := writeTree(t, filepath.Join(dir, "away"), map[string]string{"finance/tools.yaml": okIndex})
	links(t, away, map[string]string{"finance/revenue.sql": "../../outside/revenue.sql"})
	cases = append(cases, struct{ name, dir, want string }{"a relative link that climbs out of the folder", away, climbs("finance/revenue.sql", away)})
	back := writeTree(t, filepath.Join(dir, "back"), map[string]string{"finance/tools.yaml": okIndex, "shared/revenue.sql": okSQL})
	links(t, back, map[string]string{"finance/revenue.sql": "../../back/shared/revenue.sql"})
	cases = append(cases, struct{ name, dir, want string }{"a relative link that climbs out of the folder and back in", back, climbs("finance/revenue.sql", back)})
	chain := writeTree(t, filepath.Join(dir, "chain"), map[string]string{"finance/tools.yaml": okIndex, "store/revenue.sql": okSQL})
	links(t, chain, map[string]string{"finance/revenue.sql": "../shared/revenue.sql", "shared/revenue.sql": filepath.Join(chain, "store", "revenue.sql")})
	cases = append(cases, struct{ name, dir, want string }{"a relative link through an absolute link", chain, through("finance/revenue.sql", chain)})
	folderLinked := writeTree(t, filepath.Join(dir, "folder-linked"), map[string]string{"helpdesk/tools.yaml": "version: 1\ntools: {}\n"})
	if err := os.Symlink(outside, filepath.Join(folderLinked, "finance")); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, struct{ name, dir, want string }{"an absolute link to a folder outside the folder", folderLinked, absolute("finance", folderLinked)})
	broken := writeTree(t, filepath.Join(dir, "broken"), map[string]string{"finance/tools.yaml": okIndex})
	brokenLink := filepath.Join(broken, "finance", "revenue.sql")
	if err := os.Symlink("gone.sql", brokenLink); err != nil {
		t.Fatal(err)
	}
	_, brokenErr := filepath.EvalSymlinks(brokenLink)
	cases = append(cases, struct{ name, dir, want string }{"a link to nothing", broken,
		"adaptive-pg-mcp cannot read finance/revenue.sql in the catalog folder " + broken + ": " + brokenErr.Error() + ". Make it a file inside the catalog folder that the user who runs adaptive-pg-mcp can read, or remove it."})
	if os.Getuid() != 0 {
		locked := writeTree(t, filepath.Join(dir, "locked"), map[string]string{"finance/revenue.sql": okSQL, "finance/tools.yaml": okIndex})
		if err := os.Chmod(filepath.Join(locked, "finance", "revenue.sql"), 0); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, struct{ name, dir, want string }{"a file that cannot be read", locked,
			"adaptive-pg-mcp cannot read finance/revenue.sql in the catalog folder " + locked + ": " + readErr(locked, "finance/revenue.sql") + ". Make it a file inside the catalog folder that the user who runs adaptive-pg-mcp can read, or remove it."})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Load(c.dir, testDatabases, testBuiltins)
			if err == nil || err.Error() != c.want {
				t.Errorf("the refusal:\n got %v\nwant %s", err, c.want)
			}
		})
	}
	t.Run("an absolute link inside a catalog folder named by a relative path", func(t *testing.T) {
		t.Chdir(dir)
		_, _, err := Load("inner", testDatabases, testBuiltins)
		if want := absolute("finance/revenue.sql", "inner"); err == nil || err.Error() != want {
			t.Errorf("the refusal:\n got %v\nwant %s", err, want)
		}
	})
}

// links makes each link of links, by its path relative to dir, point at its target, which is relative to the link's own folder.
func links(t *testing.T, dir string, links map[string]string) {
	t.Helper()
	for link, target := range links {
		path := filepath.Join(dir, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.FromSlash(target), path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadSkipsHiddenNamesAndFollowsLinksInside(t *testing.T) {
	const ticketsSQL = "-- name: open_tickets\nSELECT id FROM tickets WHERE status = 'open'\n"
	const ticketsIndex = "version: 1\ntools:\n  open_tickets:\n    description: Lists the open tickets.\n    mode: read\n    databases: [helpdesk]\n    login: read\n"
	stamp := "..2026_10_28_12_00_00.000000001"
	cases := []struct {
		name  string
		files map[string]string
		links map[string]string
		// sources are the Source of each entry Load answers, in order.
		sources []string
	}{
		{"the layout of a Kubernetes ConfigMap mount",
			map[string]string{stamp + "/tickets.sql": ticketsSQL, stamp + "/tools.yaml": ticketsIndex},
			map[string]string{"..data": stamp, "tickets.sql": "..data/tickets.sql", "tools.yaml": "..data/tools.yaml"},
			[]string{"tickets.sql"}},
		{"a linked folder inside the catalog folder, read once",
			map[string]string{"shared/finance/revenue.sql": okSQL, "shared/finance/tools.yaml": okIndex},
			map[string]string{"finance": "shared/finance"},
			[]string{"finance/revenue.sql"}},
		{"a link to a folder above it, read once",
			map[string]string{"finance/revenue.sql": okSQL, "finance/tools.yaml": okIndex},
			map[string]string{"finance/again": ".."},
			[]string{"finance/revenue.sql"}},
		{"hidden files and folders at any depth",
			map[string]string{
				"finance/revenue.sql": okSQL, "finance/tools.yaml": okIndex,
				".git/hooks/revenue.sql": okSQL, "finance/.draft.sql": "SELECT 1;\n", "finance/.tools.yaml": "not: an index\n",
				".hidden/tools.yaml": ticketsIndex,
			},
			nil,
			[]string{"finance/revenue.sql"}},
		{"links the walk does not read, even ones that lead outside or to nothing",
			map[string]string{"finance/revenue.sql": okSQL, "finance/tools.yaml": okIndex, "notes.txt": "x\n"},
			map[string]string{"LICENSE": "../LICENSE", "README.md": "gone.md", "finance/notes.md": "/etc/hostname", "finance/docs": "gone", "copy.txt": "notes.txt"},
			[]string{"finance/revenue.sql"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeTree(t, t.TempDir(), c.files)
			links(t, dir, c.links)
			entries, reports, err := Load(dir, testDatabases, testBuiltins)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var sources []string
			for _, e := range entries {
				sources = append(sources, e.Source)
			}
			if !reflect.DeepEqual(sources, c.sources) || len(reports) > 0 {
				t.Errorf("Load answered entries from %v and the reports %v, want entries from %v and no report", sources, reports, c.sources)
			}
		})
	}
}
