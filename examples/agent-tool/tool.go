// Package agenttool exposes a tiny-sandbox execution as an agent tool.
//
// An agent decides to run code by calling a tool with a JSON object; the host
// executes it and returns a result the model can read. This package is that
// boundary, written against the SDK rather than the HTTP API, so it inherits
// the SDK's cancellation recovery, stream de-duplication, and error
// classification.
//
// The shape of the tool result matters as much as the code it runs. An agent
// cannot look at a stack trace the way a developer can: a nonzero exit, an OOM
// kill, and a timeout must arrive as distinct, machine-readable outcomes with
// the output attached, or the model has no way to decide what to do next.
package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

// ToolName is the name the model calls.
const ToolName = "run_python"

// Definition is the JSON schema an agent runtime advertises to the model. It
// follows the shape tool-use APIs expect: name, description, input schema.
type Definition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolDefinition returns the tool's declaration as an agent runtime
// advertises it to a model.
func ToolDefinition() Definition {
	return Definition{
		Name: ToolName,
		Description: "Run a Python 3 program in an isolated sandbox and return its output. " +
			"The sandbox has no network access assumptions and a memory limit; " +
			"a program that exceeds it is reported as oom, not as a normal failure.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "The Python source to run. Printed output is returned to you.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Lifetime of the sandbox in seconds. Default 60.",
					"minimum":     1,
				},
			},
			"required": []string{"code"},
		},
	}
}

// Input is the tool's argument object.
type Input struct {
	Code           string `json:"code"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// Result is what the agent receives. It is written for a model, not for a
// developer: every terminal outcome is representable, and output is never
// dropped.
type Result struct {
	// IsError tells the agent runtime to mark this result as a failure.
	IsError bool `json:"is_error"`
	// Outcome is one of: exited, nonzero_exit, oom, timeout, canceled,
	// evicted, invalid_input, unavailable.
	Outcome string `json:"outcome"`
	// ExitCode is present whenever the runtime reported one.
	ExitCode *int `json:"exit_code,omitempty"`
	// Stdout and Stderr are the retained output, truncated per the contract.
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	// OutputTruncated warns the model that the output above is incomplete.
	OutputTruncated bool `json:"output_truncated,omitempty"`
	// Message is the human-readable summary the model reads first.
	Message string `json:"message,omitempty"`
	// Hint is a suggested next step, when one is known.
	Hint string `json:"hint,omitempty"`
}

// Tool runs Python code in a sandbox created for the call.
type Tool struct {
	client *sandbox.Client
	image  string
	memory int
}

// New returns a tool that creates one sandbox per call. A long-lived agent may
// prefer to keep a sandbox and call Run on it; see NewWithSandbox.
func New(client *sandbox.Client) *Tool {
	return &Tool{client: client, image: "python:3.12-alpine", memory: 256}
}

// Run executes the tool's input in a fresh sandbox and removes the sandbox
// afterwards, whatever the outcome. The sandbox is the tool's resource, so it
// is the tool's job to release it.
func (t *Tool) Run(ctx context.Context, input Input) Result {
	if input.Code == "" {
		return Result{IsError: true, Outcome: "invalid_input", Message: "code is required"}
	}
	timeout := input.TimeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}

	sb, err := t.client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{
		Image:          &t.image,
		MemoryMB:       &t.memory,
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		return failure(err)
	}
	defer func() {
		// The caller's context may already be done, which is exactly when a
		// cleanup request needs its own.
		killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = t.client.KillSandbox(killCtx, sb.ID)
	}()

	return t.run(ctx, sb.ID, input.Code)
}

// RunIn executes the input in an existing sandbox, for an agent that wants to
// keep state between calls (imports, files, a REPL-style session).
func (t *Tool) RunIn(ctx context.Context, sandboxID string, input Input) Result {
	if input.Code == "" {
		return Result{IsError: true, Outcome: "invalid_input", Message: "code is required"}
	}
	return t.run(ctx, sandboxID, input.Code)
}

// run starts python3 -c and streams nothing to the caller: an agent reads the
// final result, and streaming would need a channel the model cannot consume.
func (t *Tool) run(ctx context.Context, sandboxID, code string) Result {
	ex, err := t.client.Exec(ctx, sandboxID, []string{"python3", "-c", code})
	if err != nil {
		result := failure(err)
		// An execution outcome still carries the output the model needs.
		if ex != nil {
			fillOutput(&result, ex)
		}
		return result
	}
	result := Result{Outcome: "exited"}
	fillOutput(&result, ex)
	return result
}

// fillOutput copies the execution's view into the tool result.
func fillOutput(result *Result, ex *sandbox.Execution) {
	result.Stdout = ex.Stdout
	result.Stderr = ex.Stderr
	result.ExitCode = ex.ExitCode
	result.OutputTruncated = ex.OutputTruncated
}

// failure maps an SDK error to a tool result. The mapping is the whole point
// of this package: an agent branches on Outcome, never on a message string.
func failure(err error) Result {
	var ex *sandbox.Execution
	var sdkErr *sandbox.Error
	if errors.As(err, &sdkErr) {
		ex = sdkErr.Execution
	}
	switch {
	case errors.Is(err, sandbox.ErrOOM):
		return Result{
			IsError: true, Outcome: "oom",
			Message: "the program was killed by the kernel for exceeding the memory limit",
			Hint:    "reduce memory use or process the data in chunks",
		}
	case errors.Is(err, sandbox.ErrTimeout):
		return Result{
			IsError: true, Outcome: "timeout",
			Message: "the sandbox lifetime expired before the program finished",
			Hint:    "raise timeout_seconds or make the program faster",
		}
	case errors.Is(err, sandbox.ErrCanceled):
		return Result{
			IsError: true, Outcome: "canceled",
			Message: "the run was canceled before it finished",
		}
	case errors.Is(err, sandbox.ErrEvicted):
		return Result{
			IsError: true, Outcome: "evicted",
			Message: "the sandbox container was stopped outside the server",
			Hint:    "retry the call; the sandbox did not fail on its own",
		}
	case errors.Is(err, sandbox.ErrNonZeroExit):
		result := Result{
			IsError: true, Outcome: "nonzero_exit",
			Message: "the program exited with a nonzero status",
		}
		if ex != nil {
			fillOutput(&result, ex)
		}
		return result
	case errors.Is(err, sandbox.ErrBusy):
		return Result{
			IsError: true, Outcome: "unavailable",
			Message: "the sandbox was already running another program",
			Hint:    "wait, or create a separate sandbox",
		}
	default:
		return Result{
			IsError: true, Outcome: "unavailable",
			Message: "the sandbox service could not run the program",
			Hint:    "retry once; if it persists, the service is unavailable",
		}
	}
}

// ResultJSON renders a result for a tool-result message.
func ResultJSON(result Result) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("agenttool: encode result: %w", err)
	}
	return string(data), nil
}
