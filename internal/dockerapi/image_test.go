package dockerapi

import (
	"strings"
	"testing"
)

func TestSplitImage(t *testing.T) {
	cases := map[string]struct{ repository, tag string }{
		"alpine:3.20":               {"alpine", "3.20"},
		"alpine":                    {"alpine", "latest"},
		"library/alpine:3.20":       {"library/alpine", "3.20"},
		"ghcr.io/org/app:1.2.3":     {"ghcr.io/org/app", "1.2.3"},
		"registry:5000/team/app":    {"registry:5000/team/app", "latest"},
		"registry:5000/team/app:v1": {"registry:5000/team/app", "v1"},
	}
	for image, want := range cases {
		repository, tag := splitImage(image)
		if repository != want.repository || tag != want.tag {
			t.Errorf("splitImage(%q) = %q, %q; want %q, %q", image, repository, tag, want.repository, want.tag)
		}
	}
}

func TestParsePullStreamSucceeds(t *testing.T) {
	stream := `{"status":"Pulling from library/alpine"}
{"status":"Downloading","progressDetail":{"current":1024,"total":4096}}
{"status":"Status: Downloaded newer image for alpine:3.20"}
`
	if err := parsePullStream(strings.NewReader(stream)); err != nil {
		t.Fatalf("parse: %v", err)
	}
}

func TestParsePullStreamReportsError(t *testing.T) {
	stream := `{"status":"Pulling"}
{"error":"pull access denied for private/nope"}
`
	err := parsePullStream(strings.NewReader(stream))
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("err = %v, want the pull failure", err)
	}
}

func TestParsePullStreamIgnoresNonJSON(t *testing.T) {
	// A daemon that writes plain progress text must not be read as an error.
	if err := parsePullStream(strings.NewReader("Downloading layer abc123\n")); err != nil {
		t.Fatalf("parse: %v", err)
	}
}
