package dockerapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame header layout: one stream-type byte, three zero bytes, then a
// big-endian uint32 length. Types 0, 1 and 2 are stdin, stdout and stderr.
const (
	frameHeader = 8
	streamStdin = 0
	streamOut   = 1
	streamErr   = 2
)

// Demux copies Docker's multiplexed output stream to stdout and stderr until
// EOF. Why the format exists: one hijacked connection carries both streams,
// so every chunk arrives tagged instead of interleaved by luck of the write
// order. Frames for stdin carry nothing to show and are discarded; unknown
// types are skipped rather than failing, so a daemon that adds one later does
// not break output.
//
// A short header or a frame cut off by EOF is an error: the caller must be
// able to tell "the process ended" from "the runtime failed mid-stream".
func Demux(r io.Reader, stdout, stderr io.Writer) error {
	var hdr [frameHeader]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("dockerapi: read frame header: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		var w io.Writer
		switch hdr[0] {
		case streamOut:
			w = stdout
		case streamErr:
			w = stderr
		case streamStdin:
			// Nothing to write; the bytes still have to be consumed.
		default:
			// Unknown stream type: consume and ignore.
		}
		if w == nil {
			if _, err := io.CopyN(io.Discard, r, size); err != nil {
				return fmt.Errorf("dockerapi: discard %d-byte frame: %w", size, err)
			}
			continue
		}
		// CopyN, not LimitReader plus Copy: a frame cut short by EOF must
		// fail, because a short copy would silently drop the tail of the
		// output and report a clean end.
		if _, err := io.CopyN(w, r, size); err != nil {
			return fmt.Errorf("dockerapi: copy %d-byte frame: %w", size, err)
		}
	}
}
