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

// Finding is one result of the start checks.
type Finding struct {
	// Refuses is true when the finding stops serve and makes check fail.
	Refuses bool
	// Text is the message for the operator.
	Text string
}

// loginRef is one login of one database as the start checks see it.
type loginRef struct {
	// db is the config name of the database.
	db string
	// role is the key of the login under logins. A database with one login
	// has it as read.
	role string
	// roles are the logins of the database that share this login's pool.
	roles []string
	// label names the login in messages, such as "the login of sales_eu".
	label string
	login config.Login
}

// loginsOf lists the logins of a database in the order the checks run: the
// one login, or read, write and then the others by name.
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

// posture is what the start checks read about one login.
type posture struct {
	super, bypassRLS, createRole bool
	// superRole is a superuser role the login can switch to, or empty.
	superRole string
	// serverRoles are the roles for server files and programs that the login
	// is a member of.
	serverRoles []string
	// extensions are the installed extensions that reach other databases.
	extensions []string
	// tables counts the tables of the database, and writable those the login
	// can write to.
	tables, writable int64
}

// postureSQL reads the posture of the session's login. It reads
// session_user, not current_user, because a call can always return to the
// session's login with RESET ROLE. A table counts as writable when the login
// has USAGE on its schema and any write privilege on the table or one of its
// columns.
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

// Check opens a pool for every login of every database and runs the start
// checks of docs/DESIGN.md section 6: TLS first, then each database in name
// order, then the catalog, whose passing tools it serves. It returns the
// findings in that order and keeps the pools open until Close. A login that
// cannot connect refuses the start, because its checks cannot run.
func (s *Server) Check(ctx context.Context) []Finding {
	s.Close()
	s.pools = pg.NewPools()
	findings := s.checkTLS()
	ready := map[pg.Login]bool{}
	for _, name := range slices.Sorted(maps.Keys(s.cfg.Databases)) {
		fs, results := s.checkDatabase(ctx, name, s.cfg.Databases[name])
		findings = append(findings, fs...)
		for _, r := range results {
			for _, role := range r.ref.roles {
				ready[pg.Login{Database: name, Role: role}] = r.posture != nil
			}
		}
	}
	return append(findings, s.checkCatalog(ctx, ready)...)
}

// Close closes the pools that Check opened, if any. A call that runs
// afterwards gets no connection and returns a tool error.
func (s *Server) Close() {
	if s.pools != nil {
		s.pools.Close()
	}
}

// checkDatabase checks every login of one database, then the database. Every
// login must reach the same database as the read login, compared by id,
// because list_databases names the read login's database.
func (s *Server) checkDatabase(ctx context.Context, name string, db config.Database) ([]Finding, []loginResult) {
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
			findings = append(findings, Finding{Refuses: true, Text: r.ref.label + " reaches a different database than " + read.ref.label + ". Point " + r.ref.login.Env + " and " + read.ref.login.Env + " at the same database."})
		}
	}
	return findings, results
}

// idGrant is the fix for a login that cannot read the database id.
const idGrant = " Grant EXECUTE on pg_control_system() to the login, or restore its default grant to PUBLIC."

// unreadableID is the finding for a login that cannot read its database id.
// With one login it is a report, since there is nothing to compare. With
// more, it refuses the start, because the logins cannot be compared with the
// read login.
func unreadableID(ref loginRef, alone bool, reason string) Finding {
	cannot := ref.label + " cannot read the id of " + ref.db + ", because " + reason
	switch {
	case alone:
		return Finding{Text: cannot + ". list_databases fails, and so does every call that carries " + objectIDKey + "." + idGrant}
	case ref.role == "read":
		return Finding{Refuses: true, Text: cannot + ". Without it, adaptive-pg-mcp cannot check that every login of " + ref.db + " reaches the same database." + idGrant}
	}
	return Finding{Refuses: true, Text: cannot + ". Without it, adaptive-pg-mcp cannot check that this login reaches the same database as the read login." + idGrant}
}

