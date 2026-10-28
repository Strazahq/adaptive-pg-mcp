package core

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// loginTimeout bounds the connect and the checks of one login at start.
const loginTimeout = 30 * time.Second

// Finding is one line of what the start checks found.
type Finding struct {
	// Refuses is true when the finding stops serve from starting and makes check fail.
	Refuses bool
	// Text is the sentence for the operator.
	Text string
}

// loginRef is one login of one database, as the start checks run on it and as sentences name it.
type loginRef struct {
	// db is the config name of the database.
	db string
	// role is the login's name in logins. A database with one login holds it as read.
	role string
	// roles are the logins of the database that share this login's pool.
	roles []string
	// label names the login in a sentence, such as the login of sales_eu.
	label string
	// login is the login as config holds it.
	login config.Login
}

// loginsOf lists the logins of a database in the order the checks run: one
// login, or read, write and then the others by name.
func loginsOf(name string, db config.Database) []loginRef {
	read, write := db.Logins["read"], db.Logins["write"]
	if len(db.Logins) == 2 && read.Env == write.Env {
		return []loginRef{{db: name, role: "read", roles: []string{"read", "write"}, label: "the login of " + name, login: read}}
	}
	order := []string{"read", "write"}
	for _, role := range slices.Sorted(maps.Keys(db.Logins)) {
		if role != "read" && role != "write" {
			order = append(order, role)
		}
	}
	refs := make([]loginRef, 0, len(order))
	for _, role := range order {
		refs = append(refs, loginRef{db: name, role: role, roles: []string{role}, label: "the " + role + " login of " + name, login: db.Logins[role]})
	}
	return refs
}

// posture is what the start checks read about one login on one connection.
type posture struct {
	super, bypassRLS, createRole bool
	// superRole is a superuser role the login is a member of, and can switch to, or empty.
	superRole string
	// serverRoles are the roles the login is a member of that reach the server's programs or files.
	serverRoles []string
	// extensions are the extensions installed in the database that reach other databases.
	extensions []string
	// tables and writable count the tables of the database, and those the login can write to.
	tables, writable int64
}

// postureSQL reads the posture of the login of the connection. It reads the
// attributes of the session's own login, because a call can return to it
// from any role it switches to, and it counts a table as writable when the
// login may use its schema and holds any write privilege on it or on one of
// its columns.
const postureSQL = `SELECT s.rolsuper, s.rolbypassrls, s.rolcreaterole,
  coalesce((SELECT min(r.rolname::text) FROM pg_roles r
            WHERE r.rolsuper AND r.oid <> s.oid AND pg_has_role(s.oid, r.oid, 'MEMBER')), ''),
  ARRAY(SELECT r.rolname::text FROM pg_roles r
        WHERE r.rolname IN ('pg_execute_server_program', 'pg_read_server_files', 'pg_write_server_files')
          AND pg_has_role(s.oid, r.oid, 'MEMBER') ORDER BY 1),
  ARRAY(SELECT e.extname::text FROM pg_extension e WHERE e.extname IN ('dblink', 'postgres_fdw') ORDER BY 1),
  t.tables, t.writable
FROM pg_roles s,
  (SELECT count(*) AS tables,
          count(*) FILTER (WHERE has_schema_privilege(c.relnamespace, 'USAGE')
            AND (has_table_privilege(c.oid, 'INSERT, UPDATE, DELETE, TRUNCATE')
                 OR has_any_column_privilege(c.oid, 'INSERT, UPDATE'))) AS writable
   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE c.relkind IN ('r', 'p') AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema') t
WHERE s.rolname = session_user`

// loginResult is what the checks of one login found.
type loginResult struct {
	ref loginRef
	// posture is nil when the login could not connect or its checks could not run.
	posture *posture
	// id is the database id read on the login, or empty when it could not be read.
	id       string
	findings []Finding
}

// Check opens a pool for every login of every database, connects to each,
// and runs the start checks of docs/DESIGN.md section 6. It returns what they
// found in the order of the config, and keeps the pools open for serving
// until Close. A login that cannot connect refuses the start, because a check
// that cannot run is a check that never happened.
func (s *Server) Check(ctx context.Context) []Finding {
	s.Close()
	s.pools = pg.NewPools()
	var findings []Finding
	for _, name := range slices.Sorted(maps.Keys(s.cfg.Databases)) {
		findings = append(findings, s.checkDatabase(ctx, name, s.cfg.Databases[name])...)
	}
	return findings
}

// Close closes the pools that Check opened, if any. A call that still runs
// afterwards gets no connection and answers a tool error.
func (s *Server) Close() {
	if s.pools != nil {
		s.pools.Close()
	}
}

