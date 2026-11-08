package main

import (
	"fmt"
	"io"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

// attach implements `sandboxctl attach SANDBOX EXECUTION`: stream from the
// beginning, Ctrl+C cancels the execution through the SDK.
func (in *invocation) attach(args []string) int {
	positional, flags := splitArgs(args)
	if len(flags) > 0 || len(positional) != 2 {
		fmt.Fprintln(in.stderr, "usage: sandboxctl attach SANDBOX EXECUTION")
		return 2
	}
	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	ctx, stop := contextWithSignals()
	defer stop()

	ex, err := client.StreamExecution(ctx, positional[0], positional[1], func(ev sandbox.Event) error {
		return in.writeEvent(ev)
	})
	if ex != nil {
		in.printExecutionStatus(ex)
	}
	if err != nil {
		return in.fail(err)
	}
	if err := outcomeExit(ex); err != nil {
		return in.fail(err)
	}
	return 0
}

// logs implements `sandboxctl logs SANDBOX EXECUTION`: retained output and the
// final status, without streaming.
func (in *invocation) logs(args []string) int {
	positional, flags := splitArgs(args)
	if len(flags) > 0 || len(positional) != 2 {
		fmt.Fprintln(in.stderr, "usage: sandboxctl logs SANDBOX EXECUTION")
		return 2
	}
	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	ex, err := client.GetExecution(contextWithoutSignals(), positional[0], positional[1])
	if err != nil {
		return in.fail(err)
	}
	if _, err := io.WriteString(in.stdout, ex.Stdout); err != nil {
		return in.fail(err)
	}
	if _, err := io.WriteString(in.stderr, ex.Stderr); err != nil {
		return in.fail(err)
	}
	in.printExecutionStatus(ex)
	if err := outcomeExit(ex); err != nil {
		return in.fail(err)
	}
	return 0
}

// inspect implements `sandboxctl inspect SANDBOX`.
func (in *invocation) inspect(args []string) int {
	positional, flags := splitArgs(args)
	if len(flags) > 0 || len(positional) != 1 {
		fmt.Fprintln(in.stderr, "usage: sandboxctl inspect SANDBOX")
		return 2
	}
	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	sb, err := client.GetSandbox(contextWithoutSignals(), positional[0])
	if err != nil {
		return in.fail(err)
	}
	fmt.Fprintf(in.stdout, "id: %s\n", sb.ID)
	fmt.Fprintf(in.stdout, "state: %s\n", sb.State)
	fmt.Fprintf(in.stdout, "image: %s\n", sb.Image)
	fmt.Fprintf(in.stdout, "memory_mb: %d\n", sb.MemoryMB)
	fmt.Fprintf(in.stdout, "timeout_seconds: %d\n", sb.TimeoutSeconds)
	fmt.Fprintf(in.stdout, "timeout_action: %s\n", sb.TimeoutAction)
	fmt.Fprintf(in.stdout, "idle_timeout_seconds: %d\n", sb.IdleTimeoutSeconds)
	fmt.Fprintf(in.stdout, "idle_timeout_action: %s\n", sb.IdleTimeoutAction)
	fmt.Fprintf(in.stdout, "created_at: %s\n", sb.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	if sb.ExpiresAt != nil {
		fmt.Fprintf(in.stdout, "expires_at: %s\n", sb.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"))
	} else {
		fmt.Fprintln(in.stdout, "expires_at: null")
	}
	if sb.KillReason != nil {
		fmt.Fprintf(in.stdout, "kill_reason: %s\n", *sb.KillReason)
	} else {
		fmt.Fprintln(in.stdout, "kill_reason: null")
	}
	return 0
}

// list implements `sandboxctl list` with the fixed `ID STATE IMAGE CREATED`
// line format.
func (in *invocation) list(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(in.stderr, "usage: sandboxctl list")
		return 2
	}
	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	list, err := client.ListSandboxes(contextWithoutSignals())
	if err != nil {
		return in.fail(err)
	}
	for _, sb := range list {
		in.printSandbox(sb)
	}
	return 0
}

// lifecycle implements pause, resume, and kill.
func (in *invocation) lifecycle(command string, args []string) int {
	positional, flags := splitArgs(args)
	if len(flags) > 0 || len(positional) != 1 {
		fmt.Fprintf(in.stderr, "usage: sandboxctl %s SANDBOX\n", command)
		return 2
	}
	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	ctx := contextWithoutSignals()
	id := positional[0]

	var sb *sandbox.Sandbox
	switch command {
	case "pause":
		sb, err = client.PauseSandbox(ctx, id)
	case "resume":
		sb, err = client.ResumeSandbox(ctx, id)
	case "kill":
		sb, err = client.KillSandbox(ctx, id)
	}
	if err != nil {
		return in.fail(err)
	}
	in.printSandbox(sb)
	return 0
}

// writeEvent renders one stream event on the CLI's streams: command output on
// stdout, the sandbox's own stderr on stderr, and a note when output was cut.
func (in *invocation) writeEvent(ev sandbox.Event) error {
	switch ev.Type {
	case sandbox.EventStdout:
		_, err := io.WriteString(in.stdout, ev.Data)
		return err
	case sandbox.EventStderr:
		_, err := io.WriteString(in.stderr, ev.Data)
		return err
	case sandbox.EventTruncated:
		fmt.Fprintf(in.stderr, "sandboxctl: output truncated at %d bytes\n", ev.LimitBytes)
		return nil
	}
	return nil
}
