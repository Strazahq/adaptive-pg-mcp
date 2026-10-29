// Package config loads server.yaml and access.yaml, and refuses unknown keys,
// missing required keys and values it cannot use.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// formatVersion is the only version of server.yaml and access.yaml this
// build reads.
const formatVersion = 1

// Mode says who authorizes each call.
type Mode string

const (
	// Standalone authorizes callers from access.yaml.
	Standalone Mode = "standalone"
	// Straza accepts only the tokens of the Straza gateway, which authorizes
	// each agent.
	Straza Mode = "straza"
)

// FreeSQL says which free SQL tools the server offers.
type FreeSQL string

const (
	// FreeSQLOff offers no free SQL.
	FreeSQLOff FreeSQL = "off"
	// FreeSQLRead offers query_sql, which runs in a read-only transaction.
	FreeSQLRead FreeSQL = "read"
	// FreeSQLReadWrite offers query_sql, execute_sql and execute_ddl.
	FreeSQLReadWrite FreeSQL = "read-write"
)

// Stdio is the listen value that serves one client over stdin and stdout.
const Stdio = "stdio"

// Config is a checked server.yaml, with the callers of access.yaml in
// standalone mode.
type Config struct {
	// Mode is standalone or straza.
	Mode Mode
	// Listen is stdio, or the host and port that Streamable HTTP listens on.
	Listen string
	// TLS is the certificate and key for HTTPS, or nil for plain HTTP.
	TLS *TLS
	// AllowPlainHTTP permits plain HTTP on a listen address beyond this
	// machine.
	AllowPlainHTTP bool
	// FreeSQL says which free SQL tools the server offers.
	FreeSQL FreeSQL
	// Limits are the caps on every call.
	Limits Limits
	// GatewayTokens are the SHA-256 hashes of the Straza tokens, in straza
	// mode only.
	GatewayTokens []Hash
	// Catalog is the catalog folder, resolved against the folder of
	// server.yaml, or empty for none.
	Catalog string
	// Databases maps config names to databases.
	Databases map[string]Database
	// Callers are the callers of access.yaml, in standalone mode only.
	Callers []Caller
}

// TLS is the tls key. Both paths are resolved against the folder of
// server.yaml.
type TLS struct {
	// CertFile is the PEM file of the certificate and any intermediates.
	CertFile string
	// KeyFile is the PEM file of the certificate's private key.
	KeyFile string
}

// Limits are the caps on every call.
type Limits struct {
	// Rows is the most rows a read returns.
	Rows int
	// Bytes is the most bytes a read returns.
	Bytes int
	// Timeout is the statement_timeout of each call.
	Timeout time.Duration
	// WriteRows is the most rows a write may change. A write over it rolls
	// back.
	WriteRows int
}

// Database is one configured database and its logins.
type Database struct {
	// Logins maps role names to logins. A single login appears as both
	// read and write.
	Logins map[string]Login
	// AllowCrossDatabase permits dblink or postgres_fdw on the database.
	AllowCrossDatabase bool
	// AllowSuperuser permits a superuser login while free SQL is on.
	AllowSuperuser bool
}

// Login is one Postgres login.
type Login struct {
	// Env is the environment variable that holds the connection string.
	Env string
	// DSN is the connection string read from Env.
	DSN DSN
}

// Hash is the SHA-256 of a token.
type Hash [sha256.Size]byte

// Caller is one caller of access.yaml.
type Caller struct {
	// Name is the caller's unique name.
	Name string
	// TokenSHA256 is the SHA-256 of the caller's token.
	TokenSHA256 Hash
	// Tools are the tool names the caller may use, as written.
	Tools []string
	// Databases are the config names of the databases the caller may use.
	Databases []string
	// Tables restricts the caller to these tables, as written. Empty means
	// no restriction.
	Tables []string
}

