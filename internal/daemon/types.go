package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Defaults and limits fixed by docs/api.md.
const (
	DefaultImage          = "alpine:3.20"
	DefaultMemoryMB       = 512
	MinMemoryMB           = 16
	DefaultTimeoutSeconds = 300

	// MaxOutputBytes is the retained stdout+stderr budget per execution.
	MaxOutputBytes = 1 << 20
	// MaxOutputEvents bounds the number of stdout and stderr events.
	MaxOutputEvents = 4096
	// MaxEventDataBytes is the largest chunk carried by one output event.
	MaxEventDataBytes = 32 << 10
	// MaxIdempotencyKey is the longest accepted idempotency key, in characters.
	MaxIdempotencyKey = 128
	// MaxFileBytes is the largest file the daemon accepts or serves.
	MaxFileBytes = 10 << 20
	// MaxRequestBodyBytes is the largest JSON request body.
	MaxRequestBodyBytes = 1 << 20
	// MaxExecutionsPerSandbox bounds the execution history. Above it, the
	// oldest completed execution and its retained output are dropped; the
	// running execution is never dropped.
	MaxExecutionsPerSandbox = 200
	// BackendCallTimeout bounds one container-runtime call. The reaper shares
	// one goroutine, so a call that never returns would stop timeout and
	// eviction checks for every sandbox.
	BackendCallTimeout = 5 * time.Second
)

// State is a sandbox lifecycle state: pending, running, paused, or killed.
type State string

// States, as api.md defines them.
const (
	StatePending State = "pending"
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateKilled  State = "killed"
)

// KillReason explains why a sandbox reached StateKilled. It is terminal.
type KillReason string

// Kill reasons, as api.md defines them.
const (
	KillRequested   KillReason = "requested"
	KillTimeout     KillReason = "timeout"
	KillIdleTimeout KillReason = "idle_timeout"
	KillEvicted     KillReason = "evicted"
)

// Action is what a timeout does when it fires. Only kill and pause exist.
type Action string

// Actions a timeout can take.
const (
	ActionKill  Action = "kill"
	ActionPause Action = "pause"
)

// Reason is an execution termination reason. If several apply, the first in
// this order wins: canceled, timeout, oom, evicted, exited. Exit code 137
// alone never implies oom.
type Reason string

// Termination reasons, in the precedence order of api.md.
const (
	ReasonCanceled Reason = "canceled"
	ReasonTimeout  Reason = "timeout"
	ReasonOOM      Reason = "oom"
	ReasonEvicted  Reason = "evicted"
	ReasonExited   Reason = "exited"
)

// ExecState is an execution state: running or completed.
type ExecState string

// Execution states.
const (
	ExecRunning   ExecState = "running"
	ExecCompleted ExecState = "completed"
)

// Sandbox is the contract view of a sandbox.
type Sandbox struct {
	ID                 string      `json:"id"`
	State              State       `json:"state"`
	Image              string      `json:"image"`
	MemoryMB           int         `json:"memory_mb"`
	TimeoutSeconds     int         `json:"timeout_seconds"`
	TimeoutAction      Action      `json:"timeout_action"`
	IdleTimeoutSeconds int         `json:"idle_timeout_seconds"`
	IdleTimeoutAction  Action      `json:"idle_timeout_action"`
	CreatedAt          time.Time   `json:"created_at"`
	ExpiresAt          *time.Time  `json:"expires_at"`
	KillReason         *KillReason `json:"kill_reason"`
}

// Execution is the contract view of an execution, including retained output.
type Execution struct {
	ID                string     `json:"id"`
	SandboxID         string     `json:"sandbox_id"`
	Command           []string   `json:"command"`
	State             ExecState  `json:"state"`
	ExitCode          *int       `json:"exit_code"`
	TerminationReason *Reason    `json:"termination_reason"`
	Stdout            string     `json:"stdout"`
	Stderr            string     `json:"stderr"`
	OutputTruncated   bool       `json:"output_truncated"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
}

// ExecutionSummary is an Execution without stdout and stderr, as returned by
// list endpoints.
type ExecutionSummary struct {
	ID                string     `json:"id"`
	SandboxID         string     `json:"sandbox_id"`
	Command           []string   `json:"command"`
	State             ExecState  `json:"state"`
	ExitCode          *int       `json:"exit_code"`
	TerminationReason *Reason    `json:"termination_reason"`
	OutputTruncated   bool       `json:"output_truncated"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
}

