package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// whoami is a test tool that answers the caller of its own request.
func whoami(s *Server) {
	s.mcp.AddTool(&mcp.Tool{Name: "whoami", Description: "Returns the caller of the request.", InputSchema: &jsonschema.Schema{Type: "object"}},
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			caller, _ := callerFrom(ctx)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: caller}}}, nil
		})
}

// serve starts s.Handler on a loopback httptest server with the whoami tool.
func serve(t *testing.T, s *Server) string {
	t.Helper()
	whoami(s)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL + endpointPath
}

// quiet is a logger that keeps its lines for a test to read.
func quiet() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, nil)), &b
}

// sessionful reports whether revision v keeps protocol sessions, as the SDK's stateful transport says.
func sessionful(v string) bool { return (&mcp.StreamableServerTransport{}).SupportsProtocolVersion(v) }

// client bounds every test request, so that a stream a refusal should have stopped fails the test instead of hanging it.
var client = &http.Client{Timeout: 5 * time.Second}

// answer is one HTTP exchange.
type answer struct {
	status int
	header http.Header
	body   string
}

// request is one HTTP request to the MCP endpoint.
type request struct {
	method   string
	auth     []string
	revision string
	session  string
	origin   string
	host     string
	body     string
	rpc      string
	name     string
}

// send makes one real HTTP request and reads the whole answer.
func send(t *testing.T, url string, r request) answer {
	t.Helper()
	if r.method == "" {
		r.method = http.MethodPost
	}
	req, err := http.NewRequest(r.method, url, strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for _, a := range r.auth {
		req.Header.Add("Authorization", a)
	}
	for k, v := range map[string]string{"Mcp-Protocol-Version": r.revision, "Mcp-Session-Id": r.session, "Origin": r.origin, "Mcp-Method": r.rpc, "Mcp-Name": r.name} {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	if r.host != "" {
		req.Host = r.host
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", r.method, url, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return answer{status: res.StatusCode, header: res.Header, body: string(b)}
}

// result returns the JSON-RPC message of an answer, written as JSON or as the last SSE data line.
func result(t *testing.T, a answer) map[string]any {
	t.Helper()
	text := strings.TrimSpace(a.body)
	if !strings.HasPrefix(text, "{") {
		for _, line := range strings.Split(a.body, "\n") {
			if d, ok := strings.CutPrefix(line, "data: "); ok {
				text = d
			}
		}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("the answer %d is not JSON-RPC: %q", a.status, a.body)
	}
	return m
}

// meta is the per-request _meta of the stateless revisions.
func meta(v string) string {
	return `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + v + `","io.modelcontextprotocol/clientCapabilities":{}}`
}

// opening is the first request a client of revision v sends.
func opening(v string, auth ...string) request {
	if sessionful(v) {
		return request{auth: auth, body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + v + `","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`}
	}
	return request{auth: auth, revision: v, rpc: "server/discover", body: `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + meta(v) + `}}`}
}

// whoamiCall is a tools/call of whoami on revision v and session.
func whoamiCall(v, session string, auth ...string) request {
	params := `"name":"whoami","arguments":{}`
	if !sessionful(v) {
		params += "," + meta(v)
	}
	return request{method: http.MethodPost, auth: auth, revision: v, session: session, rpc: "tools/call", name: "whoami", body: `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{` + params + `}}`}
}

// open starts a client of revision v with token and returns its session id, empty on a sessionless revision.
func open(t *testing.T, url, v, token string) string {
	t.Helper()
	a := send(t, url, opening(v, "Bearer "+token))
	if a.status != http.StatusOK {
		t.Fatalf("revision %s: the opening request answered %d: %s", v, a.status, a.body)
	}
	session := a.header.Get("Mcp-Session-Id")
	if sessionful(v) {
		if session == "" {
			t.Fatalf("revision %s: initialize answered no session id", v)
		}
		done := send(t, url, request{auth: []string{"Bearer " + token}, revision: v, session: session, body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`})
		if done.status != http.StatusAccepted {
			t.Fatalf("revision %s: notifications/initialized answered %d: %s", v, done.status, done.body)
		}
	} else if session != "" {
		t.Errorf("revision %s: a sessionless revision answered the session id %s", v, session)
	}
	return session
}

