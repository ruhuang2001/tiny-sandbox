package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

// runFlags are the creation and execution options of `sandboxctl run`.
//
// given records which flags the caller actually passed. A numeric flag's value
// cannot answer that question: for timeout and idle-timeout an explicit 0 is
// meaningful (it disables the timeout), and for every numeric flag an explicit
// -1 must be a usage error rather than "not given". Presence is therefore
// tracked separately, from fs.Visit.
type runFlags struct {
	sandboxID     string
	image         string
	memoryMB      int
	pidsLimit     int
	nanoCPUs      int64
	network       string
	timeout       int
	timeoutAction string
	idleTimeout   int
	idleAction    string
	execTimeout   int
	remove        bool

	given map[string]bool
}

// set reports whether the caller passed the named flag.
func (rf *runFlags) set(name string) bool { return rf.given[name] }

// runCommand implements `sandboxctl run [flags] -- CMD [ARG...]`.
func (in *invocation) runCommand(args []string) int {
	flagsPart, command := splitCommand(args)
	rf := &runFlags{}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(in.stderr)
	fs.StringVar(&rf.sandboxID, "sandbox", "", "use an existing sandbox")
	fs.StringVar(&rf.image, "image", "", "image for a new sandbox")
	fs.IntVar(&rf.memoryMB, "memory", 0, "memory limit in MB")
	fs.IntVar(&rf.pidsLimit, "pids-limit", 0, "task limit in the sandbox")
	fs.Int64Var(&rf.nanoCPUs, "nano-cpus", 0, "CPU limit in units of 1e-9 CPUs (0 is unlimited)")
	fs.StringVar(&rf.network, "network", "", "bridge or none")
	fs.IntVar(&rf.timeout, "timeout", 0, "lifetime timeout in seconds (0 disables)")
	fs.StringVar(&rf.timeoutAction, "timeout-action", "", "kill or pause")
	fs.IntVar(&rf.idleTimeout, "idle-timeout", 0, "idle timeout in seconds (0 disables)")
	fs.StringVar(&rf.idleAction, "idle-action", "", "kill or pause")
	fs.IntVar(&rf.execTimeout, "exec-timeout", 0, "timeout for this command in seconds (0 disables)")
	fs.BoolVar(&rf.remove, "rm", false, "kill the sandbox after the command")
	if err := fs.Parse(flagsPart); err != nil {
		return 2
	}
	command = append(command, fs.Args()...)

	rf.given = make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { rf.given[f.Name] = true })

	if rf.set("memory") && rf.memoryMB < 0 {
		_, _ = fmt.Fprintln(in.stderr, "sandboxctl: --memory must not be negative")
		return 2
	}
	if rf.set("timeout") && rf.timeout < 0 {
		_, _ = fmt.Fprintln(in.stderr, "sandboxctl: --timeout must not be negative; use 0 to disable")
		return 2
	}
	if rf.set("idle-timeout") && rf.idleTimeout < 0 {
		_, _ = fmt.Fprintln(in.stderr, "sandboxctl: --idle-timeout must not be negative; use 0 to disable")
		return 2
	}
	if rf.set("exec-timeout") && rf.execTimeout < 0 {
		_, _ = fmt.Fprintln(in.stderr, "sandboxctl: --exec-timeout must not be negative; use 0 to disable")
		return 2
	}
	if rf.set("pids-limit") && rf.pidsLimit <= 0 {
		_, _ = fmt.Fprintln(in.stderr, "sandboxctl: --pids-limit must be positive")
		return 2
	}
	if rf.set("nano-cpus") && rf.nanoCPUs < 0 {
		_, _ = fmt.Fprintln(in.stderr, "sandboxctl: --nano-cpus must not be negative; use 0 for no limit")
		return 2
	}
	if rf.network != "" && rf.network != sandbox.NetworkBridge && rf.network != sandbox.NetworkNone {
		_, _ = fmt.Fprintf(in.stderr, "sandboxctl: --network must be %q or %q\n",
			sandbox.NetworkBridge, sandbox.NetworkNone)
		return 2
	}

	if len(command) == 0 {
		_, _ = fmt.Fprintln(in.stderr, "sandboxctl: run needs a command, e.g. sandboxctl run -- ls -l")
		return 2
	}

	// `run -- code 'PYTHON'` is shorthand for python3 -c. A new sandbox for
	// it needs a Python image, unless one was asked for.
	if command[0] == "code" {
		if len(command) != 2 {
			_, _ = fmt.Fprintln(in.stderr, "sandboxctl: code takes exactly one argument: the Python program")
			return 2
		}
		command = []string{"python3", "-c", command[1]}
		if rf.image == "" && rf.sandboxID == "" {
			rf.image = "python:3.12-alpine"
		}
	}

	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}

	ctx, stop := contextWithSignals()
	defer stop()

	created := false
	sandboxID := rf.sandboxID
	if sandboxID == "" {
		if err := checkNoCreationFlags(rf); err != nil {
			return in.fail(err)
		}
		sb, err := client.CreateSandbox(ctx, rf.createRequest())
		if err != nil {
			return in.fail(err)
		}
		created = true
		sandboxID = sb.ID
		_, _ = fmt.Fprintf(in.stderr, "sandbox: %s (created; remove with: sandboxctl kill %s)\n", sb.ID, sb.ID)
	} else {
		if err := checkNoCreationFlags(rf); err != nil {
			return in.fail(err)
		}
		_, _ = fmt.Fprintf(in.stderr, "sandbox: %s\n", sandboxID)
	}

	startReq := sandbox.ExecuteRequest{Command: command}
	if rf.set("exec-timeout") {
		startReq.TimeoutSeconds = &rf.execTimeout
	}
	started, err := client.StartExecution(ctx, sandboxID, startReq)
	if err != nil {
		return in.startFailed(client, sandboxID, created, rf.remove, err)
	}
	_, _ = fmt.Fprintf(in.stderr, "execution: %s\n", started.ID)

	final, streamErr := client.StreamExecution(ctx, sandboxID, started.ID, func(ev sandbox.Event) error {
		switch ev.Type {
		case sandbox.EventStdout:
			_, err := io.WriteString(in.stdout, ev.Data)
			return err
		case sandbox.EventStderr:
			_, err := io.WriteString(in.stderr, ev.Data)
			return err
		}
		return nil
	})
	if final == nil {
		final = started
	}
	in.printExecutionStatus(final)

	if rf.remove && created {
		in.killQuietly(client, sandboxID)
	}

	if streamErr != nil {
		return in.fail(streamErr)
	}
	if err := outcomeExit(final); err != nil {
		return in.fail(err)
	}
	return 0
}