// rawServer is server.yaml as written. A nil field is a key the file does
// not set.
type rawServer struct {
	Version             *int                    `yaml:"version"`
	Mode                *string                 `yaml:"mode"`
	Listen              *string                 `yaml:"listen"`
	TLS                 *rawTLS                 `yaml:"tls"`
	AllowPlainHTTP      *bool                   `yaml:"allow_plain_http"`
	FreeSQL             *string                 `yaml:"free_sql"`
	Limits              *rawLimits              `yaml:"limits"`
	GatewayTokensSHA256 []string                `yaml:"gateway_tokens_sha256"`
	Catalog             *string                 `yaml:"catalog"`
	Access              *string                 `yaml:"access"`
	Databases           map[string]*rawDatabase `yaml:"databases"`
}

type rawTLS struct {
	CertFile *string `yaml:"cert_file"`
	KeyFile  *string `yaml:"key_file"`
}

type rawLimits struct {
	Rows      *int    `yaml:"rows"`
	Bytes     *int    `yaml:"bytes"`
	Timeout   *string `yaml:"timeout"`
	WriteRows *int    `yaml:"write_rows"`
}

type rawDatabase struct {
	Login              *rawLogin            `yaml:"login"`
	Logins             map[string]*rawLogin `yaml:"logins"`
	AllowCrossDatabase *bool                `yaml:"allow_cross_database"`
	AllowSuperuser     *bool                `yaml:"allow_superuser"`
}

type rawLogin struct {
	DSNEnv *string `yaml:"dsn_env"`
}

// hashRule matches a SHA-256 in lowercase hex.
var hashRule = regexp.MustCompile(`^[0-9a-f]{64}$`)

// envRule matches an environment variable name.
var envRule = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// required follows the key in every missing-key refusal.
const required = "a required key"

