package dockerapi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzDemux feeds arbitrary bytes through the framing parser. Docker's stream
// is the byte-level entry point for every execution's output, so a panic, a
// hang, or a silently short copy here corrupts everything built on top of it.
//
// The properties checked, in order of what they protect:
//   - no panic or hang, and no allocation proportional to a declared frame
//     size: a header may claim 4 GiB, and the implementation must stream;
//   - a parse wrote exactly the stdout and stderr the frames declared, and
//     nothing more, so a truncated frame never looks like a clean end;
//   - a truncated stream is reported as an error, because the destination
//     alone cannot distinguish a short read from a complete one.
func FuzzDemux(f *testing.F) {
	f.Add(multiFrame(byte(streamOut), "hello ", byte(streamErr), "warn\n", byte(streamOut), "world\n"))
	f.Add(multiFrame(byte(streamStdin), "typed at a prompt"))
	f.Add(multiFrame(byte(7), "future stream"))
	f.Add(frame(streamOut, "0123456789")[:12])        // truncated payload
	f.Add([]byte{1, 0, 0})                            // truncated header
	f.Add([]byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}) // huge length, no payload
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0})             // zero-length frame
	f.Add([]byte{})                                   // empty stream

	f.Fuzz(func(t *testing.T, data []byte) {
		wantOut, wantErr, wantIncomplete := expectedDemux(data)

		var out, errBuf bytes.Buffer
		err := Demux(bytes.NewReader(data), &out, &errBuf)

		if got := out.String(); got != wantOut {
			t.Fatalf("stdout = %q, want %q", got, wantOut)
		}
		if got := errBuf.String(); got != wantErr {
			t.Fatalf("stderr = %q, want %q", got, wantErr)
		}
		if gotIncomplete := err != nil; gotIncomplete != wantIncomplete {
			t.Fatalf("Demux error = %v, want incomplete = %v for %q", err, wantIncomplete, data)
		}
	})
}

// expectedDemux parses the same bytes with an independent, deliberately naive
// implementation. It is written separately from Demux on purpose: sharing the
// parser would only reproduce its bugs.
// The third result reports whether the stream ends mid-frame, which Demux must
// report as an error.
func expectedDemux(data []byte) (stdout, stderr string, incomplete bool) {
	var out, errBuf bytes.Buffer
	for len(data) > 0 {
		if len(data) < frameHeader {
			return out.String(), errBuf.String(), true
		}
		stream := data[0]
		size := uint64(binary.BigEndian.Uint32(data[4:8]))
		body := data[frameHeader:]
		// A frame cut short by EOF is a parse failure. The bytes that were
		// present have already been handed to the destination by then, which
		// is why the partial chunk is appended before returning: the error,
		// not an empty destination, is what tells the caller the output is
		// incomplete.
		if uint64(len(body)) < size {
			appendChunk(stream, body, &out, &errBuf)
			return out.String(), errBuf.String(), true
		}
		appendChunk(stream, body[:size], &out, &errBuf)
		data = body[size:]
	}
	return out.String(), errBuf.String(), false
}

// multiFrame concatenates frames, so a seed with several frames stays
// readable.
func multiFrame(parts ...any) []byte {
	var b bytes.Buffer
	for i := 0; i+1 < len(parts); i += 2 {
		stream, ok := parts[i].(byte)
		if !ok {
			panic("multiFrame: stream type must be a byte")
		}
		payload, ok := parts[i+1].(string)
		if !ok {
			panic("multiFrame: payload must be a string")
		}
		b.Write(frame(stream, payload))
	}
	return b.Bytes()
}

// appendChunk routes a frame body the way Demux does: stdout and stderr are
// kept, stdin and unknown types are discarded.
func appendChunk(stream byte, chunk []byte, out, errBuf *bytes.Buffer) {
	switch stream {
	case streamOut:
		out.Write(chunk)
	case streamErr:
		errBuf.Write(chunk)
	}
}
