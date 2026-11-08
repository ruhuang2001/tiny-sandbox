package dockerapi

import (
	"fmt"
	"strconv"
	"strings"
)

// OOMScript prints the container's cgroup OOM-kill counter. On cgroup v2 the
// counter is a field of memory.events; on v1 it is a field of
// memory.oom_control. Both are read as text because the daemon queries them by
// exec'ing inside the container, where the cgroup mount layout is the
// container's own, not the host's.
const OOMScript = `if [ -r /sys/fs/cgroup/memory.events ]; then cat /sys/fs/cgroup/memory.events;
elif [ -r /sys/fs/cgroup/memory/memory.oom_control ]; then cat /sys/fs/cgroup/memory/memory.oom_control;
else exit 3; fi`

// ParseOOMKill extracts the cumulative oom_kill counter from a cgroup file.
//
// cgroup v2 memory.events looks like:
//
//	low 0
//	high 0
//	max 0
//	oom 0
//	oom_kill 2
//
// cgroup v1 memory.oom_control looks like:
//
//	oom_kill_disable 0
//	under_oom 0
//	oom_kill 2
//
// The key must match exactly: "oom" and "oom_group_kill" are different
// counters, and matching them by prefix would report a kill that never
// happened.
func ParseOOMKill(data []byte) (int64, error) {
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "oom_kill" {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("dockerapi: parse oom_kill %q: %w", fields[1], err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("dockerapi: no oom_kill counter in %d bytes", len(data))
}
