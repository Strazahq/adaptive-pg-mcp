package pg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// dsnOf returns dsn as config holds it, through a server.yaml that names it in an environment variable.
func dsnOf(t *testing.T, dsn string) config.DSN {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.yaml")
	text := `version: 1
mode: straza
listen: stdio
free_sql: "off"
limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }
gateway_tokens_sha256: ["` + strings.Repeat("0", 64) + `"]
catalog: none
databases:
  sales_eu:
    login: { dsn_env: SALES_EU_DSN }
`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, func(string) string { return dsn })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg.Databases["sales_eu"].Logins["read"].DSN
}
