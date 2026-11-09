package dockerapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PullImage pulls image from the registry and returns once it is available
// locally.
//
// Why this is needed at all: `docker run` pulls a missing image for you, but
// the Engine API's create call does not. Creating a container from an image the
// daemon does not have answers 404 "No such image", so a server that only calls
// create works on a developer machine (where the image is already there) and
// fails on a clean host. Every path that creates a container therefore pulls
// first when the image is missing.
//
// The pull response is a JSON progress stream, not a single object: one line
// per layer, with an "error" line if the pull fails. A pull that reports a
// stream error is a failed pull even though the HTTP status was 200.
func (c *Client) PullImage(ctx context.Context, image string) error {
	repository, tag := splitImage(image)
	q := url.Values{"fromImage": {repository}}
	if tag != "" {
		q.Set("tag", tag)
	}
	req, err := c.request(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("dockerapi: pull %s: %w", image, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return parsePullStream(resp.Body)
}

// splitImage separates an image reference into repository and tag, leaving the
// repository's own "registry:port" colon alone.
func splitImage(image string) (repository, tag string) {
	// The tag separator is the last colon, and only counts when it appears
	// after the last slash; otherwise it belongs to a registry host:port.
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

// parsePullStream reads the pull progress stream and fails on an error line.
// Progress lines are otherwise ignored: the caller wants the image, not a
// download report.
func parsePullStream(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var payload struct {
			Error string `json:"error"`
		}
		// A line that is not JSON is treated as progress text, not an error.
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		if payload.Error != "" {
			return fmt.Errorf("dockerapi: pull failed: %s", payload.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("dockerapi: read pull stream: %w", err)
	}
	return nil
}

// IsNoSuchImage reports whether err is the daemon's "no such image" answer to
// a create call, which means the image must be pulled first.
func IsNoSuchImage(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.Status == http.StatusNotFound && strings.Contains(strings.ToLower(se.Message), "no such image")
}
