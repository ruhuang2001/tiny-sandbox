package dockerapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// frame builds one multiplexed frame: type byte, three padding bytes, a
// big-endian length, then the payload.
func frame(stream byte, payload string) []byte {
	var b bytes.Buffer
	b.WriteByte(stream)
	b.Write([]byte{0, 0, 0})
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
	b.Write(size[:])
	b.WriteString(payload)
	return b.Bytes()
}

func TestDemuxSplitsStreams(t *testing.T) {
	input := append(frame(streamOut, "hello "), frame(streamErr, "warn\n")...)
	input = append(input, frame(streamOut, "world\n")...)

	var out, errBuf bytes.Buffer
	if err := Demux(bytes.NewReader(input), &out, &errBuf); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if got := out.String(); got != "hello world\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := errBuf.String(); got != "warn\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestDemuxSkipsStdinAndUnknownTypes(t *testing.T) {
	input := frame(streamStdin, "typed at a prompt")
	input = append(input, frame(7, "future stream")...)
	input = append(input, frame(streamOut, "kept")...)

	var out, errBuf bytes.Buffer
	if err := Demux(bytes.NewReader(input), &out, &errBuf); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if out.String() != "kept" || errBuf.Len() != 0 {
		t.Errorf("stdout=%q stderr=%q", out.String(), errBuf.String())
	}
}

func TestDemuxEmptyStreamIsClean(t *testing.T) {
	var out, errBuf bytes.Buffer
	if err := Demux(strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatalf("demux: %v", err)
	}
}

func TestDemuxTruncatedHeaderFails(t *testing.T) {
	var out, errBuf bytes.Buffer
	err := Demux(bytes.NewReader([]byte{1, 0, 0}), &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "frame header") {
		t.Fatalf("err = %v, want a frame-header error", err)
	}
}

func TestDemuxTruncatedPayloadFails(t *testing.T) {
	full := frame(streamOut, "0123456789")
	cut := full[:len(full)-4]
	var out, errBuf bytes.Buffer
	err := Demux(bytes.NewReader(cut), &out, &errBuf)
	if err == nil {
		// A frame cut exactly at the payload boundary would look complete;
		// this cut is mid-payload, so the read must fail.
		t.Fatal("err = nil, want a truncated-frame error")
	}
}

func TestDemuxLargeFrame(t *testing.T) {
	payload := strings.Repeat("x", 64<<10)
	var out, errBuf bytes.Buffer
	if err := Demux(bytes.NewReader(frame(streamOut, payload)), &out, &errBuf); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if out.Len() != len(payload) {
		t.Errorf("stdout = %d bytes, want %d", out.Len(), len(payload))
	}
}

// failingWriter makes the destination fail, so Demux must report it rather
// than dropping output.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestDemuxReportsWriteErrors(t *testing.T) {
	var sink bytes.Buffer
	err := Demux(bytes.NewReader(frame(streamOut, "payload")), failingWriter{}, &sink)
	if err == nil || !strings.Contains(err.Error(), "copy 7-byte frame") {
		t.Fatalf("err = %v, want a copy error", err)
	}
}

var _ io.Writer = failingWriter{}
