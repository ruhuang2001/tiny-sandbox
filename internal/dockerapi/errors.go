package dockerapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// StatusError is a non-2xx answer from the Engine API. Docker reports failures
// as {"message": "..."}; the status is what callers switch on.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("dockerapi: unexpected status %d", e.Status)
	}
	return fmt.Sprintf("dockerapi: status %d: %s", e.Status, e.Message)
}

// statusError reads an error response into a *StatusError. A body that is not
// Docker's JSON error shape is kept as the message, truncated.
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var payload struct {
		Message string `json:"message"`
	}
	msg := string(body)
	if json.Unmarshal(body, &payload) == nil && payload.Message != "" {
		msg = payload.Message
	}
	return &StatusError{Status: resp.StatusCode, Message: msg}
}

// IsNotFound reports whether err is a 404 from the daemon: the addressed
// container, exec or file does not exist.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// IsConflict reports whether err is a 409: a name is taken, or the container
// is paused and must be resumed first.
func IsConflict(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusConflict
}