// checkLogin connects one login and reads its posture and the database id.
// alone is true when the login is the only one of its database.
func (s *Server) checkLogin(ctx context.Context, ref loginRef, db config.Database, alone bool) loginResult {
	r := loginResult{ref: ref}
	logins := make([]pg.Login, len(ref.roles))
	for i, role := range ref.roles {
		logins[i] = pg.Login{Database: ref.db, Role: role}
	}
	if err := s.pools.Add(ref.login.DSN, logins...); err != nil {
		r.findings = append(r.findings, Finding{Refuses: true, Text: "the connection string in " + ref.login.Env + " for " + ref.label + " cannot be used, because " + err.Error() + ". Correct it in the server's environment."})
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
		r.findings = append(r.findings, Finding{Refuses: true, Text: ref.label + " cannot connect, because " + e.Reason + ". Check the connection string in " + ref.login.Env + ", and that Postgres is up and accepts the login."})
		return r
	}
	if err != nil {
		r.findings = append(r.findings, Finding{Refuses: true, Text: "the start checks cannot run on " + ref.label + ", because " + s.pools.Reason(err) + ". The login must be able to read pg_roles, pg_extension and pg_class, as every login can by default."})
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
		fs = append(fs, Finding{Refuses: true, Text: ref.label + " is a superuser and free SQL is on. Use a login without superuser, turn free_sql off, or set allow_superuser: true for " + ref.db + "."})
	case p.super && freeSQL:
		fs = append(fs, Finding{Text: ref.label + " is a superuser and free SQL is on, which allow_superuser: true accepts for " + ref.db + ". Its free SQL is not confined to " + ref.db + "."})
	case p.super:
		fs = append(fs, Finding{Text: ref.label + " is a superuser, so no grant restricts its calls. A login without superuser is held to the database's grants."})
	case p.superRole != "" && freeSQL && !db.AllowSuperuser:
		fs = append(fs, Finding{Refuses: true, Text: ref.label + " can switch to the superuser role " + p.superRole + " and free SQL is on. Use a login that is not a member of a superuser role, turn free_sql off, or set allow_superuser: true for " + ref.db + "."})
	case p.superRole != "" && freeSQL:
		fs = append(fs, Finding{Text: ref.label + " can switch to the superuser role " + p.superRole + " and free SQL is on, which allow_superuser: true accepts for " + ref.db + ". Its free SQL is not confined to " + ref.db + "."})
	}
	if p.bypassRLS && !p.super {
		fs = append(fs, Finding{Text: ref.label + " has BYPASSRLS, so row-level security policies do not apply to its calls. A login without BYPASSRLS keeps them in force."})
	}
	if p.createRole && !p.super && freeSQL {
		fs = append(fs, Finding{Text: ref.label + " has CREATEROLE and free SQL is on, so a call can create and change roles server-wide. Use a login without CREATEROLE."})
	}
	if len(p.serverRoles) > 0 && !p.super && freeSQL {
		fs = append(fs, Finding{Text: ref.label + " is a member of " + joinAnd(p.serverRoles) + ", and free SQL is on, so a call can reach files or programs on the database host. Use a login without that membership."})
	}
	if ref.role == "read" && p.writable > 0 {
		fs = append(fs, Finding{Text: writesWhereItReads(ref, p)})
	}
	return fs
}

// writesWhereItReads is the report for a read login that can also write.
func writesWhereItReads(ref loginRef, p posture) string {
	advice := " A separate read login with only SELECT adds Postgres grants as a second check."
	if len(ref.roles) == 1 {
		advice = " Granting it only SELECT adds Postgres grants as a second check."
	}
	switch {
	case p.tables == 1:
		return ref.label + " can write to its only table, so the read tools rely on read-only transactions alone." + advice
	case p.writable == p.tables:
		return ref.label + " can write to all " + count(p.tables) + " tables, so the read tools rely on read-only transactions alone." + advice
	default:
		return ref.label + " can write to " + count(p.writable) + " of its " + count(p.tables) + " tables, so on those tables the read tools rely on read-only transactions alone." + advice
	}
}

// crossDatabases reports dblink or postgres_fdw once per database that the
// logins reach. A login is skipped when its id was already reported. Once a
// finding stands, a login with an unknown id is skipped, and so is every
// login after a finding for an unknown id, since either may be the same
// database.
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
// and refuses the start unless allow_cross_database accepts them. The finding
// for the first login names the database, and for any other the login.
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
	return Finding{Refuses: true, Text: subject + ", which " + verb + " a call reach another database. Drop " + them + " from " + from + ", or set allow_cross_database: true for " + name + "."}
}

// exactlyOne scans the one row of rows into dest, and fails when there is
// no row or more than one.
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

// count writes n with a comma between each group of three digits, such as
// 1,240.
func count(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
