package dockerapi

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

func TestFileTarRoundTrip(t *testing.T) {
	data := []byte("line one\nline two\n")
	body, err := BuildFileTar("home/user/app/main.go", data)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, err := ExtractFile(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("extracted %q, want %q", got, data)
	}
}

func TestBuildFileTarRejectsNonRelativeNames(t *testing.T) {
	for _, name := range []string{"/abs/path", ".", "/", "..", "a/../../b"} {
		if _, err := BuildFileTar(name, []byte("x")); err == nil {
			t.Errorf("BuildFileTar(%q) succeeded, want an error", name)
		}
	}
}

func TestBuildFileTarCleansName(t *testing.T) {
	body, err := BuildFileTar("a/./b//c.txt", []byte("x"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// ExtractFile does not expose the name; re-read the header instead.
	tr := tar.NewReader(bytes.NewReader(body))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if hdr.Name != "a/b/c.txt" {
		t.Errorf("entry name = %q, want a/b/c.txt", hdr.Name)
	}
}

func TestExtractFileSkipsDirectories(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: '5', Mode: 0o755}); err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "dir/file.txt", Typeflag: '0', Mode: 0o644, Size: 2}); err != nil {
		t.Fatalf("header: %v", err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := ExtractFile(&buf)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("extracted %q, want hi", got)
	}
}

func TestExtractFileEmptyArchive(t *testing.T) {
	var buf bytes.Buffer
	if err := tar.NewWriter(&buf).Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := ExtractFile(&buf); err == nil || !strings.Contains(err.Error(), "no file") {
		t.Fatalf("err = %v", err)
	}
}