// checkDatabase checks every login of one database and then the database
// itself. Every login of a database with more than one must reach the
// database of its read login, by the id, or the start is refused, because
// list_databases names the read login's database.
func (s *Server) checkDatabase(ctx context.Context, name string, db config.Database) []Finding {
	var results []loginResult
	var findings []Finding
	refs := loginsOf(name, db)
	for _, ref := range refs {
		r := s.checkLogin(ctx, ref, db, len(refs) == 1)
		results = append(results, r)
		findings = append(findings, r.findings...)
	}
	findings = append(findings, crossDatabases(name, db, results)...)
	read := results[0]
	for _, r := range results[1:] {
		if read.id != "" && r.id != "" && r.id != read.id {
			findings = append(findings, Finding{Refuses: true, Text: r.ref.label + " reaches another database than " + read.ref.label + ", and its calls would change a database that list_databases does not name, so adaptive-pg-mcp will not start. Point the connection strings in " + r.ref.login.Env + " and " + read.ref.login.Env + " at the same database."})
		}
	}
	return findings
}

// idGrant is what restores the read of a database id to a login.
const idGrant = " Grant EXECUTE on the function pg_control_system() to the login, or restore the default grant to PUBLIC."

// unreadableID is the finding about a login whose checks ran but that cannot
// read the id of its database. A database with one login is reported,
// because nothing needs comparing. In a database with more than one, it
// refuses the start, because the logins cannot be compared with the read
// login, and a check that cannot run is a check that never happened.
func unreadableID(ref loginRef, alone bool, reason string) Finding {
	cannot := ref.label + " cannot read the id of " + ref.db + ", because " + reason
	switch {
	case alone:
		return Finding{Text: cannot + ", so list_databases fails and every call that carries an " + objectIDKey + " is refused on this login." + idGrant}
	case ref.role == "read":
		return Finding{Refuses: true, Text: cannot + ", and without it adaptive-pg-mcp cannot check that the other logins of " + ref.db + " reach the same database, so adaptive-pg-mcp will not start." + idGrant}
	}
	return Finding{Refuses: true, Text: cannot + ", and without it adaptive-pg-mcp cannot check that the login reaches the same database as the read login of " + ref.db + ", so adaptive-pg-mcp will not start." + idGrant}
}

