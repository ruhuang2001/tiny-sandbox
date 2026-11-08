package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Retry policy from docs/sdk.md:
//
//   - candidates: transport errors before a complete response, truncated
//     bodies, and statuses 429, 500, 502, 503, 504;
//   - only requests that are safe to repeat are retried, which the caller
//     states with an explicit flag;
//   - backoff min(2s, 200ms * 2^attempt) with full jitter, overridden by
//     Retry-After;
//   - nothing is retried once the context is done.
const (
	baseBackoff = 200 * time.Millisecond
	maxBackoff  = 2 * time.Second
)

// backoff returns the delay before attempt n (0-based). Full jitter spreads a
// thundering herd over the whole cap instead of aligning every client on the
// same instant.
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	limit := baseBackoff << attempt
	if limit > maxBackoff || limit <= 0 {
		limit = maxBackoff
	}
	// A random duration in [limit/2, limit]: enough variety to avoid
	// synchronisation, while still backing off.
	half := limit / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// retryAfter parses a Retry-After header, which the contract defines as
// seconds.
func retryAfter(header http.Header) time.Duration {
	value := header.Get("Retry-After")
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// retryableStatus reports whether a status is worth another attempt.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// request describes one API call. repeatable says whether sending it twice is
// safe; a request that could start a command must set it only when it carries
// an idempotency key.
type request struct {
	method string
	path   string
	query  string
	body   any
	// repeatable allows a retry after a completed attempt. GET, DELETE, PUT,
	// pause, resume, cancel, and keyed starts set it.
	repeatable bool
	// accept asks for an event stream.
	accept string
	// raw sends body as bytes rather than JSON (file uploads).
	raw []byte
}

// response is a completed API response. The body is returned whole so a
// truncated body is detectable before parsing.
type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends req with the retry policy and returns the final response. It
// returns a *Error for transport failures and for retryable statuses once the
// budget is spent.
func (c *Client) do(ctx context.Context, req request) (*response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, req)
		if err == nil && resp.status >= 200 && resp.status < 300 {
			return resp, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, &Error{Code: "canceled", Message: "sandbox: request canceled", Cause: ctx.Err()}
			}
			lastErr = &Error{Code: "transport", Message: "sandbox: request failed", Cause: err}
		} else {
			lastErr = statusError(resp)
		}

		// Only retryable statuses and transport failures are worth another
		// attempt; a 4xx is final even for a repeatable request.
		retryable := err != nil || retryableStatus(resp.status)
		if !retryable || !req.repeatable || attempt >= c.maxRetries {
			return nil, lastErr
		}
		var after time.Duration
		if resp != nil {
			after = retryAfter(resp.header)
		}
		if !sleep(ctx, backoff(attempt, after)) {
			return nil, lastErr
		}
	}
}

// once performs one attempt and reads the body completely, so a body cut short
// is an error here rather than a parse failure later.
func (c *Client) once(ctx context.Context, req request) (*response, error) {
	url := c.domain + req.path + req.query
	var body io.Reader
	contentType := ""
	switch {
	case req.raw != nil:
		body = bytes.NewReader(req.raw)
		contentType = "application/octet-stream"
	case req.body != nil:
		encoded, err := json.Marshal(req.body)
		if err != nil {
			return nil, fmt.Errorf("sandbox: encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, url, body)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build request: %w", err)
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	if req.accept != "" {
		httpReq.Header.Set("Accept", req.accept)
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read response: %w", err)
	}
	return &response{status: httpResp.StatusCode, header: httpResp.Header, body: data}, nil
}

// statusError turns a non-2xx response into a *Error carrying the contract
// code and message.
func statusError(resp *response) *Error {
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(resp.body, &payload)
	message := payload.Message
	if message == "" {
		message = fmt.Sprintf("sandbox: unexpected status %d", resp.status)
	}
	return &Error{StatusCode: resp.status, Code: payload.Code, Message: message}
}

// decode parses a 2xx body into out.
func decode(resp *response, out any) error {
	if err := json.Unmarshal(resp.body, out); err != nil {
		return &Error{
			StatusCode: resp.status,
			Code:       "internal",
			Message:    "sandbox: could not parse the server response",
			Cause:      err,
		}
	}
	return nil
}

// sleep waits for d or until ctx is done, reporting whether it waited fully.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// isCanceled reports whether err is the caller's context ending.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
