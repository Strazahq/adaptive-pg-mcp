package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// testToken makes a token-shaped test value from a readable word, so that no test literal looks like a real key.
func testToken(word string) string { return tokenPrefix + word + strings.Repeat("0", 43-len(word)) }

var (
	// tokenA is the token of support_bot in standaloneConfig.
	tokenA = testToken("support-bot")
	// tokenB is the token of ops_bot in standaloneConfig.
	tokenB = testToken("ops-bot")
	// gatewayToken is a token Straza holds, which carries no apm_ prefix.
	gatewayToken = strings.TrimPrefix(testToken("straza-call-token"), tokenPrefix)
	// unknownToken has the form of a token, but no config holds its hash.
	unknownToken = testToken("unknown")
)

// hashOf returns the SHA-256 of a token as config holds it.
func hashOf(token string) config.Hash { return sha256.Sum256([]byte(token)) }

// standaloneConfig is a standalone config whose callers hold tokenA and tokenB.
func standaloneConfig() *config.Config {
	return &config.Config{
		Mode:   config.Standalone,
		Listen: "127.0.0.1:0",
		Callers: []config.Caller{
			{Name: "support_bot", TokenSHA256: hashOf(tokenA), Tools: []string{"list_databases", "read_rows"}, Databases: []string{"helpdesk"}},
			{Name: "ops_bot", TokenSHA256: hashOf(tokenB), Tools: []string{"list_databases", "read_rows"}, Databases: []string{"helpdesk"}},
		},
	}
}

// strazaConfig is a straza config whose gateway holds gatewayToken for calls
// and listingToken for the listing.
func strazaConfig() *config.Config {
	return &config.Config{Mode: config.Straza, Listen: "127.0.0.1:0", GatewayTokens: []config.Hash{hashOf(unknownToken + "x"), hashOf(gatewayToken)}, ListingTokens: []config.Hash{hashOf(listingToken)}}
}

func TestCheckBearer(t *testing.T) {
	standalone, straza := credentials(standaloneConfig()), credentials(strazaConfig())
	cases := []struct {
		name   string
		creds  []credential
		header []string
		caller string
		why    refusal
	}{
		{"the right token", standalone, []string{"Bearer " + tokenA}, "support_bot", admitted},
		{"another caller's token", standalone, []string{"Bearer " + tokenB}, "ops_bot", admitted},
		{"the scheme in small letters", standalone, []string{"bearer " + tokenA}, "support_bot", admitted},
		{"the scheme in capitals", standalone, []string{"BEARER " + tokenA}, "support_bot", admitted},
		{"the scheme in mixed case", standalone, []string{"bEaReR " + tokenA}, "support_bot", admitted},
		{"surrounding whitespace", standalone, []string{" \tBearer  " + tokenA + " \t"}, "support_bot", admitted},
		{"a gateway token without the apm_ prefix", straza, []string{"Bearer " + gatewayToken}, gatewayCaller, admitted},
		{"a listing token", straza, []string{"Bearer " + listingToken}, listingCaller, admitted},
		{"a listing token in mode standalone", standalone, []string{"Bearer " + listingToken}, "", badBearer},
		{"a wrong token", standalone, []string{"Bearer " + tokenA + "x"}, "", badBearer},
		{"a token whose hash is not configured", standalone, []string{"Bearer " + unknownToken}, "", badBearer},
		{"a caller token in mode straza", straza, []string{"Bearer " + tokenA}, "", badBearer},
		{"a gateway token in mode standalone", standalone, []string{"Bearer " + gatewayToken}, "", badBearer},
		{"no header", standalone, nil, "", noBearer},
		{"an empty header", standalone, []string{""}, "", noBearer},
		{"an empty token", standalone, []string{"Bearer"}, "", badBearer},
		{"an empty token after spaces", standalone, []string{"Bearer   "}, "", badBearer},
		{"another scheme", standalone, []string{"Basic c3VwcG9ydF9ib3Q6cGFzcw=="}, "", noBearer},
		{"another scheme with a right token", standalone, []string{"Token " + tokenA}, "", noBearer},
		{"the token without a scheme", standalone, []string{tokenA}, "", noBearer},
		{"a token and more", standalone, []string{"Bearer " + tokenA + " " + tokenA}, "", badBearer},
		{"two headers", standalone, []string{"Bearer " + tokenA, "Bearer " + tokenA}, "", badBearer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caller, why := checkBearer(c.creds, c.header)
			if caller != c.caller || why != c.why {
				t.Errorf("checkBearer = %q, %v; want %q, %v", caller, why, c.caller, c.why)
			}
		})
	}
}

