package dockerapi

// ContainerInspect is the part of GET /containers/{id}/json the backend reads.
// OOMKilled is set by the daemon when the container's init process was killed
// by the kernel OOM killer; it is the fallback when the cgroup file cannot be
// read, for example because the container is out of memory.
type ContainerInspect struct {
	ID    string         `json:"Id"`
	Name  string         `json:"Name"`
	State ContainerState `json:"State"`
}

// ContainerState is the inspected state of a container.
type ContainerState struct {
	Status    string `json:"Status"`
	Running   bool   `json:"Running"`
	Paused    bool   `json:"Paused"`
	OOMKilled bool   `json:"OOMKilled"`
	ExitCode  int    `json:"ExitCode"`
	Pid       int    `json:"Pid"`
}
