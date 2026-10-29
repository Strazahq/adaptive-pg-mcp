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
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes a keep-alive connection that carries no request for
	// this long.
	idleTimeout = 2 * time.Minute
	// sessionIdleTimeout closes a session of an older revision that sees no
	// request for this long.
	sessionIdleTimeout = 30 * time.Minute
	// shutdownGrace is how long a stop waits for open requests before it
	// closes their connections.
	shutdownGrace = 10 * time.Second
)

// originRefused is the response body for a request that carries an Origin
// header.
const originRefused = "the request has an Origin header, so it comes from a web page, and web pages may not call this server. Use an MCP client that sends no Origin header."

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

// refuseOffLoopback refuses plain HTTP on a listener that is not on a
// loopback address, unless allow_plain_http: true accepts it. The config
// accepts localhost by name, which the system resolves when the listener
// binds, so only the bound address shows where plain HTTP would serve.
func (s *Server) refuseOffLoopback(ln net.Listener) error {
	if s.cfg.TLS != nil || s.cfg.AllowPlainHTTP {
		return nil
	}
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if ip := net.ParseIP(host); err == nil && ip != nil && ip.IsLoopback() {
		return nil
	}
	_, port, _ := net.SplitHostPort(s.cfg.Listen)
	return fmt.Errorf("adaptive-pg-mcp will not start, because listen %s resolved to %s, not a loopback address, and without tls the tokens would cross the network in cleartext. Make localhost resolve to 127.0.0.1, set listen to %s, or set tls.cert_file and tls.key_file.", s.cfg.Listen, ln.Addr(), net.JoinHostPort("127.0.0.1", port))
}

// server returns the one MCP server for every HTTP request.
func (s *Server) server(*http.Request) *mcp.Server { return s.mcp }

// refuseOrigin responds 403 to any request that carries an Origin header.
// The server serves no web page, so no origin may call it, and a page that
// reaches it through DNS rebinding is turned away before the token check.
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

// Serve runs the start checks, then serves Streamable HTTP on ln until ctx
// ends, and stops within shutdownGrace. With tls in server.yaml it serves
// HTTPS only, and reads the certificate and key again at most once per
// certReload. When plain HTTP would serve off loopback, or a start check
// refuses, it closes ln without serving and returns an error.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := s.refuseOffLoopback(ln); err != nil {
		_ = ln.Close()
		return err
	}
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
	scheme := "http://"
	if s.cfg.TLS != nil {
		pair, fault := newKeyPair(*s.cfg.TLS, s.log, time.Now())
		if fault != nil {
			_ = ln.Close()
			return errors.New("adaptive-pg-mcp will not start, because " + fault.refusal())
		}
		hs.TLSConfig = pair.tlsConfig()
		scheme = "https://"
	}
	done := make(chan error, 1)
	go func() {
		if hs.TLSConfig != nil {
			done <- hs.ServeTLS(ln, "", "")
			return
		}
		done <- hs.Serve(ln)
	}()
	s.log.Info("serving Streamable HTTP", "address", scheme+ln.Addr().String()+endpointPath, "mode", string(s.cfg.Mode))
	select {
	case err := <-done:
		return fmt.Errorf("adaptive-pg-mcp stopped serving HTTP on %s: %v. Check that the address still belongs to this machine, and start the server again.", ln.Addr(), err)
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
