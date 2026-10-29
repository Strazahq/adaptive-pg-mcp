package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	hashA = "1111111111111111111111111111111111111111111111111111111111111111"
	hashB = "2222222222222222222222222222222222222222222222222222222222222222"
)

// serverDoc is a valid standalone server.yaml that each case changes one key of.
func serverDoc() map[string]any {
	return map[string]any{
		"version":  1,
		"mode":     "standalone",
		"listen":   "127.0.0.1:8080",
		"free_sql": "read-write",
		"limits":   map[string]any{"rows": 1000, "bytes": 1048576, "timeout": "30s", "write_rows": 100},
		"catalog":  "none",
		"access":   "access.yaml",
		"databases": map[string]any{
			"sales_eu": map[string]any{"login": map[string]any{"dsn_env": "SALES_EU_DSN"}},
			"helpdesk": map[string]any{
				"logins": map[string]any{
					"read":           map[string]any{"dsn_env": "HELPDESK_READ_DSN"},
					"write":          map[string]any{"dsn_env": "HELPDESK_WRITE_DSN"},
					"finance_reader": map[string]any{"dsn_env": "HELPDESK_FINANCE_DSN"},
				},
				"allow_cross_database": true,
				"allow_superuser":      false,
			},
		},
	}
}

// accessDoc is a valid access.yaml for serverDoc.
func accessDoc() map[string]any {
	return map[string]any{
		"version": 1,
		"callers": []any{
			map[string]any{"name": "support_bot", "token_sha256": hashA, "tools": []any{"list_tables", "read_rows"}, "databases": []any{"helpdesk"}, "tables": []any{"public.tickets"}},
			map[string]any{"name": "ops_bot", "token_sha256": hashB, "tools": []any{}, "databases": []any{"helpdesk", "sales_eu"}},
		},
	}
}

// environment holds a connection string for every login of serverDoc.
func environment() map[string]string {
	return map[string]string{
		"SALES_EU_DSN":         "postgres://sales@db.example.internal/sales_eu",
		"HELPDESK_READ_DSN":    "postgres://reader@db.example.internal/helpdesk",
		"HELPDESK_WRITE_DSN":   "postgres://writer@db.example.internal/helpdesk",
		"HELPDESK_FINANCE_DSN": "postgres://finance@db.example.internal/helpdesk",
	}
}

// at walks a dotted path through nested maps and returns the map that holds
// the last key. A number in the path picks an item of a list.
func at(doc map[string]any, path string) (map[string]any, string) {
	parts := strings.Split(path, ".")
	var cur any = doc
	for _, p := range parts[:len(parts)-1] {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			var i int
			fmt.Sscanf(p, "%d", &i)
			cur = c[i]
		}
	}
	return cur.(map[string]any), parts[len(parts)-1]
}

func del(doc map[string]any, path string)        { m, k := at(doc, path); delete(m, k) }
func set(doc map[string]any, path string, v any) { m, k := at(doc, path); m[k] = v }

// files is one run of Load: the two documents as text and the environment.
type files struct {
	server, access string
	env            map[string]string
}

// marshal writes both documents as YAML text.
func marshal(t *testing.T, server, access map[string]any, env map[string]string) files {
	t.Helper()
	s, err := yaml.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}
	a, err := yaml.Marshal(access)
	if err != nil {
		t.Fatal(err)
	}
	return files{server: string(s), access: string(a), env: env}
}

// load writes the files into a fresh folder and runs Load on them. It returns
// the folder, which every expected refusal names as $DIR.
func load(t *testing.T, f files) (*Config, string, error) {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{"server.yaml": f.server, "access.yaml": f.access} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(filepath.Join(dir, "server.yaml"), func(k string) string { return f.env[k] })
	return cfg, dir, err
}

// lineOf returns the 1-based line of text that holds needle.
func lineOf(t *testing.T, text, needle string) int {
	t.Helper()
	for i, l := range strings.Split(text, "\n") {
		if strings.Contains(l, needle) {
			return i + 1
		}
	}
	t.Fatalf("%q is not in:\n%s", needle, text)
	return 0
}