// startFailed handles a start that did not go through. When the start was
// canceled, the sandbox the CLI just created has no reason to survive.
func (in *invocation) startFailed(client *sandbox.Client, sandboxID string, created, remove bool, err error) int {
	if created && (remove || errors.Is(err, sandbox.ErrCanceled)) {
		in.killQuietly(client, sandboxID)
	}
	return in.fail(err)
}

// killQuietly kills a sandbox the CLI created, ignoring the error: the command
// outcome is what the caller is reading.
func (in *invocation) killQuietly(client *sandbox.Client, sandboxID string) {
	kctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = client.KillSandbox(kctx, sandboxID)
}

// outcomeExit maps a completed execution to the CLI's error, or nil.
func outcomeExit(ex *sandbox.Execution) error {
	if ex == nil {
		return nil
	}
	if ex.TerminationReason != nil && *ex.TerminationReason == sandbox.ReasonExited {
		if ex.ExitCode == nil || *ex.ExitCode == 0 {
			return nil
		}
	}
	// Reuse the SDK's mapping so the CLI and SDK agree on reasons and output.
	code := 0
	if ex.ExitCode != nil {
		code = *ex.ExitCode
	}
	reason := sandbox.Reason("")
	if ex.TerminationReason != nil {
		reason = *ex.TerminationReason
	}
	return &sandbox.Error{
		Code:      string(reason),
		Message:   fmt.Sprintf("command ended (%s) with exit code %d", reason, code),
		Execution: ex,
	}
}

// createRequest turns the flags into the SDK's request. A flag is forwarded
// when the caller gave it, so --timeout 0 reaches the server and disables the
// lifetime timeout instead of silently meaning "use the default".
func (rf *runFlags) createRequest() sandbox.CreateSandboxRequest {
	req := sandbox.CreateSandboxRequest{}
	if rf.image != "" {
		req.Image = &rf.image
	}
	if rf.set("memory") {
		req.MemoryMB = &rf.memoryMB
	}
	if rf.set("pids-limit") {
		req.PidsLimit = &rf.pidsLimit
	}
	if rf.set("nano-cpus") {
		req.NanoCPUs = &rf.nanoCPUs
	}
	if rf.network != "" {
		req.Network = &rf.network
	}
	if rf.set("timeout") {
		req.TimeoutSeconds = &rf.timeout
	}
	if rf.timeoutAction != "" {
		action := sandbox.Action(rf.timeoutAction)
		req.TimeoutAction = &action
	}
	if rf.set("idle-timeout") {
		req.IdleTimeoutSeconds = &rf.idleTimeout
	}
	if rf.idleAction != "" {
		action := sandbox.Action(rf.idleAction)
		req.IdleTimeoutAction = &action
	}
	return req
}

// checkNoCreationFlags rejects creation flags combined with --sandbox: they
// would silently do nothing.
func checkNoCreationFlags(rf *runFlags) error {
	var used []string
	if rf.image != "" {
		used = append(used, "--image")
	}
	if rf.set("memory") {
		used = append(used, "--memory")
	}
	if rf.set("pids-limit") {
		used = append(used, "--pids-limit")
	}
	if rf.set("nano-cpus") {
		used = append(used, "--nano-cpus")
	}
	if rf.network != "" {
		used = append(used, "--network")
	}
	if rf.set("timeout") {
		used = append(used, "--timeout")
	}
	if rf.timeoutAction != "" {
		used = append(used, "--timeout-action")
	}
	if rf.set("idle-timeout") {
		used = append(used, "--idle-timeout")
	}
	if rf.idleAction != "" {
		used = append(used, "--idle-action")
	}
	if len(used) == 0 {
		return nil
	}
	// A `code` shorthand sets --image implicitly, so only report flags the
	// caller actually passed; the implicit image is applied after this check
	// for a new sandbox.
	if rf.sandboxID == "" {
		return nil
	}
	return fmt.Errorf("%s cannot be combined with --sandbox", strings.Join(used, ", "))
}