// Load reads and checks server.yaml at path and, in standalone mode, the
// access.yaml it names. It reads each dsn_env variable with getenv.
func Load(path string, getenv func(string) string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("adaptive-pg-mcp cannot read server.yaml at %s: %v. Pass its path with -config, or create the file.", path, err)
	}
	var raw rawServer
	if err := decode(path, data, &raw); err != nil {
		return nil, err
	}
	cfg, accessPath, err := check(path, &raw)
	if err != nil {
		return nil, err
	}
	if err := readLogins(cfg, getenv); err != nil {
		return nil, err
	}
	if cfg.Mode == Standalone {
		if cfg.Callers, err = loadAccess(accessPath, cfg.Databases); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// missing refuses an unset required key of server.yaml, with hint saying
// what to add.
func missing(path, key, hint string) error {
	return fmt.Errorf("%s does not set %s, %s. %s", path, key, required, hint)
}

// check turns server.yaml as written into a Config, and returns the path of
// access.yaml in standalone mode.
func check(path string, raw *rawServer) (*Config, string, error) {
	cfg := &Config{}
	switch {
	case unset(raw.Mode):
		return nil, "", missing(path, "mode", "Add mode: standalone, or mode: straza behind the Straza gateway.")
	case *raw.Mode != string(Standalone) && *raw.Mode != string(Straza):
		return nil, "", fmt.Errorf("%s has mode %s. Set mode to standalone, or to straza behind the Straza gateway.", path, *raw.Mode)
	}
	cfg.Mode = Mode(*raw.Mode)

	if unset(raw.Listen) {
		return nil, "", missing(path, "listen", "Add listen: stdio, or an address such as 127.0.0.1:8080.")
	}
	if host, _, err := net.SplitHostPort(*raw.Listen); err == nil && host == "" {
		return nil, "", fmt.Errorf("%s has listen %s, which has no host and would serve on every interface. Set listen to 127.0.0.1%s for this machine only, or name an address.", path, *raw.Listen, *raw.Listen)
	}
	if *raw.Listen != Stdio && !hostPort(*raw.Listen) {
		return nil, "", fmt.Errorf("%s has listen %s. Set listen to stdio, or to a host and port such as 127.0.0.1:8080.", path, *raw.Listen)
	}
	cfg.Listen = *raw.Listen
	if err := checkTransport(path, raw, cfg); err != nil {
		return nil, "", err
	}

	switch {
	case unset(raw.FreeSQL):
		return nil, "", missing(path, "free_sql", "Add free_sql: off, read or read-write.")
	case !slices.Contains([]FreeSQL{FreeSQLOff, FreeSQLRead, FreeSQLReadWrite}, FreeSQL(*raw.FreeSQL)):
		return nil, "", fmt.Errorf("%s has free_sql %s. Set free_sql to off, read or read-write.", path, *raw.FreeSQL)
	}
	cfg.FreeSQL = FreeSQL(*raw.FreeSQL)

	limits, err := checkLimits(path, raw.Limits)
	if err != nil {
		return nil, "", err
	}
	cfg.Limits = limits

	if unset(raw.Catalog) {
		return nil, "", missing(path, "catalog", "Add catalog: the catalog folder, or none.")
	}
	if *raw.Catalog != "none" {
		cfg.Catalog = besides(path, *raw.Catalog)
	}

	accessPath, err := checkMode(path, raw, cfg)
	if err != nil {
		return nil, "", err
	}

	if len(raw.Databases) == 0 {
		return nil, "", missing(path, "databases", "Add databases with at least one database and its login.")
	}
	cfg.Databases = make(map[string]Database, len(raw.Databases))
	for _, name := range slices.Sorted(maps.Keys(raw.Databases)) {
		db, err := checkDatabase(path, name, raw.Databases[name])
		if err != nil {
			return nil, "", err
		}
		cfg.Databases[name] = db
	}
	return cfg, accessPath, nil
}

// checkLimits checks the limits key and the keys under it.
func checkLimits(path string, raw *rawLimits) (Limits, error) {
	if raw == nil {
		return Limits{}, missing(path, "limits", "Add limits with rows, bytes, timeout and write_rows.")
	}
	counts := []struct {
		key  string
		v    *int
		what string
		eg   string
	}{
		{"limits.rows", raw.Rows, "the most rows a read returns", "1000"},
		{"limits.bytes", raw.Bytes, "the most bytes a read returns", "1048576"},
		{"limits.write_rows", raw.WriteRows, "the most rows a write may change", "100"},
	}
	for _, c := range counts {
		if c.v == nil {
			return Limits{}, missing(path, c.key, fmt.Sprintf("Add %s, %s, such as %s.", c.key, c.what, c.eg))
		}
		if *c.v < 1 {
			return Limits{}, fmt.Errorf("%s has %s %d, and it must be at least 1. Set %s to %s, such as %s.", path, c.key, *c.v, c.key, c.what, c.eg)
		}
	}
	if unset(raw.Timeout) {
		return Limits{}, missing(path, "limits.timeout", "Add limits.timeout, the statement timeout of each call, such as 30s.")
	}
	timeout, err := time.ParseDuration(*raw.Timeout)
	if err != nil {
		return Limits{}, fmt.Errorf("%s has limits.timeout %s, which is not a duration. Write it with a unit, such as 30s.", path, *raw.Timeout)
	}
	if timeout <= 0 {
		return Limits{}, fmt.Errorf("%s has limits.timeout %s. Set it above zero, such as 30s.", path, *raw.Timeout)
	}
	return Limits{Rows: *raw.Rows, Bytes: *raw.Bytes, Timeout: timeout, WriteRows: *raw.WriteRows}, nil
}

// checkMode checks the keys of one mode: access in standalone mode,
// gateway_tokens_sha256 in straza mode. It returns the resolved path of
// access.yaml.
func checkMode(path string, raw *rawServer, cfg *Config) (string, error) {
	if cfg.Mode == Standalone {
		if raw.GatewayTokensSHA256 != nil {
			return "", fmt.Errorf("%s sets gateway_tokens_sha256 in standalone mode, where access.yaml names the callers. Remove it, or set mode: straza.", path)
		}
		if unset(raw.Access) {
			return "", fmt.Errorf("%s does not set access, which standalone mode needs. Add access: the path to access.yaml.", path)
		}
		return besides(path, *raw.Access), nil
	}
	if raw.Access != nil {
		return "", fmt.Errorf("%s sets access in straza mode, where Straza authorizes callers. Remove it, or set mode: standalone.", path)
	}
	if len(raw.GatewayTokensSHA256) == 0 {
		return "", fmt.Errorf("%s does not set gateway_tokens_sha256, which straza mode needs. Add the token_sha256 that adaptive-pg-mcp token prints for each Straza token.", path)
	}
	for i, h := range raw.GatewayTokensSHA256 {
		hash, ok := parseHash(h)
		if !ok {
			return "", fmt.Errorf("%s has item %d of gateway_tokens_sha256 that is not 64 lowercase hex characters. Copy each hash as adaptive-pg-mcp token prints it.", path, i+1)
		}
		cfg.GatewayTokens = append(cfg.GatewayTokens, hash)
	}
	return "", nil
}

// checkTransport checks tls and allow_plain_http. It refuses either key with
// listen stdio, both keys together, and plain HTTP beyond this machine
// without allow_plain_http: true, because bearer tokens would cross the
// network in cleartext.
func checkTransport(path string, raw *rawServer, cfg *Config) error {
	plain := raw.AllowPlainHTTP != nil && *raw.AllowPlainHTTP
	switch {
	case cfg.Listen == Stdio && raw.TLS != nil:
		return fmt.Errorf("%s sets tls, but listen is stdio. Remove tls, or set listen to the address to serve HTTPS on.", path)
	case cfg.Listen == Stdio && plain:
		return fmt.Errorf("%s sets allow_plain_http: true, but listen is stdio. Remove allow_plain_http, or set listen to the address to serve HTTP on.", path)
	case raw.TLS != nil && plain:
		return fmt.Errorf("%s sets both tls and allow_plain_http: true, but tls serves HTTPS only. Remove allow_plain_http to serve HTTPS, or tls to serve plain HTTP.", path)
	case raw.TLS != nil && unset(raw.TLS.CertFile):
		return fmt.Errorf("%s sets tls without cert_file. Add tls.cert_file: the path to the PEM file of the certificate.", path)
	case raw.TLS != nil && unset(raw.TLS.KeyFile):
		return fmt.Errorf("%s sets tls without key_file. Add tls.key_file: the path to the PEM file of the private key.", path)
	case raw.TLS != nil:
		cfg.TLS = &TLS{CertFile: besides(path, *raw.TLS.CertFile), KeyFile: besides(path, *raw.TLS.KeyFile)}
	case cfg.Listen != Stdio && !plain && !OnThisMachine(cfg.Listen):
		_, port, _ := net.SplitHostPort(cfg.Listen)
		return fmt.Errorf("%s has listen %s without tls, so bearer tokens would cross the network in cleartext. Add tls, set listen to %s, or set allow_plain_http: true if a proxy or service mesh encrypts the traffic.", path, cfg.Listen, net.JoinHostPort("127.0.0.1", port))
	}
	cfg.AllowPlainHTTP = plain
	return nil
}

// OnThisMachine reports whether the host of listen is localhost or a
// loopback IP address, such as 127.0.0.1 or ::1. It does not resolve names,
// so any other host name counts as beyond this machine.
func OnThisMachine(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

// checkDatabase checks one database and its login or logins.
func checkDatabase(path, name string, raw *rawDatabase) (Database, error) {
	if raw == nil || (raw.Login == nil && raw.Logins == nil) {
		return Database{}, fmt.Errorf("%s database %s sets neither login nor logins. Add login with dsn_env, or logins with read and write.", path, name)
	}
	if raw.Login != nil && raw.Logins != nil {
		return Database{}, fmt.Errorf("%s database %s sets both login and logins. Keep login for a single login, or logins with read and write.", path, name)
	}
	db := Database{
		Logins:             map[string]Login{},
		AllowCrossDatabase: raw.AllowCrossDatabase != nil && *raw.AllowCrossDatabase,
		AllowSuperuser:     raw.AllowSuperuser != nil && *raw.AllowSuperuser,
	}
	if raw.Login != nil {
		env, err := checkEnv(path, name, "", raw.Login)
		if err != nil {
			return Database{}, err
		}
		db.Logins["read"], db.Logins["write"] = Login{Env: env}, Login{Env: env}
		return db, nil
	}
	for _, role := range []string{"read", "write"} {
		if _, ok := raw.Logins[role]; !ok {
			return Database{}, fmt.Errorf("%s database %s has logins without %s. Add logins.%s with dsn_env, or use login for a single login.", path, name, role, role)
		}
	}
	for _, role := range slices.Sorted(maps.Keys(raw.Logins)) {
		env, err := checkEnv(path, name, role, raw.Logins[role])
		if err != nil {
			return Database{}, err
		}
		db.Logins[role] = Login{Env: env}
	}
	return db, nil
}

// checkEnv checks the dsn_env of one login. role is empty for the single
// login of a database.
func checkEnv(path, db, role string, raw *rawLogin) (string, error) {
	if raw == nil || unset(raw.DSNEnv) {
		return "", fmt.Errorf("%s does not set dsn_env for %s. Add dsn_env: the name of the environment variable that holds its connection string.", path, loginName(db, role))
	}
	if !envRule.MatchString(*raw.DSNEnv) {
		return "", fmt.Errorf("%s has a dsn_env for %s that is not an environment variable name. Put the connection string in a variable, and set dsn_env to its name, such as %s.", path, loginName(db, role), exampleEnv(db, role))
	}
	return *raw.DSNEnv, nil
}

// readLogins reads the connection string of every login from its
// environment variable.
func readLogins(cfg *Config, getenv func(string) string) error {
	for _, name := range slices.Sorted(maps.Keys(cfg.Databases)) {
		db := cfg.Databases[name]
		single := db.Logins["read"].Env == db.Logins["write"].Env && len(db.Logins) == 2
		for _, role := range slices.Sorted(maps.Keys(db.Logins)) {
			l := db.Logins[role]
			label := loginName(name, role)
			if single {
				label = loginName(name, "")
			}
			v := getenv(l.Env)
			if v == "" {
				return fmt.Errorf("the environment variable %s for %s is unset or empty. Set it to a connection string for %s.", l.Env, label, name)
			}
			l.DSN = DSN{value: &v}
			db.Logins[role] = l
		}
	}
	return nil
}

// loginName names a login in a sentence, such as the login of sales_eu or
// the read login of helpdesk.
func loginName(db, role string) string {
	if role == "" {
		return "the login of " + db
	}
	return "the " + role + " login of " + db
}

// exampleEnv suggests an environment variable name for a login, such as
// HELPDESK_READ_DSN.
func exampleEnv(db, role string) string {
	s := db
	if role != "" {
		s += "_" + role
	}
	out := []byte(s + "_DSN")
	for i, c := range out {
		switch {
		case c >= 'a' && c <= 'z':
			out[i] = c - 'a' + 'A'
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
		default:
			out[i] = '_'
		}
	}
	return string(out)
}

// unset reports whether a string key is missing or empty.
func unset(s *string) bool { return s == nil || *s == "" }

// hostPort reports whether s is a host and a numeric port, such as
// 127.0.0.1:8080.
func hostPort(s string) bool {
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n <= 65535
}

// besides resolves p against the folder of the file at path, unless p is
// absolute.
func besides(path, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(path), p)
}

// parseHash reads a SHA-256 written as 64 lowercase hex characters.
func parseHash(s string) (Hash, bool) {
	var h Hash
	if !hashRule.MatchString(s) {
		return h, false
	}
	_, err := hex.Decode(h[:], []byte(s))
	return h, err == nil
}

// DSN is a connection string from the environment. It prints as a
// placeholder under every fmt verb.
type DSN struct {
	// value is a pointer, so a reflective print of a struct that holds a DSN
	// shows only an address.
	value *string
}

// hiddenDSN is what a DSN prints as.
const hiddenDSN = "[connection string hidden]"

// Reveal returns the connection string. Only the code that connects calls
// it.
func (d DSN) Reveal() string {
	if d.value == nil {
		return ""
	}
	return *d.value
}

// String returns the placeholder, never the connection string.
func (DSN) String() string { return hiddenDSN }

// GoString returns the placeholder for %#v, never the connection string.
func (DSN) GoString() string { return hiddenDSN }

// Format writes the placeholder for every fmt verb, never the connection
// string.
func (DSN) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, hiddenDSN) }
