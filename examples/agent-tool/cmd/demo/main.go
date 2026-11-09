// Command agent-tool runs the run_python tool against a real sandboxd, or
// against an in-process demo server when none is configured.
//
// Against a real server:
//
//	SANDBOX_DOMAIN=http://127.0.0.1:8080 go run ./examples/agent-tool -- -code 'print(2**10)'
//
// With no server, the demo starts the real HTTP handler over the in-memory
// backend, so `go run ./examples/agent-tool` works with no Docker and no setup.
// The point is that the tool code path is identical in both cases: it only
// knows the SDK.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"

	agenttool "github.com/ruhuang2001/tiny-sandbox/examples/agent-tool"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func main() {
	code := flag.String("code", "print(2 ** 10)", "Python source to run")
	timeout := flag.Int("timeout", 60, "sandbox lifetime in seconds")
	showDefinition := flag.Bool("definition", false, "print the tool definition as JSON and exit")
	flag.Parse()

	if *showDefinition {
		printJSON(agenttool.ToolDefinition())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, cleanup, err := clientFor(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tool: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	tool := agenttool.New(client)
	result := tool.Run(ctx, agenttool.Input{Code: *code, TimeoutSeconds: *timeout})

	// A real runtime would send this as a tool-result message. Printing it
	// shows exactly what the model would read.
	printJSON(result)
	if result.IsError {
		os.Exit(1)
	}
}

// clientFor returns a client for the configured server, or for an in-process
// demo server when SANDBOX_DOMAIN is unset.
func clientFor(ctx context.Context) (*sandbox.Client, func(), error) {
	if domain := os.Getenv("SANDBOX_DOMAIN"); domain != "" {
		client, err := sandbox.NewClient(sandbox.Config{Domain: domain})
		return client, func() {}, err
	}

	// No server configured: serve the contract in-process over the in-memory
	// backend. This is the same handler the binary uses, so the demo exercises
	// the real JSON and streaming paths.
	backend := membackend.New()
	backend.Programs["python3"] = memDemoPython
	store := daemon.New(backend)
	srv := httptest.NewServer(daemon.NewHandler(store, "demo"))
	_ = ctx // the demo server lives for the process

	client, err := sandbox.NewClient(sandbox.Config{Domain: srv.URL})
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	fmt.Fprintln(os.Stderr, "agent-tool: no SANDBOX_DOMAIN set; using the in-memory demo server")
	return client, srv.Close, nil
}

// memDemoPython is a stand-in for a real interpreter, so the demo needs no
// Docker. It recognises the one expression the demo prints; anything else
// reports a nonzero exit, which the tool maps to nonzero_exit.
func memDemoPython(_ context.Context, args []string) (string, string, int) {
	if len(args) == 2 && args[0] == "-c" && args[1] == "print(2 ** 10)" {
		return "1024\n", "", 0
	}
	return "", "name 'unknown' is not defined\n", 1
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tool: encode: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}
