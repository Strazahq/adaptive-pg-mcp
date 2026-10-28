package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

const (
	// password is the known password the secrets test hides in a connection string.
	password = "pw-9c41-must-never-print"
	// knownToken is the known token of support_bot in the test config, made of plain words.
	knownToken = "apm_known-token-of-the-secrets-test-000000000"
)

// hashHex is the SHA-256 of a token in lowercase hex.
func hashHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// syncBuffer is a buffer that a running server and the test may use at once.
// Its one-slot channel lets one of them in at a time.
type syncBuffer struct {
	turn chan struct{}
	b    bytes.Buffer
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{turn: make(chan struct{}, 1)} }

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.turn <- struct{}{}
	defer func() { <-s.turn }()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.turn <- struct{}{}
	defer func() { <-s.turn }()
	return s.b.String()
}

// writeConfig writes server.yaml and access.yaml into a fresh folder and returns the path of server.yaml.
func writeConfig(t *testing.T, listen, extra string) string {
	t.Helper()
	dir := t.TempDir()
	server := `version: 1
mode: standalone
listen: "` + listen + `"
free_sql: read
limits: { rows: 1000, bytes: 1048576, timeout: 30s, write_rows: 100 }
catalog: none
access: access.yaml
databases:
  sales_eu:
    login: { dsn_env: SALES_EU_DSN }
  helpdesk:
    logins:
      read: { dsn_env: HELPDESK_READ_DSN }
      write: { dsn_env: HELPDESK_WRITE_DSN }
` + extra
	access := `version: 1
callers:
  - name: support_bot
    token_sha256: "` + hashHex(knownToken) + `"
    tools: [list_tables, read_rows]
    databases: [helpdesk]
`
	for name, text := range map[string]string{"server.yaml": server, "access.yaml": access} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "server.yaml")
}

// environ holds a connection string for every login, the first with the known password.
func environ(token string) map[string]string {
	return map[string]string{
		"SALES_EU_DSN":          "postgres://sales:" + password + "@db.example.internal/sales_eu",
		"HELPDESK_READ_DSN":     "postgres://reader:" + password + "@db.example.internal/helpdesk",
		"HELPDESK_WRITE_DSN":    "postgres://writer:" + password + "@db.example.internal/helpdesk",
		"ADAPTIVE_PG_MCP_TOKEN": token,
	}
}

// lookup turns an environment map into a getenv.
func lookup(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestTokenPrintsATokenAndItsHash(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run(context.Background(), []string{"token"}, &out, &errs, lookup(nil)); code != 0 {
		t.Fatalf("token exited %d: %s", code, errs.String())
	}
	m := regexp.MustCompile(`^token: (apm_[A-Za-z0-9_-]{43})\ntoken_sha256: ([0-9a-f]{64})\n$`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("token printed %q, want a token line and a token_sha256 line", out.String())
	}
	if m[2] != hashHex(m[1]) {
		t.Errorf("token_sha256 %s is not the SHA-256 of the token", m[2])
	}
	if strings.Contains(errs.String(), m[1]) {
		t.Errorf("stderr repeats the token: %s", errs.String())
	}
	if code := run(context.Background(), []string{"token", "extra"}, &out, &errs, lookup(nil)); code != 2 {
		t.Errorf("token with an argument exited %d, want 2", code)
	}
}

func TestServeOnStdioRefusesAMissingOrWrongToken(t *testing.T) {
	cases := []struct{ name, token, want string }{
		{"missing", "", "ADAPTIVE_PG_MCP_TOKEN is not set, and on stdio adaptive-pg-mcp needs it to know who calls, so it will not start."},
		{"wrong", knownToken + "x", "the token in ADAPTIVE_PG_MCP_TOKEN matches no configured hash, so adaptive-pg-mcp will not start."},
	}
	path := writeConfig(t, "stdio", "")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			code := run(context.Background(), []string{"serve", "-config", path}, &out, &errs, lookup(environ(c.token)))
			if code != 1 || !strings.Contains(errs.String(), c.want) {
				t.Errorf("serve exited %d with %q, want 1 and %q", code, errs.String(), c.want)
			}
			if out.Len() != 0 {
				t.Errorf("serve wrote %q to stdout, which carries the protocol on stdio", out.String())
			}
		})
	}
}

