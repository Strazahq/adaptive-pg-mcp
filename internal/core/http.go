package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// endpointPath is the one path that serves MCP over Streamable HTTP.
	endpointPath = "/mcp"
	// maxRequestBody is the largest request body the server reads, in bytes.
	maxRequestBody = 1 << 20
	// readHeaderTimeout bounds how long a client may take to send its request headers.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes a keep-alive connection that carries no request for this long.
	idleTimeout = 2 * time.Minute
	// sessionIdleTimeout closes a session of an older revision that sees no request for this long.
	sessionIdleTimeout = 30 * time.Minute
	// shutdownGrace is how long a stop waits for open requests before it closes their connections.
	shutdownGrace = 10 * time.Second
)

// originRefused answers a request that carries an Origin header.
const originRefused = "the request comes from a web page, as its Origin header shows, and adaptive-pg-mcp serves no web page, so it refused the request. Call the server from an MCP client that sends no Origin header."

// Handler is the Streamable HTTP handler. Every request passes the Origin
// check and then the token check, on every path and every revision, before
// the SDK sees it. A request that names a revision with protocol sessions, or
// none, goes to a stateful SDK handler that binds each session to the caller
// that opened it. Every other request goes to a stateless SDK handler, which
// serves 2026-07-28 and later, where the protocol has no sessions.
func (s *Server) Handler() http.Handler {
	stateful := mcp.NewStreamableHTTPHandler(s.server, &mcp.StreamableHTTPOptions{
		Logger:              s.log,
		MaxRequestBodyBytes: maxRequestBody,
		SessionTimeout:      sessionIdleTimeout,
	})
	stateless := mcp.NewStreamableHTTPHandler(s.server, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		Logger:                       s.log,
		MaxRequestBodyBytes:          maxRequestBody,
		PropagateRequestCancellation: true,
	})
	sessions := &mcp.StreamableServerTransport{}
	mux := http.NewServeMux()
	mux.Handle(endpointPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("Mcp-Protocol-Version")
		if v == "" || sessions.SupportsProtocolVersion(v) {
			stateful.ServeHTTP(w, r)
			return
		}
		stateless.ServeHTTP(w, r)
	}))
	return s.refuseOrigin(s.authenticate(mux))
}

// server returns the one MCP server for every HTTP request.
func (s *Server) server(*http.Request) *mcp.Server { return s.mcp }

// refuseOrigin answers 403 to any request that carries an Origin header. The
// server serves no web page, so no page has an origin it may call from, and a
// page that reaches it through DNS rebinding is turned away before the token check.
func (s *Server) refuseOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Origin")) > 0 {
			s.log.Warn("refused an HTTP request from a web page", "remote", r.RemoteAddr, "method", r.Method)
			http.Error(w, originRefused, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Serve runs the start checks, and then serves Streamable HTTP on ln until
// ctx ends and stops within shutdownGrace. When a start check refuses, it
// closes ln without serving and returns a sentence.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := s.start(ctx); err != nil {
		_ = ln.Close()
		return err
	}
	defer s.Close()
	hs := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	done := make(chan error, 1)
	go func() { done <- hs.Serve(ln) }()
	s.log.Info("serving Streamable HTTP", "address", "http://"+ln.Addr().String()+endpointPath, "mode", string(s.cfg.Mode))
	select {
	case err := <-done:
		return fmt.Errorf("adaptive-pg-mcp stopped serving HTTP on %s: %v. Check that the address is still the machine's, and start it again.", ln.Addr(), err)
	case <-ctx.Done():
	}
	stop, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := hs.Shutdown(stop); err != nil {
		_ = hs.Close()
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("adaptive-pg-mcp did not stop cleanly: %v. Check the log above for the request it was serving.", err)
	}
	s.log.Info("stopped serving Streamable HTTP")
	return nil
}