// Summary drops the retained output.
func (e Execution) Summary() ExecutionSummary {
	return ExecutionSummary{
		ID:                e.ID,
		SandboxID:         e.SandboxID,
		Command:           e.Command,
		State:             e.State,
		ExitCode:          e.ExitCode,
		TerminationReason: e.TerminationReason,
		OutputTruncated:   e.OutputTruncated,
		StartedAt:         e.StartedAt,
		FinishedAt:        e.FinishedAt,
	}
}

// CreateRequest carries the optional fields of POST /v1/sandboxes. A nil
// pointer means "use the default"; a pointer to zero is an explicit zero.
type CreateRequest struct {
	Image              *string `json:"image,omitempty"`
	MemoryMB           *int    `json:"memory_mb,omitempty"`
	TimeoutSeconds     *int    `json:"timeout_seconds,omitempty"`
	TimeoutAction      *Action `json:"timeout_action,omitempty"`
	IdleTimeoutSeconds *int    `json:"idle_timeout_seconds,omitempty"`
	IdleTimeoutAction  *Action `json:"idle_timeout_action,omitempty"`
}

// ExecuteRequest is the body of POST /v1/sandboxes/{id}/executions.
type ExecuteRequest struct {
	Command        []string `json:"command"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
}

// sandboxOptions is a CreateRequest resolved against the defaults.
type sandboxOptions struct {
	image              string
	memoryMB           int
	timeoutSeconds     int
	timeoutAction      Action
	idleTimeoutSeconds int
	idleTimeoutAction  Action
}

// resolve applies defaults and validates the request.
func (r CreateRequest) resolve() (sandboxOptions, error) {
	opts := sandboxOptions{
		image:              DefaultImage,
		memoryMB:           DefaultMemoryMB,
		timeoutSeconds:     DefaultTimeoutSeconds,
		timeoutAction:      ActionKill,
		idleTimeoutSeconds: 0,
		idleTimeoutAction:  ActionKill,
	}
	if r.Image != nil {
		if *r.Image == "" {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "image must not be empty")
		}
		opts.image = *r.Image
	}
	if r.MemoryMB != nil {
		if *r.MemoryMB < MinMemoryMB {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "memory_mb must be at least %d", MinMemoryMB)
		}
		opts.memoryMB = *r.MemoryMB
	}
	if r.TimeoutSeconds != nil {
		if *r.TimeoutSeconds < 0 {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "timeout_seconds must not be negative")
		}
		opts.timeoutSeconds = *r.TimeoutSeconds
	}
	if r.TimeoutAction != nil {
		if err := validateAction("timeout_action", *r.TimeoutAction); err != nil {
			return sandboxOptions{}, err
		}
		opts.timeoutAction = *r.TimeoutAction
	}
	if r.IdleTimeoutSeconds != nil {
		if *r.IdleTimeoutSeconds < 0 {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "idle_timeout_seconds must not be negative")
		}
		opts.idleTimeoutSeconds = *r.IdleTimeoutSeconds
	}
	if r.IdleTimeoutAction != nil {
		if err := validateAction("idle_timeout_action", *r.IdleTimeoutAction); err != nil {
			return sandboxOptions{}, err
		}
		opts.idleTimeoutAction = *r.IdleTimeoutAction
	}
	return opts, nil
}

func validateAction(field string, action Action) error {
	switch action {
	case ActionKill, ActionPause:
		return nil
	default:
		return errorf(CodeInvalidRequest, "%s must be %q or %q", field, ActionKill, ActionPause)
	}
}

// newID returns an opaque identifier with the given prefix, such as "sbx_".
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on supported platforms; if it does, the
		// process has bigger problems than a duplicate ID.
		panic("daemon: generate id: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneReason(r *KillReason) *KillReason {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}