func TestLoadReadsAValidStandaloneConfig(t *testing.T) {
	cfg, dir, err := load(t, marshal(t, serverDoc(), accessDoc(), environment()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != Standalone || cfg.Listen != "127.0.0.1:8080" || cfg.FreeSQL != FreeSQLReadWrite || cfg.Catalog != "" {
		t.Errorf("mode %s listen %s free_sql %s catalog %q", cfg.Mode, cfg.Listen, cfg.FreeSQL, cfg.Catalog)
	}
	if want := (Limits{Rows: 1000, Bytes: 1048576, Timeout: 30 * time.Second, WriteRows: 100}); cfg.Limits != want {
		t.Errorf("limits %+v, want %+v", cfg.Limits, want)
	}
	sales := cfg.Databases["sales_eu"]
	if sales.Logins["read"].Env != "SALES_EU_DSN" || sales.Logins["write"].DSN.Reveal() != environment()["SALES_EU_DSN"] || len(sales.Logins) != 2 {
		t.Errorf("sales_eu logins %+v, want read and write on SALES_EU_DSN", sales.Logins)
	}
	if sales.AllowCrossDatabase || sales.AllowSuperuser {
		t.Errorf("sales_eu allow_ keys default to %v and %v, want false", sales.AllowCrossDatabase, sales.AllowSuperuser)
	}
	help := cfg.Databases["helpdesk"]
	if help.Logins["finance_reader"].DSN.Reveal() != environment()["HELPDESK_FINANCE_DSN"] || len(help.Logins) != 3 || !help.AllowCrossDatabase {
		t.Errorf("helpdesk %+v", help)
	}
	if len(cfg.Callers) != 2 || cfg.Callers[0].Name != "support_bot" || cfg.Callers[0].Tables[0] != "public.tickets" || cfg.Callers[1].Tables != nil {
		t.Errorf("callers %+v", cfg.Callers)
	}
	if cfg.Callers[0].TokenSHA256[0] != 0x11 || len(cfg.Callers[1].Tools) != 0 {
		t.Errorf("callers %+v", cfg.Callers)
	}

	s := serverDoc()
	set(s, "catalog", "catalog")
	cfg, dir, err = load(t, marshal(t, s, accessDoc(), environment()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Catalog != filepath.Join(dir, "catalog") {
		t.Errorf("catalog %s, want it beside server.yaml in %s", cfg.Catalog, dir)
	}
}

func TestLoadReadsAValidStrazaConfig(t *testing.T) {
	s := serverDoc()
	set(s, "mode", "straza")
	del(s, "access")
	set(s, "gateway_tokens_sha256", []any{hashA, hashB})
	cfg, _, err := load(t, marshal(t, s, accessDoc(), environment()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != Straza || len(cfg.GatewayTokens) != 2 || cfg.GatewayTokens[1][0] != 0x22 || cfg.Callers != nil {
		t.Errorf("mode %s gateway tokens %d callers %v", cfg.Mode, len(cfg.GatewayTokens), cfg.Callers)
	}
}

func TestLoadReadsUnquotedYAMLWords(t *testing.T) {
	text := marshal(t, serverDoc(), accessDoc(), environment())
	text.server = strings.Replace(text.server, "free_sql: read-write", "free_sql: off", 1)
	cfg, _, err := load(t, text)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FreeSQL != FreeSQLOff {
		t.Errorf("free_sql: off reads as %q, want off", cfg.FreeSQL)
	}
}

func TestLoadAcceptsTheTransports(t *testing.T) {
	pair := map[string]any{"cert_file": "tls/server.crt", "key_file": "/etc/adaptive-pg-mcp/server.key"}
	cases := []struct {
		name   string
		change func(s map[string]any)
		tls    bool
		plain  bool
	}{
		{"plain HTTP on 127.0.0.1", func(map[string]any) {}, false, false},
		{"plain HTTP on another loopback address", func(s map[string]any) { set(s, "listen", "127.0.0.2:8080") }, false, false},
		{"plain HTTP on ::1", func(s map[string]any) { set(s, "listen", "[::1]:8080") }, false, false},
		{"plain HTTP on localhost", func(s map[string]any) { set(s, "listen", "localhost:8080") }, false, false},
		{"plain HTTP beyond this machine with allow_plain_http", func(s map[string]any) { set(s, "listen", "0.0.0.0:8080"); set(s, "allow_plain_http", true) }, false, true},
		{"plain HTTP on loopback with allow_plain_http", func(s map[string]any) { set(s, "allow_plain_http", true) }, false, true},
		{"HTTPS beyond this machine", func(s map[string]any) { set(s, "listen", "0.0.0.0:8443"); set(s, "tls", pair) }, true, false},
		{"HTTPS with allow_plain_http false", func(s map[string]any) { set(s, "tls", pair); set(s, "allow_plain_http", false) }, true, false},
		{"stdio with allow_plain_http false", func(s map[string]any) { set(s, "listen", "stdio"); set(s, "allow_plain_http", false) }, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := serverDoc()
			c.change(s)
			cfg, dir, err := load(t, marshal(t, s, accessDoc(), environment()))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AllowPlainHTTP != c.plain {
				t.Errorf("allow_plain_http reads as %v, want %v", cfg.AllowPlainHTTP, c.plain)
			}
			switch {
			case !c.tls && cfg.TLS != nil:
				t.Errorf("tls reads as %+v, want none", cfg.TLS)
			case c.tls && (cfg.TLS == nil || *cfg.TLS != TLS{CertFile: filepath.Join(dir, "tls", "server.crt"), KeyFile: "/etc/adaptive-pg-mcp/server.key"}):
				t.Errorf("tls reads as %+v, want cert_file beside server.yaml in %s and key_file as written", cfg.TLS, dir)
			}
		})
	}
}

func TestOnThisMachine(t *testing.T) {
	cases := []struct {
		listen string
		local  bool
	}{
		{"127.0.0.1:8080", true},
		{"127.10.0.1:8080", true},
		{"[::1]:8080", true},
		{"[::ffff:127.0.0.1]:8080", true},
		{"localhost:8080", true},
		{"LocalHost:8080", true},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"192.0.2.10:8080", false},
		{"localhost.example.internal:8080", false},
		{"stdio", false},
	}
	for _, c := range cases {
		if got := OnThisMachine(c.listen); got != c.local {
			t.Errorf("OnThisMachine(%s) is %v, want %v", c.listen, got, c.local)
		}
	}
}
