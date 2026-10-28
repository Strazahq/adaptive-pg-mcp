// Command adaptive-pg-mcp serves PostgreSQL databases over MCP, and makes the tokens its callers use.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/core"
)

// usage is the help text for the commands.
const usage = `adaptive-pg-mcp serves PostgreSQL databases over MCP.

Commands:
  adaptive-pg-mcp serve [-config ./server.yaml]
      Serve the databases of server.yaml over stdio or Streamable HTTP, as its listen key says.
      On stdio, the caller's token comes from the environment variable ADAPTIVE_PG_MCP_TOKEN.
  adaptive-pg-mcp token
      Print a new token and its SHA-256 for access.yaml or gateway_tokens_sha256.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// run runs one command and returns its exit code: 0 on success, 1 when the
// command fails, and 2 when it is called wrongly. Only token writes to stdout.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr, getenv)
	case "token":
		return token(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "adaptive-pg-mcp has no command %q. Run adaptive-pg-mcp serve or adaptive-pg-mcp token.\n", args[0])
	return 2
}

// serve loads the config and serves it on stdio or on the address in listen until ctx ends.
func serve(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "./server.yaml", "the path to server.yaml")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "serve takes only -config, and it got %q as well. Remove the extra arguments.\n", flags.Args())
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	cfg, err := config.Load(*path, getenv)
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	srv := core.New(cfg, log)
	if cfg.Listen == config.Stdio {
		err = srv.ServeStdio(ctx, getenv(core.TokenEnv), &mcp.StdioTransport{})
	} else {
		var ln net.Listener
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			log.Error(fmt.Sprintf("adaptive-pg-mcp cannot listen on %s, which %s names in listen: %v. Free the address, or set listen to another one.", cfg.Listen, *path, err))
			return 1
		}
		err = srv.Serve(ctx, ln)
	}
	if err != nil && ctx.Err() == nil {
		log.Error(err.Error())
		return 1
	}
	return 0
}

// token prints a new token and its SHA-256 on stdout, and how to use them on stderr.
func token(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "token takes no arguments, and it got %q. Run adaptive-pg-mcp token alone.\n", args)
		return 2
	}
	tok, hash, err := core.NewToken()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "token: %s\ntoken_sha256: %s\n", tok, hash)
	fmt.Fprintln(stderr, "Give the token to its caller, and put token_sha256 in access.yaml, or in gateway_tokens_sha256 for a token Straza holds. adaptive-pg-mcp keeps neither, so this is the only time the token is shown.")
	return 0
}
