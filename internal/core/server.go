package core

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// serverName is the name the server gives itself in every answer.
const serverName = "adaptive-pg-mcp"

// TokenEnv is the environment variable that holds the caller's token on stdio.
const TokenEnv = "ADAPTIVE_PG_MCP_TOKEN"

// Server is adaptive-pg-mcp for one config, served over stdio or Streamable HTTP.
type Server struct {
	cfg   *config.Config
	log   *slog.Logger
	creds []credential
	mcp   *mcp.Server
}

// New builds the server for cfg. It registers no tools yet.
func New(cfg *config.Config, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, log: log, creds: credentials(cfg)}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: serverName, Version: buildVersion()}, &mcp.ServerOptions{Logger: log})
	s.mcp.AddReceivingMiddleware(identify)
	return s
}

// buildVersion is the module version from the build info, or (devel) for a build from a working tree.
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

// stdioCallerKey carries the caller that the start check found on stdio into the session.
type stdioCallerKey struct{}

// callerKey carries the caller of one request to its handler.
type callerKey struct{}

// identify is receiving middleware that names the caller of every request and
// refuses a request that has none. Over HTTP the caller is the TokenInfo of
// that very request, never of the request that opened its session. Over stdio
// it is the caller whose token the start check matched.
func identify(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		var caller string
		if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
			caller = extra.TokenInfo.UserID
		} else {
			caller, _ = ctx.Value(stdioCallerKey{}).(string)
		}
		if caller == "" {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: "the request has no checked caller, so adaptive-pg-mcp refused it. Serve it with adaptive-pg-mcp serve, which checks the token of every caller."}
		}
		return next(context.WithValue(ctx, callerKey{}, caller), method, req)
	}
}

// callerFrom returns the caller of the request whose handler runs on ctx: the
// caller name in mode standalone, or straza in mode straza.
func callerFrom(ctx context.Context) (string, bool) {
	caller, ok := ctx.Value(callerKey{}).(string)
	return caller, ok && caller != ""
}

// ServeStdio checks token once against the configured hashes, and then serves
// that one caller on t until the client closes it or ctx ends. When the token
// is missing or matches nothing it returns a sentence and never connects t.
func (s *Server) ServeStdio(ctx context.Context, token string, t mcp.Transport) error {
	where := "the token of a caller in access.yaml"
	if s.cfg.Mode == config.Straza {
		where = "a token whose SHA-256 is in gateway_tokens_sha256"
	}
	if token == "" {
		return fmt.Errorf("%s is not set, and on stdio adaptive-pg-mcp needs it to know who calls, so it will not start. Set %s to %s in the environment the client starts the server with.", TokenEnv, TokenEnv, where)
	}
	caller, ok := match(s.creds, token)
	if !ok {
		return fmt.Errorf("the token in %s matches no configured hash, so adaptive-pg-mcp will not start. Set %s to %s.", TokenEnv, TokenEnv, where)
	}
	s.log.Info("serving one caller over stdio", "caller", caller)
	return s.mcp.Run(context.WithValue(ctx, stdioCallerKey{}, caller), t)
}
