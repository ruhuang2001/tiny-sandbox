package sandbox

import (
	"context"
	"net/http"
	"net/url"
)

// Sandbox methods. Every call takes a context and returns typed results.

// CreateSandbox creates a sandbox. The call is not idempotent and is never
// retried: a retried create could start a second container.
func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/v1/sandboxes", body: req})
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := decode(resp, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// GetSandbox returns one sandbox.
func (c *Client) GetSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	return c.sandboxOp(ctx, http.MethodGet, sandboxID, "")
}

// ListSandboxes returns every sandbox, newest first.
func (c *Client) ListSandboxes(ctx context.Context) ([]*Sandbox, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/sandboxes", repeatable: true})
	if err != nil {
		return nil, err
	}
	var list sandboxList
	if err := decode(resp, &list); err != nil {
		return nil, err
	}
	return list.Sandboxes, nil
}

// PauseSandbox freezes the sandbox. Idempotent.
func (c *Client) PauseSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	return c.sandboxOp(ctx, http.MethodPost, sandboxID, "/pause")
}

// ResumeSandbox resumes the sandbox. Idempotent.
func (c *Client) ResumeSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	return c.sandboxOp(ctx, http.MethodPost, sandboxID, "/resume")
}

// KillSandbox kills the sandbox and removes its container. Idempotent.
func (c *Client) KillSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	return c.sandboxOp(ctx, http.MethodDelete, sandboxID, "")
}

// sandboxOp runs one operation on a sandbox path. All of them are safe to
// repeat except create, which has its own method.
func (c *Client) sandboxOp(ctx context.Context, method, sandboxID, suffix string) (*Sandbox, error) {
	resp, err := c.do(ctx, request{
		method:     method,
		path:       "/v1/sandboxes/" + url.PathEscape(sandboxID) + suffix,
		repeatable: true,
	})
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := decode(resp, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// ReadFile returns a file inside the sandbox.
func (c *Client) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	resp, err := c.do(ctx, request{
		method:     http.MethodGet,
		path:       "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/files",
		query:      "?path=" + url.QueryEscape(path),
		repeatable: true,
	})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// WriteFile writes a file inside the sandbox, creating parent directories.
func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	_, err := c.do(ctx, request{
		method:     http.MethodPut,
		path:       "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/files",
		query:      "?path=" + url.QueryEscape(path),
		raw:        data,
		repeatable: true,
	})
	return err
}
