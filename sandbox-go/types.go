package sandbox

// Types mirroring the HTTP contract in docs/api.md. Optional response fields
// use pointers so "unset" and "zero" stay distinct; request fields that are
// optional use pointers for the same reason.

// State is a sandbox lifecycle state.
type State string

// States, as api.md defines them.
const (
	StatePending State = "pending"
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateKilled  State = "killed"
)

// KillReason explains why a sandbox was killed.
type KillReason string

// Kill reasons, as api.md defines them.
const (
	KillRequested   KillReason = "requested"
	KillTimeout     KillReason = "timeout"
	KillIdleTimeout KillReason = "idle_timeout"
	KillEvicted     KillReason = "evicted"
)

// Action is what a timeout does when it fires.
type Action string

// Actions a timeout can take.
const (
	ActionKill  Action = "kill"
	ActionPause Action = "pause"
)

// Reason is an execution termination reason.
type Reason string

// Termination reasons, as api.md defines them.
const (
	ReasonCanceled Reason = "canceled"
	ReasonTimeout  Reason = "timeout"
	ReasonOOM      Reason = "oom"
	ReasonEvicted  Reason = "evicted"
	ReasonExited   Reason = "exited"
)

// Sandbox is the contract view of a sandbox.
type Sandbox struct {
	ID                 string      `json:"id"`
	State              State       `json:"state"`
	Image              string      `json:"image"`
	MemoryMB           int         `json:"memory_mb"`
	PidsLimit          int         `json:"pids_limit"`
	NanoCPUs           int64       `json:"nano_cpus"`
	Network            string      `json:"network"`
	TimeoutSeconds     int         `json:"timeout_seconds"`
	TimeoutAction      Action      `json:"timeout_action"`
	IdleTimeoutSeconds int         `json:"idle_timeout_seconds"`
	IdleTimeoutAction  Action      `json:"idle_timeout_action"`
	CreatedAt          Time        `json:"created_at"`
	ExpiresAt          *Time       `json:"expires_at"`
	KillReason         *KillReason `json:"kill_reason"`
}

// Execution is one command run in a sandbox.
type Execution struct {
	ID                string   `json:"id"`
	SandboxID         string   `json:"sandbox_id"`
	Command           []string `json:"command"`
	State             string   `json:"state"`
	ExitCode          *int     `json:"exit_code"`
	TerminationReason *Reason  `json:"termination_reason"`
	Stdout            string   `json:"stdout"`
	Stderr            string   `json:"stderr"`
	OutputTruncated   bool     `json:"output_truncated"`
	StartedAt         Time     `json:"started_at"`
	FinishedAt        *Time    `json:"finished_at"`
}

// Network modes a sandbox can be created with.
const (
	// NetworkBridge gives the sandbox outbound network access through Docker's
	// bridge. This is the default: package installs and HTTP calls work.
	NetworkBridge = "bridge"
	// NetworkNone removes networking entirely.
	NetworkNone = "none"
)

// CreateSandboxRequest is the body of POST /v1/sandboxes. Every field is
// optional; a nil pointer takes the server default.
type CreateSandboxRequest struct {
	Image              *string `json:"image,omitempty"`
	MemoryMB           *int    `json:"memory_mb,omitempty"`
	PidsLimit          *int    `json:"pids_limit,omitempty"`
	NanoCPUs           *int64  `json:"nano_cpus,omitempty"`
	Network            *string `json:"network,omitempty"`
	TimeoutSeconds     *int    `json:"timeout_seconds,omitempty"`
	TimeoutAction      *Action `json:"timeout_action,omitempty"`
	IdleTimeoutSeconds *int    `json:"idle_timeout_seconds,omitempty"`
	IdleTimeoutAction  *Action `json:"idle_timeout_action,omitempty"`
}

// ExecuteRequest is the body of POST .../executions.
//
// TimeoutSeconds bounds one execution: nil or 0 means no deadline of its own.
// The sandbox lifetime policy still applies: it can kill or pause the sandbox.
type ExecuteRequest struct {
	Command        []string `json:"command"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
	TimeoutSeconds *int     `json:"timeout_seconds,omitempty"`
}

// EventType is an SSE event type.
type EventType string

// Event types carried by an execution stream.
const (
	EventStdout    EventType = "stdout"
	EventStderr    EventType = "stderr"
	EventTruncated EventType = "truncated"
	EventComplete  EventType = "complete"
)

// Event is one event from an execution stream. Exactly one of Data, LimitBytes
// and Execution is meaningful, depending on Type.
type Event struct {
	ID         uint64
	Type       EventType
	Data       string
	LimitBytes int
	Execution  *Execution
}

// sandboxList and executionList are the list envelopes.
type sandboxList struct {
	Sandboxes []*Sandbox `json:"sandboxes"`
}

type executionList struct {
	Executions []*Execution `json:"executions"`
}
