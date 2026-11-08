// Package dockerapi is a minimal client for the Docker Engine API over a unix
// socket. It covers exactly the operations internal/dockerbackend needs:
// container create, start, remove, pause, unpause, kill and list; exec create,
// start and inspect; and the archive API for files.
//
// It knows nothing about sandboxes. Parsers and request shaping live here so
// they can be unit-tested without a daemon.
package dockerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxErrorBody bounds how much of an error response is read into a message.
const maxErrorBody = 64 << 10

// Client sends requests to one Docker daemon. It is safe for concurrent use.
type Client struct {
	base string // scheme://host; the socket decides where it really goes
	http *http.Client
	// version is the path prefix of the negotiated API version, such as
	// "/v1.52". Empty until Negotiate succeeds.
	version string
}

// New returns a client for base using hc. Call Negotiate before the first
// request; Dial does it for you.
func New(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc}
}

// Dial connects to the Docker socket at socketPath and negotiates the API
// version. The returned client has no request timeout: exec streams are as
// long-lived as the command they carry, and callers bound requests with their
// own contexts.
func Dial(ctx context.Context, socketPath string) (*Client, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:       16,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	c := &Client{base: "http://docker", http: &http.Client{Transport: tr}}
	if err := c.Negotiate(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Version returns the negotiated API version, such as "1.52", or an empty
// string before negotiation.
func (c *Client) Version() string { return strings.TrimPrefix(c.version, "/v") }

// Negotiate asks the daemon for its API version and prefixes later requests
// with it. Using the daemon's own version means a newer client speaks the
// dialect the server actually implements.
func (c *Client) Negotiate(ctx context.Context) error {
	var v struct {
		APIVersion string `json:"ApiVersion"`
		Version    string `json:"Version"`
	}
	if err := c.getJSON(ctx, "/version", &v); err != nil {
		return fmt.Errorf("dockerapi: negotiate: %w", err)
	}
	if v.APIVersion == "" {
		return errors.New("dockerapi: negotiate: daemon reported no API version")
	}
	c.version = "/v" + v.APIVersion
	return nil
}

// ---------------------------------------------------------------- containers

// ContainerCreate creates a container and returns its ID.
func (c *Client) ContainerCreate(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := c.postJSON(ctx, "/containers/create", q, cfg, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("dockerapi: create returned no container id")
	}
	return out.ID, nil
}

// ContainerStart starts a created container and returns once it is running.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
}

// ContainerRemove force-removes a container and everything running in it.
func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	q := url.Values{"force": {"1"}, "v": {"1"}}
	return c.send(ctx, http.MethodDelete, "/containers/"+id, q, nil)
}

// ContainerInspect returns one container's state.
func (c *Client) ContainerInspect(ctx context.Context, id string) (ContainerInspect, error) {
	var out ContainerInspect
	if err := c.getJSON(ctx, "/containers/"+id+"/json", &out); err != nil {
		return ContainerInspect{}, err
	}
	return out, nil
}

// ContainerPause freezes every process in a container.
func (c *Client) ContainerPause(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodPost, "/containers/"+id+"/pause", nil, nil)
}

// ContainerUnpause resumes a paused container.
func (c *Client) ContainerUnpause(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodPost, "/containers/"+id+"/unpause", nil, nil)
}

// ContainerKill sends a signal to a container's init process. The Docker
// daemon reports a container that is already stopped without an error.
func (c *Client) ContainerKill(ctx context.Context, id, signal string) error {
	q := url.Values{}
	if signal != "" {
		q.Set("signal", signal)
	}
	return c.send(ctx, http.MethodPost, "/containers/"+id+"/kill", q, nil)
}

// ContainerList returns the containers matching filters. all selects stopped
// containers too, which liveness needs: with all set, absence from the list
// means the container no longer exists.
func (c *Client) ContainerList(ctx context.Context, all bool, filters map[string][]string) ([]Container, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	if len(filters) > 0 {
		raw, err := json.Marshal(filters)
		if err != nil {
			return nil, fmt.Errorf("dockerapi: encode filters: %w", err)
		}
		q.Set("filters", string(raw))
	}
	var out []Container
	if err := c.getJSONQuery(ctx, "/containers/json", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- exec

// ExecCreate registers a command in a container and returns the exec ID.
// Nothing runs until ExecStart.
func (c *Client) ExecCreate(ctx context.Context, containerID string, command []string) (string, error) {
	body := execConfig{
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  false,
		Tty:          false,
		Cmd:          command,
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.postJSON(ctx, "/containers/"+containerID+"/exec", nil, body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("dockerapi: exec create returned no id")
	}
	return out.ID, nil
}

// ExecStart runs a registered exec and returns its output stream. The stream
// is multiplexed in Docker's 8-byte frame format; use Demux to read it. It
// reaches EOF when the process exits.
//
// The caller must close it. The stream's lifetime is the command's, not the
// caller's context: pass a context that stays valid while the command runs.
func (c *Client) ExecStart(ctx context.Context, execID string) (io.ReadCloser, error) {
	q := url.Values{"detach": {"false"}}
	body, err := json.Marshal(execStart{Detach: false, Tty: false})
	if err != nil {
		return nil, fmt.Errorf("dockerapi: encode exec start: %w", err)
	}
	req, err := c.request(ctx, http.MethodPost, "/exec/"+execID+"/start", q, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ExecInspect reports an exec's current state and, once it is done, its exit
// code. ExitCode is nil while the process runs.
func (c *Client) ExecInspect(ctx context.Context, execID string) (ExecInspect, error) {
	var out ExecInspect
	if err := c.getJSON(ctx, "/exec/"+execID+"/json", &out); err != nil {
		return ExecInspect{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------- archive

// ArchiveGet streams a tar archive of path from a container. The caller must
// close it and read it with ExtractFile.
func (c *Client) ArchiveGet(ctx context.Context, containerID, path string) (io.ReadCloser, error) {
	q := url.Values{"path": {path}}
	req, err := c.request(ctx, http.MethodGet, "/containers/"+containerID+"/archive", q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/x-tar")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ArchivePut extracts a tar archive into path inside a container. Docker
// creates missing parent directories for the archive's entries.
func (c *Client) ArchivePut(ctx context.Context, containerID, path string, tarBody []byte) error {
	q := url.Values{"path": {path}}
	req, err := c.request(ctx, http.MethodPut, "/containers/"+containerID+"/archive", q, bytes.NewReader(tarBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// ---------------------------------------------------------------- transport

func (c *Client) path(p string) string { return c.version + p }

func (c *Client) request(ctx context.Context, method, p string, q url.Values, body io.Reader) (*http.Request, error) {
	u := c.base + c.path(p)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("dockerapi: build request: %w", err)
	}
	return req, nil
}

// do sends the request and turns a non-2xx answer into a *StatusError.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dockerapi: %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	return nil, statusError(resp)
}

// send performs a request whose response carries nothing of interest.
func (c *Client) send(ctx context.Context, method, p string, q url.Values, body []byte) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := c.request(ctx, method, p, q, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (c *Client) getJSON(ctx context.Context, p string, out any) error {
	return c.getJSONQuery(ctx, p, nil, out)
}

func (c *Client) getJSONQuery(ctx context.Context, p string, q url.Values, out any) error {
	req, err := c.request(ctx, http.MethodGet, p, q, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("dockerapi: decode %s: %w", p, err)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, p string, q url.Values, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("dockerapi: encode %s: %w", p, err)
	}
	req, err := c.request(ctx, http.MethodPost, p, q, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("dockerapi: decode %s: %w", p, err)
	}
	return nil
}
