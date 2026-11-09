package dockerapi

// ContainerConfig is the body of POST /containers/create. Only the fields the
// sandbox backend sets are modelled; the Engine API ignores the rest.
type ContainerConfig struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// HostConfig carries the resource limits and the security floor every sandbox
// container is created with. MemorySwap is set equal to Memory so a sandbox
// cannot use swap to exceed its declared memory limit.
//
// PidsLimit is a pointer because Docker distinguishes "unset" from zero: zero
// means unlimited, and an unset field would let the daemon's own default
// apply. The sandbox always sets it.
type HostConfig struct {
	Memory      int64             `json:"Memory,omitempty"`
	MemorySwap  int64             `json:"MemorySwap,omitempty"`
	PidsLimit   *int64            `json:"PidsLimit,omitempty"`
	CapDrop     []string          `json:"CapDrop,omitempty"`
	SecurityOpt []string          `json:"SecurityOpt,omitempty"`
	Tmpfs       map[string]string `json:"Tmpfs,omitempty"`
	// Init runs Docker's init process as PID 1 instead of the container's
	// command. The sandbox needs it because PID 1 is what reaps orphaned
	// grandchildren, and a pids limit makes unreaped zombies permanent lost
	// task slots.
	Init *bool `json:"Init,omitempty"`
}

// Container is one entry of GET /containers/json.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// ExecInspect is the body of GET /exec/{id}/json. ExitCode is null until the
// process exits.
type ExecInspect struct {
	ID          string `json:"ID"`
	Running     bool   `json:"Running"`
	ExitCode    *int   `json:"ExitCode"`
	Pid         int    `json:"Pid"`
	ContainerID string `json:"ContainerID"`
}

type execConfig struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
}

type execStart struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}
