package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

// doctor implements `sandboxctl doctor`: one line per check on stdout, then
// Ready or the hints for whatever failed.
func (in *invocation) doctor(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(in.stderr, "usage: sandboxctl doctor")
		return 2
	}

	client, clientErr := in.client()
	domain := in.effectiveDomain()

	var failed []error
	report := func(name string, err error, detail string) {
		if err == nil {
			if detail != "" {
				fmt.Fprintf(in.stdout, "%s: ok (%s)\n", name, detail)
			} else {
				fmt.Fprintf(in.stdout, "%s: ok\n", name)
			}
			return
		}
		fmt.Fprintf(in.stdout, "%s: fail (%s)\n", name, messageOf(err))
		failed = append(failed, err)
	}

	// 1. config
	if clientErr != nil {
		report("config", clientErr, "")
		checksFailed(in.stdout, in.stderr, failed)
		return 1
	}
	keyState := "no api key"
	if in.globals.apiKey != "" || os.Getenv("SANDBOX_API_KEY") != "" {
		keyState = "api key set"
	}
	report("config", nil, domain+", "+keyState)

	// 2. health: the one direct HTTP call the CLI makes.
	healthCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := healthProbe(healthCtx, domain)
	cancel()
	report("health", err, "")

	// 3. auth: a random sandbox that cannot exist answers 404 when authorized.
	authCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.GetSandbox(authCtx, "sbx_doctor_probe")
	cancel()
	switch {
	case err == nil:
		report("auth", fmt.Errorf("a nonexistent sandbox was returned"), "")
	case isNotFound(err):
		report("auth", nil, "")
	case isUnauthorized(err):
		report("auth", err, "")
	default:
		report("auth", err, "")
	}

	// 4. docker: only meaningful when the server is local.
	if isLocalDomain(domain) {
		report("docker", checkDocker(), "")
	} else {
		fmt.Fprintf(in.stdout, "docker: skipped (remote domain)\n")
	}

	// 5. sandbox: create, run, verify, kill.
	report("sandbox", in.checkSandbox(client), "")

	return checksFailed(in.stdout, in.stderr, failed)
}

// checksFailed prints Ready or the hints and returns the exit code.
func checksFailed(stdout, stderr io.Writer, failed []error) int {
	if len(failed) == 0 {
		fmt.Fprintln(stdout, "Ready")
		return 0
	}
	fmt.Fprintln(stdout, "not ready")
	seen := make(map[string]bool)
	for _, err := range failed {
		hint := hintFor(err)
		if hint == "" || seen[hint] {
			continue
		}
		seen[hint] = true
		fmt.Fprintf(stderr, "hint: %s\n", hint)
	}
	return 1
}

// effectiveDomain is the URL the client will use.
func (in *invocation) effectiveDomain() string {
	if in.globals.domain != "" {
		return in.globals.domain
	}
	if env := os.Getenv("SANDBOX_DOMAIN"); env != "" {
		return env
	}
	return "http://localhost:8080"
}

// checkDocker asks the Docker CLI whether a daemon is reachable. Docker is a
// host dependency of a local server, not of the CLI, so this is a check rather
// than a requirement.
func checkDocker() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker info failed")
	}
	return nil
}

// checkSandbox creates a sandbox, runs echo, verifies the output, and kills it.
func (in *invocation) checkSandbox(client *sandbox.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{})
	if err != nil {
		return err
	}
	defer func() {
		killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer killCancel()
		_, _ = client.KillSandbox(killCtx, sb.ID)
	}()

	ex, err := client.Exec(ctx, sb.ID, []string{"echo", "doctor-ok"})
	if err != nil {
		return err
	}
	if strings.TrimSpace(ex.Stdout) != "doctor-ok" {
		return fmt.Errorf("echo returned %q", strings.TrimSpace(ex.Stdout))
	}
	return nil
}

// isNotFound and isUnauthorized keep the doctor switch readable.
func isNotFound(err error) bool     { return errors.Is(err, sandbox.ErrNotFound) }
func isUnauthorized(err error) bool { return errors.Is(err, sandbox.ErrUnauthorized) }

// contextWithoutSignals is the context for commands that finish quickly.
// Ctrl+C still terminates the process; these calls are short enough that a
// graceful cancel adds nothing.
func contextWithoutSignals() context.Context { return context.Background() }
