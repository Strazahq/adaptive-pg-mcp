//go:build pg

package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pgtest"
)

// sleeping counts the backends that run a statement holding marker right now, and names their states.
func sleeping(t *testing.T, admin *pgx.Conn, marker string) (active int, states []string) {
	t.Helper()
	rows, err := admin.Query(t.Context(), "SELECT state FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND query LIKE '%' || $1 || '%'", marker)
	if err != nil {
		t.Fatalf("reading pg_stat_activity: %v", err)
	}
	var state *string
	_, err = pgx.ForEachRow(rows, []any{&state}, func() error {
		s := "none"
		if state != nil {
			s = *state
		}
		states = append(states, s)
		if s == "active" {
			active++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading pg_stat_activity: %v", err)
	}
	return active, states
}

func TestACancelledCallStopsItsQueryInPostgres(t *testing.T) {
	for _, s := range pgtest.Servers(t) {
		t.Run(s.String(), func(t *testing.T) {
			l := pgtest.NewLogin(t, s, loginPassword)
			d := pgtest.NewDatabase(t, s)
			env := map[string]string{"HELPDESK_DSN": l.DSN(t, d)}
			overHTTPServer := New(loadData(t, "read", "", helpdeskSingle, env), slog.New(slog.DiscardHandler))
			for _, f := range overHTTPServer.Check(t.Context()) {
				if f.Refuses {
					t.Fatalf("Check refused: %s", f.Text)
				}
			}
			t.Cleanup(overHTTPServer.Close)
			ts := httptest.NewServer(overHTTPServer.Handler())
			t.Cleanup(ts.Close)
			overStdioServer := New(loadData(t, "read", "", helpdeskSingle, env), slog.New(slog.DiscardHandler))
			admin := s.Connect(t)

			for _, v := range mcp.SupportedProtocolVersions() {
				for _, transport := range []string{"stdio", "http"} {
					t.Run(v+"/"+transport, func(t *testing.T) {
						var cs *mcp.ClientSession
						if transport == "stdio" {
							cs = overStdio(t, overStdioServer, gatewayToken, v)
						} else {
							var err error
							if cs, err = overHTTP(t, ts.URL+endpointPath, gatewayToken, v); err != nil {
								t.Fatalf("connect over HTTP: %v", err)
							}
						}
						b := make([]byte, 8)
						_, _ = rand.Read(b)
						marker := "sleep_" + hex.EncodeToString(b)
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						done := make(chan error, 1)
						go func() {
							_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "query_sql", Arguments: map[string]any{"database": "helpdesk", "sql": "SELECT pg_sleep(20) AS " + marker}})
							done <- err
						}()

						deadline := time.Now().Add(10 * time.Second)
						for active, _ := sleeping(t, admin, marker); active == 0; active, _ = sleeping(t, admin, marker) {
							if time.Now().After(deadline) {
								t.Fatalf("no backend ran the statement within 10s")
							}
							time.Sleep(20 * time.Millisecond)
						}
						cancelled := time.Now()
						cancel()
						select {
						case err := <-done:
							if !errors.Is(err, context.Canceled) {
								t.Errorf("the cancelled call returned %v, want context.Canceled", err)
							}
						case <-time.After(5 * time.Second):
							t.Fatalf("the cancelled call did not return within 5s")
						}
						var states []string
						for active := 1; active > 0; {
							if time.Since(cancelled) > 5*time.Second {
								t.Fatalf("a backend still runs pg_sleep(20) 5s after the cancel, in the states %v", states)
							}
							time.Sleep(20 * time.Millisecond)
							active, states = sleeping(t, admin, marker)
						}
						stopped := time.Since(cancelled).Round(time.Millisecond)
						for len(states) > 0 {
							if time.Since(cancelled) > 5*time.Second {
								t.Fatalf("the backend of the cancelled call is still open 5s after the cancel, in the states %v", states)
							}
							time.Sleep(20 * time.Millisecond)
							_, states = sleeping(t, admin, marker)
						}
						t.Logf("the backend stopped the statement %v after the cancel, and its connection was closed %v after it", stopped, time.Since(cancelled).Round(time.Millisecond))
					})
				}
			}
		})
	}
}
