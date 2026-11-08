package dockerapi

import (
	"strings"
	"testing"
)

func TestParseOOMKillCgroupV2(t *testing.T) {
	data := []byte("low 0\nhigh 0\nmax 0\noom 4\noom_kill 2\noom_group_kill 1\n")
	n, err := ParseOOMKill(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n != 2 {
		t.Errorf("oom_kill = %d, want 2", n)
	}
}

func TestParseOOMKillCgroupV1(t *testing.T) {
	data := []byte("oom_kill_disable 0\nunder_oom 0\noom_kill 7\n")
	n, err := ParseOOMKill(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n != 7 {
		t.Errorf("oom_kill = %d, want 7", n)
	}
}

// The counter names share a prefix: "oom", "oom_kill" and "oom_group_kill" are
// three different numbers. Only the exact key counts.
func TestParseOOMKillRequiresExactKey(t *testing.T) {
	cases := map[string]string{
		"oom counter only":        "oom 3\n",
		"group counter only":      "oom_group_kill 3\n",
		"prefix lookalike only":   "oom_kills 3\n",
		"prefixed key":            "xoom_kill 3\n",
		"less than two fields":    "oom_kill\n",
		"counter is not a number": "oom_kill lots\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOOMKill([]byte(data)); err == nil {
				t.Fatalf("parse(%q) succeeded, want an error", data)
			}
		})
	}
}

func TestParseOOMKillEmpty(t *testing.T) {
	_, err := ParseOOMKill(nil)
	if err == nil || !strings.Contains(err.Error(), "no oom_kill counter") {
		t.Fatalf("err = %v", err)
	}
}
