package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// serverName is the implementation name the server reports to clients.
const serverName = "adaptive-pg-mcp"

// TokenEnv is the environment variable that holds the caller's token on stdio.
const TokenEnv = "ADAPTIVE_PG_MCP_TOKEN"

// Server is the MCP server for one config, served over stdio or Streamable
// HTTP.
type Server struct {
	cfg   *config.Config
	log   *slog.Logger
	creds []credential
	mcp   *mcp.Server
	// pools are the connection pools that Check opens, and nil before it runs.
	pools *pg.Pools
	// page is the most databases one page of list_databases returns.
	page int
}

// New builds the server for cfg with its built-in tools: list_databases, the
// read and write tools, and the free SQL tools that free_sql allows. It
// connects to no database until Check, Serve or ServeStdio runs.
func New(cfg *config.Config, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, log: log, creds: credentials(cfg), page: listPage}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: serverName, Version: buildVersion()}, &mcp.ServerOptions{Logger: log})
	s.mcp.AddReceivingMiddleware(identify, s.listingOnly, s.filterTools)
	s.mcp.AddTool(listDatabasesTool(), s.listDatabases)
	s.addDataTools()
	return s
}

// start runs the start checks, logs every finding, and returns an error when
// one of them refuses the start.
func (s *Server) start(ctx context.Context) error {
	refused := 0
	for _, f := range s.Check(ctx) {
		if f.Refuses {
			refused++
			s.log.Error(f.Text)
		} else {
			s.log.Warn(f.Text)
		}
	}
	if refused > 0 {
		s.Close()
		checks := strconv.Itoa(refused) + " start checks"
		if refused == 1 {
			checks = "1 start check"
		}
		return errors.New("adaptive-pg-mcp will not start, because " + checks + " refused it. Fix what the log lines above say, and run adaptive-pg-mcp check to test the fix without serving.")
	}
	s.log.Info("the start checks passed", "databases", len(s.cfg.Databases))
	return nil
}

// version is the release version, set at build time with
// -ldflags "-X github.com/strazahq/adaptive-pg-mcp/internal/core.version=1.2.3".
// The image build sets it, because a build without .git has no module version.
var version string

// buildVersion is the version set at build time, else the module version
// from the build info, or (devel) for a build from a working tree.
func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

// stdioCallerKey carries the caller whose token ServeStdio matched into the
// session.
type stdioCallerKey struct{}

// callerKey carries the caller of one request to its handler.
type callerKey struct{}

// identify is receiving middleware that names the caller of every request
// and refuses a request that has none. Over HTTP the caller comes from the
// TokenInfo of that request, never of the request that opened its session.
// Over stdio it is the caller whose token ServeStdio matched.
func identify(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		var caller string
		if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
			caller = extra.TokenInfo.UserID
		} else {
			caller, _ = ctx.Value(stdioCallerKey{}).(string)
		}
		if caller == "" {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: "the request has no checked caller. Run the server with adaptive-pg-mcp serve, which checks the token of every caller."}
		}
		return next(context.WithValue(ctx, callerKey{}, caller), method, req)
	}
}

// callerFrom returns the caller of the request whose handler runs on ctx:
// the caller name in standalone mode, or straza or straza-listing in straza
// mode.
func callerFrom(ctx context.Context) (string, bool) {
	caller, ok := ctx.Value(callerKey{}).(string)
	return caller, ok && caller != ""
}

// ServeStdio checks token once against the configured hashes, runs the start
// checks, and serves that one caller on t until the client closes it or ctx
// ends. When the token is missing or unknown, it returns an error and
// connects neither a database nor t. When a start check refuses, it returns
// an error and never connects t.
func (s *Server) ServeStdio(ctx context.Context, token string, t mcp.Transport) error {
	where := "the token of a caller in access.yaml"
	if s.cfg.Mode == config.Straza {
		where = "a token whose SHA-256 is in gateway_tokens_sha256"
	}
	if token == "" {
		return fmt.Errorf("%s is not set, so adaptive-pg-mcp cannot tell who calls on stdio and will not start. Set %s to %s, in the env of the client's MCP server entry.", TokenEnv, TokenEnv, where)
	}
	caller, ok := match(s.creds, token)
	if !ok {
		return fmt.Errorf("the token in %s matches no configured hash, so adaptive-pg-mcp will not start. Set %s to %s.", TokenEnv, TokenEnv, where)
	}
	if err := s.start(ctx); err != nil {
		return err
	}
	defer s.Close()
	s.log.Info("serving one caller over stdio", "caller", caller)
	return s.mcp.Run(context.WithValue(ctx, stdioCallerKey{}, caller), t)
}
