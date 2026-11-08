package sandbox

import (
	"encoding/json"
	"fmt"
	"time"
)

// Time is an RFC 3339 timestamp as the API sends it. The server always emits
// UTC; Time keeps the parse error phraseable.
type Time struct {
	time.Time
}

// UnmarshalJSON parses an RFC 3339 string. A JSON null leaves the zero time.
func (t *Time) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("sandbox: timestamp is not a string: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("sandbox: parse timestamp %q: %w", s, err)
	}
	t.Time = parsed
	return nil
}

// MarshalJSON writes an RFC 3339 UTC string.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format(time.RFC3339Nano))
}
