// Command adaptive-pg-mcp serves PostgreSQL databases over MCP and makes
// caller tokens.
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
	"strconv"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
	"github.com/strazahq/adaptive-pg-mcp/internal/core"
)

// usage is the help text.
const usage = `adaptive-pg-mcp serves PostgreSQL databases over MCP.

Commands:
  adaptive-pg-mcp serve [-config ./server.yaml]
      Run the start checks, then serve on stdio or Streamable HTTP, as listen says.
      On stdio, the caller's token comes from ADAPTIVE_PG_MCP_TOKEN.
  adaptive-pg-mcp check [-config ./server.yaml]
      Run the start checks without serving, and print the findings.
      Exits 1 when serve would refuse to start.
  adaptive-pg-mcp token
      Print a new token and its SHA-256 for access.yaml, gateway_tokens_sha256 or
      listing_tokens_sha256.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// run runs one command and returns its exit code: 0 on success, 1 when the
// command fails, and 2 on a usage error. Only token, check and help write
// to stdout.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr, getenv)
	case "check":
		return check(ctx, args[1:], stdout, stderr, getenv)
	case "token":
		return token(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "adaptive-pg-mcp has no command %q. Use serve, check or token.\n", args[0])
	return 2
}

// serve loads the config, runs the start checks, and serves on stdio or on
// the address in listen until ctx ends.
func serve(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "./server.yaml", "the path to server.yaml")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "serve takes only -config, but got %q too. Remove the extra arguments.\n", flags.Args())
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
			log.Error(fmt.Sprintf("adaptive-pg-mcp cannot listen on %s, set in %s: %v. Free the address, or change listen.", cfg.Listen, *path, err))
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

// check loads the config and runs the start checks without serving. It prints
// each finding on stdout as a refusal: or a report: line, then a check: line
// that says whether serve would start, and exits 1 when it would not.
func check(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "./server.yaml", "the path to server.yaml")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "check takes only -config, but got %q too. Remove the extra arguments.\n", flags.Args())
		return 2
	}
	cfg, err := config.Load(*path, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	srv := core.New(cfg, slog.New(slog.NewTextHandler(stderr, nil)))
	findings := srv.Check(ctx)
	srv.Close()
	refused := 0
	for _, f := range findings {
		kind := "report: "
		if f.Refuses {
			kind = "refusal: "
			refused++
		}
		fmt.Fprintln(stdout, kind+f.Text)
	}
	switch refused {
	case 0:
		fmt.Fprintln(stdout, "check: no refusals, so adaptive-pg-mcp would start.")
		return 0
	case 1:
		fmt.Fprintln(stdout, "check: 1 refusal above, so adaptive-pg-mcp would not start. Fix it, and run adaptive-pg-mcp check again.")
	default:
		fmt.Fprintln(stdout, "check: "+strconv.Itoa(refused)+" refusals above, so adaptive-pg-mcp would not start. Fix them, and run adaptive-pg-mcp check again.")
	}
	return 1
}

// token prints a new token and its SHA-256 on stdout, and how to use them on
// stderr.
func token(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "token takes no arguments, but got %q. Run adaptive-pg-mcp token alone.\n", args)
		return 2
	}
	tok, hash, err := core.NewToken()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "token: %s\ntoken_sha256: %s\n", tok, hash)
	fmt.Fprintln(stderr, "Give the token to its caller, and put token_sha256 in access.yaml, or in gateway_tokens_sha256 for a Straza token, or listing_tokens_sha256 for its listing token. The token is not stored and will not be shown again.")
	return 0
}
