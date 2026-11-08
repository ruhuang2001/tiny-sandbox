package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

var version = "dev"

const usage = `sandboxctl — developer CLI for tiny-sandbox

usage:
  sandboxctl <command> [flags]

commands:
  doctor                                 check config, connectivity, docker
  run [flags] -- CMD [ARG...]            run a command in a sandbox
  run [flags] -- code 'PYTHON'           shorthand for python3 -c
  attach SANDBOX EXECUTION               stream an execution from the start
  logs SANDBOX EXECUTION                 print retained output
  inspect SANDBOX                        print one sandbox
  list                                   list sandboxes
  pause SANDBOX                          pause a sandbox
  resume SANDBOX                         resume a sandbox
  kill SANDBOX                           kill a sandbox
  version                                print the build version
  help                                   print this message

run flags:
  --sandbox ID          use an existing sandbox instead of creating one
  --image NAME          image for a new sandbox
  --memory MB           memory limit
  --timeout S           lifetime timeout in seconds
  --timeout-action A    kill or pause
  --idle-timeout S      idle timeout in seconds
  --idle-action A       kill or pause
  --rm                  kill the sandbox after the command

global flags (before or after the command):
  --domain URL          server base URL
  --api-key KEY         API key

Configuration:
  SANDBOX_DOMAIN   server base URL (default http://localhost:8080)
  SANDBOX_API_KEY  API key, when the server requires one
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main with its streams injected, so the end-to-end suite can assert
// both what a command prints and what it returns.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	g := &globals{}
	// Global flags may appear before the command; Flags stops at the first
	// non-flag argument, which is the command name.
	if code, done := g.parse(args, stdout, stderr); done {
		return code
	}
	args = g.rest
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	command, rest := args[0], args[1:]
	in := &invocation{stdout: stdout, stderr: stderr, globals: g}

	switch command {
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "doctor":
		return in.doctor(rest)
	case "run":
		return in.runCommand(rest)
	case "attach":
		return in.attach(rest)
	case "logs":
		return in.logs(rest)
	case "inspect":
		return in.inspect(rest)
	case "list":
		return in.list(rest)
	case "pause", "resume", "kill":
		return in.lifecycle(command, rest)
	default:
		fmt.Fprintf(stderr, "sandboxctl: unknown command %q\n\n%s", command, usage)
		return 2
	}
}

// globals holds the flags that apply to every command.
type globals struct {
	domain string
	apiKey string
	rest   []string
}

// parse reads global flags up to the command name (or "--").
func (g *globals) parse(args []string, stdout, stderr io.Writer) (int, bool) {
	fs := flag.NewFlagSet("sandboxctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&g.domain, "domain", "", "server base URL (default $SANDBOX_DOMAIN)")
	fs.StringVar(&g.apiKey, "api-key", "", "API key (default $SANDBOX_API_KEY)")
	if err := fs.Parse(args); err != nil {
		return 2, true
	}
	g.rest = fs.Args()
	return 0, false
}

// invocation is one command with its streams and configuration.
type invocation struct {
	stdout  io.Writer
	stderr  io.Writer
	globals *globals
}

// client builds the SDK client from the global flags and environment.
func (in *invocation) client() (*sandbox.Client, error) {
	cfg := sandbox.Config{Domain: in.globals.domain, APIKey: in.globals.apiKey}
	c, err := sandbox.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// contextWithSignals cancels work on Ctrl+C or SIGTERM.
func contextWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// fail prints an error the way the contract specifies and returns the exit
// code for it.
func (in *invocation) fail(err error) int {
	fmt.Fprintf(in.stderr, "sandboxctl: %s\n", messageOf(err))
	if hint := hintFor(err); hint != "" {
		fmt.Fprintf(in.stderr, "hint: %s\n", hint)
	}
	return exitCode(err)
}

// messageOf strips the SDK's "sandbox: " prefix, because the CLI speaks for
// itself.
func messageOf(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "sandbox: ")
	return msg
}

// exitCode maps an SDK error to the CLI's exit code: the command's own code
// when it is a normal exit status, 130 for cancellation, 1 for everything
// else.
func exitCode(err error) int {
	var sdkErr *sandbox.Error
	if !errors.As(err, &sdkErr) {
		return 1
	}
	if sdkErr.Execution != nil && sdkErr.Execution.TerminationReason != nil &&
		*sdkErr.Execution.TerminationReason == sandbox.ReasonExited &&
		sdkErr.Execution.ExitCode != nil {
		code := *sdkErr.Execution.ExitCode
		// 1–125 are the command's own failures; 126/127 mean the sandbox
		// could not run it at all, and every other outcome is a sandbox
		// failure, so they all collapse to 1.
		if code >= 1 && code <= 125 {
			return code
		}
		return 1
	}
	if errors.Is(err, sandbox.ErrCanceled) {
		return 130
	}
	return 1
}

// hintFor suggests the next step for the conditions cli.md lists.
func hintFor(err error) string {
	switch {
	case errors.Is(err, sandbox.ErrOOM):
		return "increase --memory or reduce the command's memory use"
	case errors.Is(err, sandbox.ErrTimeout):
		return "increase --timeout"
	case errors.Is(err, sandbox.ErrEvicted):
		return "the container was stopped outside sandboxd"
	case errors.Is(err, sandbox.ErrBusy):
		return "wait for the running execution or cancel it"
	case errors.Is(err, sandbox.ErrUnauthorized):
		return "set a valid SANDBOX_API_KEY or --api-key"
	case errors.Is(err, sandbox.ErrNotFound):
		return "check the sandbox and execution IDs"
	case isConnectionRefused(err):
		return "start sandboxd or set SANDBOX_DOMAIN"
	}
	return ""
}

// isConnectionRefused detects a transport failure to reach the server.
func isConnectionRefused(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var sdkErr *sandbox.Error
	if errors.As(err, &sdkErr) && sdkErr.Code == "transport" {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

// ---------------------------------------------------------------- output

// printSandbox writes the stable one-line form used by list and lifecycle
// commands.
func (in *invocation) printSandbox(sb *sandbox.Sandbox) {
	fmt.Fprintf(in.stdout, "%s %s %s %s\n", sb.ID, sb.State, sb.Image,
		sb.CreatedAt.UTC().Format(time.RFC3339))
}

// printExecutionStatus writes the status line the contract fixes.
func (in *invocation) printExecutionStatus(ex *sandbox.Execution) {
	if ex == nil {
		return
	}
	reason := "unknown"
	if ex.TerminationReason != nil {
		reason = string(*ex.TerminationReason)
	}
	exit := "none"
	if ex.ExitCode != nil {
		exit = fmt.Sprintf("%d", *ex.ExitCode)
	}
	fmt.Fprintf(in.stderr, "execution %s: %s (%s), exit %s\n", ex.ID, ex.State, reason, exit)
}

// ---------------------------------------------------------------- helpers

// splitCommand separates command arguments from flags: everything after "--"
// is the command, and a bare command with no "--" is accepted too, because
// `sandboxctl run -- ls` is the documented form but `sandboxctl run ls` has no
// other meaning.
func splitCommand(args []string) (flags, command []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// splitArgs splits trailing positional arguments from flags for the commands
// that take IDs before flags.
func splitArgs(args []string) (positional, flags []string) {
	i := 0
	for ; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			break
		}
		positional = append(positional, args[i])
	}
	return positional, args[i:]
}

// isLocalDomain reports whether a domain points at this machine, which decides
// whether doctor can check Docker.
func isLocalDomain(domain string) bool {
	u, err := url.Parse(domain)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// healthProbe is doctor's only direct HTTP call. Everything else goes through
// the SDK, so no path bypasses its start recovery.
func healthProbe(ctx context.Context, domain string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(domain, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %d", resp.StatusCode)
	}
	return nil
}