// callerOf returns the caller whoami answers, or fails t.
func callerOf(t *testing.T, a answer) string {
	t.Helper()
	m := result(t, a)
	res, _ := m["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("whoami answered %d %s", a.status, a.body)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

func TestTokenCheckOnEveryRevision(t *testing.T) {
	revisions := mcp.SupportedProtocolVersions()
	if len(revisions) < 2 {
		t.Fatalf("the SDK serves %v, want more than one revision to test", revisions)
	}
	cases := []struct {
		name   string
		auth   []string
		status int
	}{
		{"the right token", []string{"Bearer " + tokenA}, http.StatusOK},
		{"the scheme in small letters", []string{"bearer " + tokenA}, http.StatusOK},
		{"the scheme in capitals", []string{"BEARER " + tokenA}, http.StatusOK},
		{"surrounding whitespace", []string{"  Bearer   " + tokenA + "  "}, http.StatusOK},
		{"a wrong token", []string{"Bearer " + tokenA + "x"}, http.StatusUnauthorized},
		{"a token whose hash is not configured", []string{"Bearer " + unknownToken}, http.StatusUnauthorized},
		{"no header", nil, http.StatusUnauthorized},
		{"an empty token", []string{"Bearer "}, http.StatusUnauthorized},
		{"another scheme", []string{"Basic " + tokenA}, http.StatusUnauthorized},
		{"two headers", []string{"Bearer " + tokenA, "Bearer " + tokenA}, http.StatusUnauthorized},
	}
	log, logs := quiet()
	url := serve(t, New(standaloneConfig(), log))
	for _, v := range revisions {
		for _, c := range cases {
			t.Run(v+"/"+c.name, func(t *testing.T) {
				a := send(t, url, opening(v, c.auth...))
				if a.status != c.status {
					t.Fatalf("the opening request answered %d, want %d: %s", a.status, c.status, a.body)
				}
				if c.status == http.StatusUnauthorized {
					if !strings.HasPrefix(a.header.Get("WWW-Authenticate"), "Bearer ") || !strings.Contains(a.body, "bearer token") {
						t.Errorf("the refusal has challenge %q and body %q", a.header.Get("WWW-Authenticate"), a.body)
					}
					return
				}
				res, _ := result(t, a)["result"].(map[string]any)
				if sessionful(v) && res["protocolVersion"] != v {
					t.Errorf("initialize negotiated %v, want %s", res["protocolVersion"], v)
				}
				if supported, _ := res["supportedVersions"].([]any); !sessionful(v) && !slices.Contains(supported, any(v)) {
					t.Errorf("server/discover lists %v, want it to hold %s", supported, v)
				}
			})
		}
	}
	for _, tok := range []string{tokenA, unknownToken} {
		if strings.Contains(logs.String(), tok) {
			t.Errorf("a log line shows a token:\n%s", logs.String())
		}
	}
}

func TestTokenCheckOnEveryRequest(t *testing.T) {
	url := serve(t, New(standaloneConfig(), slog.New(slog.DiscardHandler)))
	bad := [][]string{nil, {"Bearer " + unknownToken}, {"Basic " + tokenA}}
	for _, v := range mcp.SupportedProtocolVersions() {
		t.Run(v, func(t *testing.T) {
			session := open(t, url, v, tokenA)
			for _, auth := range bad {
				later := []request{whoamiCall(v, session, auth...)}
				if sessionful(v) {
					later = append(later,
						request{method: http.MethodGet, auth: auth, revision: v, session: session},
						request{method: http.MethodDelete, auth: auth, revision: v, session: session})
				}
				for _, r := range later {
					if a := send(t, url, r); a.status != http.StatusUnauthorized {
						t.Errorf("%s with %q answered %d, want 401: %s", r.method, auth, a.status, a.body)
					}
				}
			}
			if got := callerOf(t, send(t, url, whoamiCall(v, session, "Bearer "+tokenA))); got != "support_bot" {
				t.Errorf("whoami answered %q, want support_bot", got)
			}
		})
	}
}

func TestSessionIsBoundToItsCaller(t *testing.T) {
	url := serve(t, New(standaloneConfig(), slog.New(slog.DiscardHandler)))
	for _, v := range mcp.SupportedProtocolVersions() {
		t.Run(v, func(t *testing.T) {
			session := open(t, url, v, tokenA)
			if !sessionful(v) {
				// A sessionless revision mints no session id, so a session id the
				// request carries grants nothing: the caller is the token's.
				a := send(t, url, whoamiCall(v, "a-session-id-of-support_bot", "Bearer "+tokenB))
				if got := callerOf(t, a); got != "ops_bot" {
					t.Errorf("whoami with the token of ops_bot answered %q, want ops_bot", got)
				}
				return
			}
			for _, r := range []request{
				whoamiCall(v, session, "Bearer "+tokenB),
				{method: http.MethodGet, auth: []string{"Bearer " + tokenB}, revision: v, session: session},
				{method: http.MethodDelete, auth: []string{"Bearer " + tokenB}, revision: v, session: session},
			} {
				if a := send(t, url, r); a.status != http.StatusForbidden {
					t.Errorf("%s with the token of ops_bot on the session of support_bot answered %d, want 403: %s", r.method, a.status, a.body)
				}
			}
			if got := callerOf(t, send(t, url, whoamiCall(v, session, "Bearer "+tokenA))); got != "support_bot" {
				t.Errorf("whoami on its own session answered %q, want support_bot", got)
			}
			other := open(t, url, v, tokenB)
			if got := callerOf(t, send(t, url, whoamiCall(v, other, "Bearer "+tokenB))); got != "ops_bot" {
				t.Errorf("whoami on the session of ops_bot answered %q, want ops_bot", got)
			}
			if a := send(t, url, request{method: http.MethodDelete, auth: []string{"Bearer " + tokenA}, revision: v, session: session}); a.status != http.StatusNoContent {
				t.Errorf("DELETE of its own session answered %d, want 204", a.status)
			}
		})
	}
}

func TestOriginAndHost(t *testing.T) {
	const refused = "the request has an Origin header, so it comes from a web page, and web pages may not call this server. Use an MCP client that sends no Origin header.\n"
	url := serve(t, New(standaloneConfig(), slog.New(slog.DiscardHandler)))
	v := mcp.SupportedProtocolVersions()[0]
	for _, origin := range []string{"https://attacker.example", "http://127.0.0.1:8080", "null"} {
		for _, auth := range [][]string{{"Bearer " + tokenA}, nil} {
			r := opening(v, auth...)
			r.origin = origin
			a := send(t, url, r)
			if a.status != http.StatusForbidden || a.body != refused {
				t.Errorf("Origin %s with %q answered %d %q, want 403 and the refusal", origin, auth, a.status, a.body)
			}
		}
	}
	r := opening(v, "Bearer "+tokenA)
	r.host = "attacker.example"
	if a := send(t, url, r); a.status != http.StatusForbidden {
		t.Errorf("a request with the Host attacker.example on loopback answered %d, want 403 from the SDK's DNS rebinding check", a.status)
	}
	if a := send(t, url, opening(v, "Bearer "+tokenA)); a.status != http.StatusOK {
		t.Errorf("a request without Origin answered %d, want 200: %s", a.status, a.body)
	}
}

func TestOnlyTheEndpointServes(t *testing.T) {
	url := strings.TrimSuffix(serve(t, New(standaloneConfig(), slog.New(slog.DiscardHandler))), endpointPath)
	v := mcp.SupportedProtocolVersions()[0]
	if a := send(t, url+"/other", opening(v)); a.status != http.StatusUnauthorized {
		t.Errorf("another path without a token answered %d, want 401", a.status)
	}
	if a := send(t, url+"/other", opening(v, "Bearer "+tokenA)); a.status != http.StatusNotFound {
		t.Errorf("another path with a token answered %d, want 404", a.status)
	}
}

func TestRequestBodyCap(t *testing.T) {
	url := serve(t, New(standaloneConfig(), slog.New(slog.DiscardHandler)))
	for _, v := range mcp.SupportedProtocolVersions() {
		r := opening(v, "Bearer "+tokenA)
		r.body = `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxRequestBody) + `"}}`
		if a := send(t, url, r); a.status != http.StatusRequestEntityTooLarge {
			t.Errorf("revision %s: a body over %d bytes answered %d, want 413", v, maxRequestBody, a.status)
		}
	}
}