func TestSecretsStayOutOfLogsAndErrors(t *testing.T) {
	var seen strings.Builder
	keep := func(s string) { seen.WriteString(s + "\n") }

	// Config loading, and its failures while the connection strings are set.
	good := writeConfig(t, "127.0.0.1:0", "")
	cfg, err := config.Load(good, lookup(environ(knownToken)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	keep(fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg))
	failing := []struct {
		name  string
		extra string
		env   map[string]string
	}{
		{"an unset variable", "  finance:\n    login: { dsn_env: FINANCE_DSN }\n", environ(knownToken)},
		{"a connection string in dsn_env", "  finance:\n    login: { dsn_env: \"postgres://f:" + password + "@db.example.internal/f\" }\n", environ(knownToken)},
		{"a connection string as a number", "  finance:\n    allow_superuser: \"" + password + "\"\n    login: { dsn_env: FINANCE_DSN }\n", environ(knownToken)},
		{"an unknown key", "  finance:\n    dsn: \"postgres://f:" + password + "@db.example.internal/f\"\n", environ(knownToken)},
	}
	for _, f := range failing {
		if _, err := config.Load(writeConfig(t, "127.0.0.1:0", f.extra), lookup(f.env)); err == nil {
			t.Errorf("%s: Load accepted the config", f.name)
		} else {
			keep(err.Error())
		}
	}

	// Serve start-up on stdio with a wrong token, which is the known one changed.
	var out, errs bytes.Buffer
	run(context.Background(), []string{"serve", "-config", writeConfig(t, "stdio", "")}, &out, &errs, lookup(environ(knownToken+"!")))
	keep(errs.String() + out.String())

	// Check and serve on HTTP, while no database of the connection strings can be reached.
	for _, args := range [][]string{{"check", "-config", good}, {"serve", "-config", good}} {
		var out, errs bytes.Buffer
		if code := run(context.Background(), args, &out, &errs, lookup(environ(knownToken))); code != 1 {
			t.Errorf("%s exited %d while no database can be reached, want 1", args[0], code)
		}
		keep(errs.String() + out.String())
	}

	for _, secret := range []string{password, knownToken} {
		if strings.Contains(seen.String(), secret) {
			t.Errorf("a log line, an error or an answer shows %q:\n%s", secret, seen.String())
		}
	}
	if !strings.Contains(seen.String(), "FINANCE_DSN") || !strings.Contains(seen.String(), "the login of sales_eu cannot connect, because the host name of the connection string does not resolve") {
		t.Errorf("the capture misses the failures it should hold:\n%s", seen.String())
	}
}

// serveHTTP runs serve on HTTP with the config at path, sends it requests
// that carry the known token in ways it must refuse and one it must admit,
// calls list_databases with the known token, and stops it. It hands every
// answer and log line to keep.
func serveHTTP(t *testing.T, path string, env map[string]string, keep func(string)) {
	t.Helper()
	logs := newSyncBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan int, 1)
	go func() {
		exited <- run(ctx, []string{"serve", "-config", path}, io.Discard, logs, lookup(env))
	}()
	url := waitForAddress(t, logs)
	refused := []struct {
		name   string
		header map[string][]string
		status int
	}{
		{"another scheme", map[string][]string{"Authorization": {"Basic " + knownToken}}, http.StatusUnauthorized},
		{"the token changed", map[string][]string{"Authorization": {"Bearer " + knownToken + "x"}}, http.StatusUnauthorized},
		{"the token and more", map[string][]string{"Authorization": {"Bearer " + knownToken + " " + knownToken}}, http.StatusUnauthorized},
		{"the token twice", map[string][]string{"Authorization": {"Bearer " + knownToken, "Bearer " + knownToken}}, http.StatusUnauthorized},
		{"a web page", map[string][]string{"Authorization": {"Bearer " + knownToken}, "Origin": {"https://attacker.example"}}, http.StatusForbidden},
		{"an unknown session", map[string][]string{"Authorization": {"Bearer " + knownToken}, "Mcp-Session-Id": {"unknown"}, "Mcp-Protocol-Version": {"2025-11-25"}}, http.StatusNotFound},
		{"the right token", map[string][]string{"Authorization": {"Bearer " + knownToken}}, http.StatusOK},
	}
	for _, r := range refused {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
		req.Header = r.header
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != r.status {
			t.Errorf("%s answered %d, want %d: %s", r.name, res.StatusCode, r.status, body)
		}
		keep(fmt.Sprintf("%s %v %s", r.name, res.Header, body))
	}
	keep(listDatabases(t, url))
	cancel()
	if code := <-exited; code != 0 {
		t.Errorf("serve exited %d after its context ended, want 0", code)
	}
	keep(logs.String())
}

// bearer adds the known token to every request of the SDK client.
type bearer struct{}

// RoundTrip sends r with the known token in its Authorization header.
func (bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+knownToken)
	return http.DefaultTransport.RoundTrip(r)
}

// listDatabases calls list_databases at url with the known token and returns its text.
func listDatabases(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{}}, MaxRetries: -1}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_databases", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("list_databases: %v", err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError || text != "list_databases found 1 database: helpdesk." {
		t.Errorf("list_databases answered %q, want the one database of support_bot", text)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	return text + " " + string(raw)
}

// waitForAddress waits for serve to log its address and returns the endpoint URL.
func waitForAddress(t *testing.T, logs *syncBuffer) string {
	t.Helper()
	re := regexp.MustCompile(`address=(http://127\.0\.0\.1:\d+/mcp)`)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if m := re.FindStringSubmatch(logs.String()); m != nil {
			return m[1]
		}
	}
	t.Fatalf("serve logged no address:\n%s", logs.String())
	return ""
}
