package dockerapi

import (
	"encoding/json"
	"testing"
)

// The Engine API ignores a field it does not recognize, so a mistyped JSON
// name is not an error: it is a limit that silently does nothing. The fake
// daemon decodes into the same struct, which would round-trip any spelling.
// These assertions pin the wire names to what Docker documents.
func TestHostConfigJSONFieldNames(t *testing.T) {
	pids := int64(1024)
	initProcess := true
	data, err := json.Marshal(HostConfig{
		Memory:      512 << 20,
		MemorySwap:  512 << 20,
		PidsLimit:   &pids,
		CapDrop:     []string{"NET_RAW"},
		SecurityOpt: []string{"no-new-privileges"},
		Init:        &initProcess,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"Memory", "MemorySwap", "PidsLimit", "CapDrop", "SecurityOpt", "Init"} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is missing from %s", key, data)
		}
	}
	// Docker decodes numbers into int64; a float would be a JSON type error on
	// strict daemons and is rejected here so the shape is checked, not assumed.
	if _, ok := got["PidsLimit"].(float64); !ok {
		t.Errorf("PidsLimit = %T, want a JSON number", got["PidsLimit"])
	}
}

// An unset PidsLimit must not become zero. Docker reads zero as "unlimited",
// so an omitted limit and an explicit zero would mean opposite things.
func TestHostConfigOmitsUnsetLimits(t *testing.T) {
	data, err := json.Marshal(HostConfig{Memory: 64 << 20})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["PidsLimit"]; ok {
		t.Errorf("unset PidsLimit is serialized: %s", data)
	}
}
