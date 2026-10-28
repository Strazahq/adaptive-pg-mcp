package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearer adds a bearer token to every request of the SDK client.
type bearer struct{ token string }

// RoundTrip sends r with the token in its Authorization header.
func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// overHTTP connects an SDK client to url with token, asking for revision v, or the newest when v is empty.
func overHTTP(t *testing.T, url, token, v string) (*mcp.ClientSession, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}}, MaxRetries: -1}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, tr, &mcp.ClientSessionOptions{ProtocolVersion: v})
	if err == nil {
		t.Cleanup(func() { cs.Close() })
	}
	return cs, err
}

// overStdio serves s over a pipe with token and connects an SDK client to it, asking for revision v.
func overStdio(t *testing.T, s *Server, token, v string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- s.ServeStdio(ctx, token, &mcp.IOTransport{Reader: toServer, Writer: fromServer}) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: toClient, Writer: fromClient}, &mcp.ClientSessionOptions{ProtocolVersion: v})
	if err != nil {
		cancel()
		t.Fatalf("connect over stdio: %v", err)
	}
	t.Cleanup(func() {
		cs.Close()
		cancel()
		<-served
	})
	return cs
}

// callWhoami calls whoami and returns the caller it answers.
func callWhoami(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestServesTheTargetRevision(t *testing.T) {
	raw, err := os.ReadFile("../../docs/MCP-REVISION")
	if err != nil {
		t.Fatalf("read docs/MCP-REVISION: %v", err)
	}
	revision := strings.TrimSpace(string(raw))
	if !slices.Contains(mcp.SupportedProtocolVersions(), revision) {
		t.Fatalf("the SDK serves %v, and docs/MCP-REVISION names %s", mcp.SupportedProtocolVersions(), revision)
	}
	s := New(standaloneConfig(), slog.New(slog.DiscardHandler))
	cs, err := overHTTP(t, serve(t, s), tokenA, "")
	if err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	if got := cs.InitializeResult().ProtocolVersion; got != revision {
		t.Errorf("Streamable HTTP negotiated %s, want %s from docs/MCP-REVISION", got, revision)
	}
	if got := overStdio(t, s, tokenA, "").InitializeResult().ProtocolVersion; got != revision {
		t.Errorf("stdio negotiated %s, want %s from docs/MCP-REVISION", got, revision)
	}
}

func TestSDKClientOnEveryRevision(t *testing.T) {
	for _, mode := range []struct {
		name, token, caller string
		s                   *Server
	}{
		{"standalone", tokenB, "ops_bot", New(standaloneConfig(), slog.New(slog.DiscardHandler))},
		{"straza", gatewayToken, gatewayCaller, New(strazaConfig(), slog.New(slog.DiscardHandler))},
	} {
		url := serve(t, mode.s)
		for _, v := range mcp.SupportedProtocolVersions() {
			t.Run(mode.name+"/"+v, func(t *testing.T) {
				cs, err := overHTTP(t, url, mode.token, v)
				if err != nil {
					t.Fatalf("connect over HTTP: %v", err)
				}
				if got := cs.InitializeResult().ProtocolVersion; got != v {
					t.Errorf("HTTP negotiated %s, want %s", got, v)
				}
				if got := callWhoami(t, cs); got != mode.caller {
					t.Errorf("whoami over HTTP answered %q, want %s", got, mode.caller)
				}
				if _, err := overHTTP(t, url, unknownToken, v); err == nil {
					t.Errorf("a client with an unknown token connected over HTTP")
				}
				st := overStdio(t, mode.s, mode.token, v)
				if got := st.InitializeResult().ProtocolVersion; got != v {
					t.Errorf("stdio negotiated %s, want %s", got, v)
				}
				if got := callWhoami(t, st); got != mode.caller {
					t.Errorf("whoami over stdio answered %q, want %s", got, mode.caller)
				}
			})
		}
	}
}

// untouched is a transport that records whether the server connected it.
type untouched struct{ connected bool }

// Connect records the attempt and refuses it.
func (u *untouched) Connect(context.Context) (mcp.Connection, error) {
	u.connected = true
	return nil, errors.New("the test transport refuses every connection")
}

func TestServeStdioRefusesAMissingOrWrongToken(t *testing.T) {
	const (
		standaloneHint = "the token of a caller in access.yaml"
		strazaHint     = "a token whose SHA-256 is in gateway_tokens_sha256"
	)
	missing := func(hint string) string {
		return "ADAPTIVE_PG_MCP_TOKEN is not set, and on stdio adaptive-pg-mcp needs it to know who calls, so it will not start. Set ADAPTIVE_PG_MCP_TOKEN to " + hint + " in the environment the client starts the server with."
	}
	wrong := func(hint string) string {
		return "the token in ADAPTIVE_PG_MCP_TOKEN matches no configured hash, so adaptive-pg-mcp will not start. Set ADAPTIVE_PG_MCP_TOKEN to " + hint + "."
	}
	cases := []struct {
		name  string
		s     *Server
		token string
		want  string
	}{
		{"missing in standalone", New(standaloneConfig(), slog.New(slog.DiscardHandler)), "", missing(standaloneHint)},
		{"missing in straza", New(strazaConfig(), slog.New(slog.DiscardHandler)), "", missing(strazaHint)},
		{"unknown in standalone", New(standaloneConfig(), slog.New(slog.DiscardHandler)), unknownToken, wrong(standaloneHint)},
		{"a gateway token in standalone", New(standaloneConfig(), slog.New(slog.DiscardHandler)), gatewayToken, wrong(standaloneHint)},
		{"a caller token in straza", New(strazaConfig(), slog.New(slog.DiscardHandler)), tokenA, wrong(strazaHint)},
		{"the token with a space", New(standaloneConfig(), slog.New(slog.DiscardHandler)), tokenA + " ", wrong(standaloneHint)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tr untouched
			err := c.s.ServeStdio(context.Background(), c.token, &tr)
			if err == nil || err.Error() != c.want {
				t.Errorf("ServeStdio:\n got %v\nwant %s", err, c.want)
			}
			if tr.connected {
				t.Errorf("ServeStdio connected the transport after a refused token")
			}
			if c.token != "" && strings.Contains(err.Error(), c.token) {
				t.Errorf("the refusal shows the token: %v", err)
			}
		})
	}
}

func TestARequestWithoutACallerIsRefused(t *testing.T) {
	s := New(standaloneConfig(), slog.New(slog.DiscardHandler))
	whoami(s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.mcp.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	_, err = mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err == nil || !strings.Contains(err.Error(), "the request has no checked caller, so adaptive-pg-mcp refused it.") {
		t.Errorf("a session without a checked caller connected, or failed otherwise: %v", err)
	}
}