func TestMatchComparesEveryHash(t *testing.T) {
	creds := []credential{{hash: hashOf("a"), caller: "first"}, {hash: hashOf("b"), caller: "second"}, {hash: hashOf("c"), caller: "third"}}
	for token, want := range map[string]string{"a": "first", "b": "second", "c": "third", "d": "", "": ""} {
		got, ok := match(creds, token)
		if got != want || ok != (want != "") {
			t.Errorf("match(%q) = %q, %v; want %q", token, got, ok, want)
		}
	}
	if got, ok := match(nil, "a"); ok || got != "" {
		t.Errorf("match with no hashes = %q, %v; want a refusal", got, ok)
	}
}

func TestAuthenticate(t *testing.T) {
	const (
		missing = "the request has no bearer token. Send the header Authorization: Bearer followed by the token issued for this server.\n"
		invalid = "the bearer token is malformed or unknown to this server. Send the token issued for this server, once, after Bearer.\n"
	)
	cases := []struct {
		name      string
		header    []string
		status    int
		challenge string
		body      string
		caller    string
	}{
		{"the right token", []string{"Bearer " + tokenA}, http.StatusOK, "", "", "support_bot"},
		{"another caller's token", []string{"bearer " + tokenB}, http.StatusOK, "", "", "ops_bot"},
		{"no header", nil, http.StatusUnauthorized, `Bearer realm="adaptive-pg-mcp"`, missing, ""},
		{"another scheme", []string{"Basic " + tokenA}, http.StatusUnauthorized, `Bearer realm="adaptive-pg-mcp"`, missing, ""},
		{"an empty token", []string{"Bearer"}, http.StatusUnauthorized, `Bearer realm="adaptive-pg-mcp", error="invalid_token"`, invalid, ""},
		{"a wrong token", []string{"Bearer " + unknownToken}, http.StatusUnauthorized, `Bearer realm="adaptive-pg-mcp", error="invalid_token"`, invalid, ""},
		{"two headers", []string{"Bearer " + tokenA, "Bearer " + tokenB}, http.StatusUnauthorized, `Bearer realm="adaptive-pg-mcp", error="invalid_token"`, invalid, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := New(standaloneConfig(), slog.New(slog.NewTextHandler(&logs, nil)))
			var reached bool
			h := s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if ti := auth.TokenInfoFromContext(r.Context()); ti == nil || ti.UserID != c.caller {
					t.Errorf("the handler sees token info %+v, want the caller %s", ti, c.caller)
				}
				if v := r.Header.Values("Authorization"); len(v) != 0 {
					t.Errorf("the handler still sees the Authorization header %q", v)
				}
			}))
			r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
			for _, v := range c.header {
				r.Header.Add("Authorization", v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.status || w.Header().Get("WWW-Authenticate") != c.challenge || w.Body.String() != c.body {
				t.Errorf("answer %d %q %q, want %d %q %q", w.Code, w.Header().Get("WWW-Authenticate"), w.Body.String(), c.status, c.challenge, c.body)
			}
			if reached != (c.status == http.StatusOK) {
				t.Errorf("the handler ran: %v, want %v", reached, c.status == http.StatusOK)
			}
			if c.status != http.StatusOK && !strings.Contains(logs.String(), "level=WARN") {
				t.Errorf("a refusal logs nothing at WARN:\n%s", logs.String())
			}
			for _, tok := range []string{tokenA, tokenB, unknownToken} {
				if strings.Contains(logs.String(), tok) || strings.Contains(w.Body.String(), tok) {
					t.Errorf("a log line or the answer shows a token:\n%s\n%s", logs.String(), w.Body.String())
				}
			}
		})
	}
}

func TestNewToken(t *testing.T) {
	form := regexp.MustCompile(`^apm_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for range 20 {
		token, hash, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if !form.MatchString(token) {
			t.Errorf("token %q is not apm_ and 43 base64url characters", token)
		}
		sum := sha256.Sum256([]byte(token))
		if hash != hex.EncodeToString(sum[:]) {
			t.Errorf("hash %s is not the lowercase hex SHA-256 of the token", hash)
		}
		if caller, ok := match([]credential{{hash: sum, caller: "c"}}, token); !ok || caller != "c" {
			t.Errorf("a new token does not pass the token check against its own hash")
		}
		if seen[token] {
			t.Errorf("NewToken returned %s twice", token)
		}
		seen[token] = true
	}
}