// checkLogin connects to one login and reads its posture and the database
// id on it. alone is true when the login is the only one of its database.
func (s *Server) checkLogin(ctx context.Context, ref loginRef, db config.Database, alone bool) loginResult {
	r := loginResult{ref: ref}
	logins := make([]pg.Login, len(ref.roles))
	for i, role := range ref.roles {
		logins[i] = pg.Login{Database: ref.db, Role: role}
	}
	if err := s.pools.Add(ref.login.DSN, logins...); err != nil {
		r.findings = append(r.findings, Finding{Refuses: true, Text: "the connection string in " + ref.login.Env + " for " + ref.label + " cannot be used, because " + err.Error() + ", so adaptive-pg-mcp will not start. Correct it in the environment the server starts with. It is not repeated here, because it may hold a password."})
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	var p posture
	err := s.pools.Run(ctx, logins[0], pg.ReadOnly, s.cfg.Limits.Timeout, func(tx *pg.Tx) error {
		rows, err := tx.Query(ctx, postureSQL)
		if err != nil {
			return err
		}
		return exactlyOne(rows, &p.super, &p.bypassRLS, &p.createRole, &p.superRole, &p.serverRoles, &p.extensions, &p.tables, &p.writable)
	})
	if e := (*pg.Error)(nil); errors.As(err, &e) && e.Connect {
		r.findings = append(r.findings, Finding{Refuses: true, Text: ref.label + " cannot connect, because " + e.Reason + ", so adaptive-pg-mcp will not start. Check the connection string in " + ref.login.Env + ", and that the database server is up and accepts the login."})
		return r
	}
	if err != nil {
		r.findings = append(r.findings, Finding{Refuses: true, Text: "the start checks cannot run on " + ref.label + ", because " + s.pools.Reason(err) + ", so adaptive-pg-mcp will not start. Check that the login may read pg_roles, pg_extension and pg_class, as every login may by default."})
		return r
	}
	r.posture = &p
	r.findings = append(r.findings, s.loginFindings(ref, db, p)...)

	r.id, err = s.readIDOn(ctx, logins[0])
	if err != nil {
		r.findings = append(r.findings, unreadableID(ref, alone, s.pools.Reason(err)))
	}
	return r
}

// loginFindings turns the posture of one login into findings.
func (s *Server) loginFindings(ref loginRef, db config.Database, p posture) []Finding {
	var fs []Finding
	freeSQL := s.cfg.FreeSQL != config.FreeSQLOff
	switch {
	case p.super && freeSQL && !db.AllowSuperuser:
		fs = append(fs, Finding{Refuses: true, Text: ref.label + " is a superuser and free SQL is on, so adaptive-pg-mcp will not start. Use a login without superuser, turn free_sql off, or set allow_superuser: true for " + ref.db + "."})
	case p.super && freeSQL:
		fs = append(fs, Finding{Text: ref.label + " is a superuser and free SQL is on, which allow_superuser: true accepts for " + ref.db + ". A superuser's free SQL is not limited to the database, so a login without superuser is the safer setup."})
	case p.super:
		fs = append(fs, Finding{Text: ref.label + " is a superuser, so no grant of the database limits its calls. A login without superuser adds the database's own lock."})
	case p.superRole != "" && freeSQL && !db.AllowSuperuser:
		fs = append(fs, Finding{Refuses: true, Text: ref.label + " can switch to the superuser role " + p.superRole + " and free SQL is on, so adaptive-pg-mcp will not start. Use a login that is not a member of a superuser role, turn free_sql off, or set allow_superuser: true for " + ref.db + "."})
	case p.superRole != "" && freeSQL:
		fs = append(fs, Finding{Text: ref.label + " can switch to the superuser role " + p.superRole + " and free SQL is on, which allow_superuser: true accepts for " + ref.db + ". A superuser's free SQL is not limited to the database, so a login without that membership is the safer setup."})
	}
	if p.bypassRLS && !p.super {
		fs = append(fs, Finding{Text: ref.label + " has BYPASSRLS, so no row-level security policy limits what its calls read or change. A login without BYPASSRLS keeps the policies in force."})
	}
	if p.createRole && !p.super && freeSQL {
		fs = append(fs, Finding{Text: ref.label + " has CREATEROLE and free SQL is on, so a call can create and change roles of the whole database server, not only of " + ref.db + ". A login without CREATEROLE removes that reach."})
	}
	if len(p.serverRoles) > 0 && !p.super && freeSQL {
		fs = append(fs, Finding{Text: ref.label + " is a member of " + joinAnd(p.serverRoles) + ", and free SQL is on, so a call can reach the programs or files of the database server, not only " + ref.db + ". A login without that membership removes that reach."})
	}
	if ref.role == "read" && p.writable > 0 {
		fs = append(fs, Finding{Text: writesWhereItReads(ref, p)})
	}
	return fs
}

// writesWhereItReads is the report about a login whose reads run where it can also write.
func writesWhereItReads(ref loginRef, p posture) string {
	advice := " A separate read login with SELECT only adds the database's own lock."
	if len(ref.roles) == 1 {
		advice = " Granting it SELECT only adds the database's own lock."
	}
	switch {
	case p.tables == 1:
		return ref.label + " can write to its only table, so its read tools rely on read-only transactions alone." + advice
	case p.writable == p.tables:
		return ref.label + " can write to all " + count(p.tables) + " tables, so its read tools rely on read-only transactions alone." + advice
	default:
		return ref.label + " can write to " + count(p.writable) + " of its " + count(p.tables) + " tables, so on those its read tools rely on read-only transactions alone." + advice
	}
}

// crossDatabases checks every login whose posture was read for dblink or
// postgres_fdw, because every database a login reaches is one a call can
// touch. It says so once per database: a login whose database has the same
// id as one already named is skipped, and so is a login whose id is unknown
// once a finding stands, since it may reach that same database.
func crossDatabases(name string, db config.Database, results []loginResult) []Finding {
	var findings []Finding
	named := map[string]bool{}
	unknownNamed := false
	for i, r := range results {
		if r.posture == nil || len(r.posture.extensions) == 0 || named[r.id] {
			continue
		}
		if len(findings) > 0 && (r.id == "" || unknownNamed) {
			continue
		}
		findings = append(findings, crossDatabase(name, db, r, i == 0))
		named[r.id] = r.id != ""
		unknownNamed = unknownNamed || r.id == ""
	}
	return findings
}

// crossDatabase reports dblink or postgres_fdw on the database of one login,
// and refuses the start unless allow_cross_database accepts them. The first
// login of a database speaks for the database, and any other login is named.
func crossDatabase(name string, db config.Database, r loginResult, first bool) Finding {
	installed := joinAnd(r.posture.extensions) + " installed"
	subject, from := name+" has "+installed, name
	if !first {
		subject, from = r.ref.label+" reaches a database that has "+installed, "that database"
	}
	if db.AllowCrossDatabase {
		return Finding{Text: subject + ", which allow_cross_database: true accepts for " + name + ". A call can reach another database through it."}
	}
	verb, them := "lets", "the extension"
	if len(r.posture.extensions) > 1 {
		verb, them = "let", "the extensions"
	}
	return Finding{Refuses: true, Text: subject + ", which " + verb + " a call reach another database, so adaptive-pg-mcp will not start. Drop " + them + " from " + from + ", or set allow_cross_database: true for " + name + "."}
}

// exactlyOne scans the one row of rows into dest, and fails when there is no row or more than one.
func exactlyOne(rows pgx.Rows, dest ...any) error {
	_, err := pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (struct{}, error) {
		return struct{}{}, row.Scan(dest...)
	})
	return err
}

// joinAnd joins names as a, b and c.
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// count writes n with a comma between each group of three digits, such as 1,240.
func count(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
