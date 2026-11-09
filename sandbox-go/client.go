// Package sandbox is the Go SDK for tiny-sandbox. It speaks the HTTP contract
// in docs/api.md and depends on the standard library only.
//
// The Client covers the sandbox lifecycle, executions with streamed output and
// cancellation, and files. Errors are a single *Error type that matches the
// sentinels in this package with errors.Is, so a caller can branch on an
// outcome (ErrOOM, ErrTimeout, ErrCanceled, ErrEvicted, ErrNonZeroExit)
// without parsing messages.
package sandbox

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Config configures a Client. Zero values fall back to the environment, then
// to the documented defaults.
type Config struct {
	Domain     string       // default: $SANDBOX_DOMAIN, then "http://localhost:8080"
	APIKey     string       // default: $SANDBOX_API_KEY
	HTTPClient *http.Client // default: a client without a global timeout (streams are long-lived)
	MaxRetries int          // default 3; 0 means use the default, negative disables retries
}

// Client talks to one sandboxd endpoint.
type Client struct {
	domain     string
	apiKey     string
	httpClient *http.Client
	maxRetries int
}

// NewClient validates cfg and returns a Client.
func NewClient(cfg Config) (*Client, error) {
	domain := cfg.Domain
	if domain == "" {
		domain = os.Getenv("SANDBOX_DOMAIN")
	}
	if domain == "" {
		domain = "http://localhost:8080"
	}
	if !strings.Contains(domain, "://") {
		domain = "http://" + domain
	}
	u, err := url.Parse(domain)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("sandbox: invalid domain %q", cfg.Domain)
	}

	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("SANDBOX_API_KEY")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	maxRetries := cfg.MaxRetries
	switch {
	case maxRetries == 0:
		maxRetries = 3
	case maxRetries < 0:
		maxRetries = 0
	}

	return &Client{
		domain:     strings.TrimSuffix(u.String(), "/"),
		apiKey:     apiKey,
		httpClient: httpClient,
		maxRetries: maxRetries,
	}, nil
}
