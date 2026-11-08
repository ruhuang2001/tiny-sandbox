package dockerapi

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// MaxArchiveBytes bounds one archived file, matching the daemon's file limit.
const MaxArchiveBytes = 10 << 20

// BuildFileTar returns a tar archive holding data as a single regular file.
// name is the path inside the archive and must be relative: the archive is
// extracted at "/" in the container, so a leading separator would be doubled.
func BuildFileTar(name string, data []byte) ([]byte, error) {
	name = path.Clean(name)
	if name == "." || name == "/" || path.IsAbs(name) ||
		name == ".." || strings.HasPrefix(name, "../") {
		return nil, fmt.Errorf("dockerapi: archive name %q must be a relative file path inside the container", name)
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		ModTime:  time.Unix(0, 0),
	}
	if err := w.WriteHeader(hdr); err != nil {
		return nil, fmt.Errorf("dockerapi: write tar header: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("dockerapi: write tar body: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("dockerapi: close tar: %w", err)
	}
	return buf.Bytes(), nil
}

// ExtractFile returns the contents of the regular file in a tar archive
// produced by the archive endpoint. A directory entry is skipped: the archive
// of a directory can contain the file under it, but the first regular file is
// the only thing the caller asked for.
func ExtractFile(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("dockerapi: archive contains no file")
		}
		if err != nil {
			return nil, fmt.Errorf("dockerapi: read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > MaxArchiveBytes {
			return nil, fmt.Errorf("dockerapi: file is %d bytes, limit is %d", hdr.Size, MaxArchiveBytes)
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxArchiveBytes))
		if err != nil {
			return nil, fmt.Errorf("dockerapi: read archive body: %w", err)
		}
		return data, nil
	}
}
