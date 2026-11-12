package daemon

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Handler serves the HTTP contract in docs/api.md over a Sandboxes store. It
// is the only place that translates between JSON and the domain.
type Handler struct {
	store   *Sandboxes
	version string
	apiKey  string
	// shuttingDown is closed when the server begins a graceful shutdown. An
	// event stream would otherwise keep its handler alive until the shutdown
	// deadline expires, and http.Server.Shutdown does not cancel request
	// contexts, so the stream is the one handler that needs to be told.
	shuttingDown <-chan struct{}
	mux          *http.ServeMux
}

// NewHandler returns the server's HTTP handler over store.
func NewHandler(store *Sandboxes, version string) *Handler {
	h := &Handler{store: store, version: version}
	h.mux = h.buildMux(Routes(version))
	return h
}

// WithAPIKey enables bearer authentication for /v1 routes. /healthz stays
// public, as the contract says.
func (h *Handler) WithAPIKey(key string) *Handler {
	h.apiKey = key
	return h
}

// WithShutdown makes event streams end when the channel closes, so a graceful
// shutdown can drain them instead of waiting out its deadline. A stream that
// ends this way is not a failure: the execution keeps running until the store's
// own shutdown removes its container.
func (h *Handler) WithShutdown(done <-chan struct{}) *Handler {
	h.shuttingDown = done
	return h
}

// Handler reports the handler and pattern that would serve req, for the route
// coverage test. It mirrors http.ServeMux.Handler without exposing the mux.
func (h *Handler) Handler(req *http.Request) (http.Handler, string) {
	return h.mux.Handler(req)
}

// ServeHTTP dispatches with authentication applied.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.apiKey != "" && r.URL.Path != "/healthz" && !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, string(CodeUnauthorized), "missing or wrong API key")
		return
	}
	h.mux.ServeHTTP(w, r)
}

// authorized compares the bearer token in constant time.
func (h *Handler) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || len(token) != len(h.apiKey) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.apiKey)) == 1
}

// buildMux turns the route table into a mux. Routes whose handler is nil get
// the store-backed handler for their operation; the health route keeps its
// own.
func (h *Handler) buildMux(routes []Route) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range routes {
		handle := route.Handle
		if handle == nil {
			handle = h.handlerFor(route.Method, route.Path)
		}
		mux.HandleFunc(route.Method+" "+route.Path, handle)
	}
	return mux
}

// handlerFor returns the handler for one contract operation, matching the
// route table's exact method and path template. Path parameters are read with
// Request.PathValue, so the handler does not depend on the router's internals.
func (h *Handler) handlerFor(method, path string) http.HandlerFunc {
	key := method + " " + path
	switch key {
	case "POST /v1/sandboxes":
		return h.createSandbox
	case "GET /v1/sandboxes":
		return h.listSandboxes
	case "GET /v1/sandboxes/{sandbox_id}":
		return h.getSandbox
	case "DELETE /v1/sandboxes/{sandbox_id}":
		return h.deleteSandbox
	case "POST /v1/sandboxes/{sandbox_id}/pause":
		return h.pauseSandbox
	case "POST /v1/sandboxes/{sandbox_id}/resume":
		return h.resumeSandbox
	case "POST /v1/sandboxes/{sandbox_id}/executions":
		return h.startExecution
	case "GET /v1/sandboxes/{sandbox_id}/executions":
		return h.listExecutions
	case "GET /v1/sandboxes/{sandbox_id}/executions/{execution_id}":
		return h.getExecution
	case "POST /v1/sandboxes/{sandbox_id}/executions/{execution_id}/cancel":
		return h.cancelExecution
	case "GET /v1/sandboxes/{sandbox_id}/executions/{execution_id}/events":
		return h.streamEvents
	case "GET /v1/sandboxes/{sandbox_id}/files":
		return h.readFile
	case "PUT /v1/sandboxes/{sandbox_id}/files":
		return h.writeFile
	default:
		return notImplemented
	}
}

// ---------------------------------------------------------------- sandboxes

func (h *Handler) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sb, err := h.store.Create(r.Context(), req)
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/sandboxes/"+sb.ID)
	writeJSON(w, http.StatusCreated, sb)
}

func (h *Handler) listSandboxes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": h.store.List()})
}

func (h *Handler) getSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandbox_id")
	if id == "" {
		writeError(w, http.StatusNotFound, string(CodeNotFound), "sandbox id is required")
		return
	}
	h.store.Touch(id)
	sb, err := h.store.Get(id)
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := h.store.Delete(r.Context(), r.PathValue("sandbox_id"))
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

// pauseSandbox and resumeSandbox differ only in the store method they call.
func (h *Handler) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, h.store.Pause)
}

func (h *Handler) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, h.store.Resume)
}

func (h *Handler) lifecycle(w http.ResponseWriter, r *http.Request, op func(context.Context, string) (Sandbox, error)) {
	sb, err := op(r.Context(), r.PathValue("sandbox_id"))
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

// ---------------------------------------------------------------- executions

func (h *Handler) startExecution(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ex, created, err := h.store.StartExecution(r.Context(), r.PathValue("sandbox_id"), req)
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, ex)
}

func (h *Handler) listExecutions(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListExecutions(r.PathValue("sandbox_id"))
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"executions": list})
}

func (h *Handler) getExecution(w http.ResponseWriter, r *http.Request) {
	ex, err := h.store.GetExecution(r.PathValue("sandbox_id"), r.PathValue("execution_id"))
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

func (h *Handler) cancelExecution(w http.ResponseWriter, r *http.Request) {
	ex, err := h.store.CancelExecution(r.Context(), r.PathValue("sandbox_id"), r.PathValue("execution_id"))
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

// ---------------------------------------------------------------- files

// readFile serves the raw bytes of a file.
func (h *Handler) readFile(w http.ResponseWriter, r *http.Request) {
	path, ok := filePath(w, r)
	if !ok {
		return
	}
	data, err := h.store.ReadFile(r.Context(), r.PathValue("sandbox_id"), path)
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// writeFile stores raw bytes, creating parent directories. The body is bounded
// one byte past the limit so an over-limit upload is a 413 rather than a
// truncated file.
func (h *Handler) writeFile(w http.ResponseWriter, r *http.Request) {
	path, ok := filePath(w, r)
	if !ok {
		return
	}
	body := http.MaxBytesReader(w, r.Body, MaxFileBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, string(CodePayloadTooLarge),
			fmt.Sprintf("file upload exceeds %d bytes", MaxFileBytes))
		return
	}
	if err := h.store.WriteFile(r.Context(), r.PathValue("sandbox_id"), path, data); err != nil {
		writeDaemonError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// filePath validates the shared ?path= parameter.
func filePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	path := r.URL.Query().Get("path")
	if path == "" || path[0] != '/' {
		writeError(w, http.StatusBadRequest, string(CodeInvalidRequest), "path must be absolute")
		return "", false
	}
	return path, true
}

// ---------------------------------------------------------------- events

// streamEvents serves the SSE stream: replay after Last-Event-ID, then live
// events, a ping every 15 seconds while idle, and a close after complete.
func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandbox_id")
	executionID := r.PathValue("execution_id")

	var lastID uint64
	if header := r.Header.Get("Last-Event-ID"); header != "" {
		n, err := strconv.ParseUint(header, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, string(CodeInvalidRequest), "Last-Event-ID must be a non-negative integer")
			return
		}
		lastID = n
	}

	stream, err := h.store.Events(sandboxID, executionID, lastID)
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	defer stream.Close()

	// The stream ends when the client goes away or the server begins to shut
	// down. http.Server.Shutdown does not cancel request contexts, so without
	// the second arm this handler blocks until the shutdown deadline and the
	// container it was streaming outlives the process.
	streamCtx, stopStream := context.WithCancel(r.Context())
	defer stopStream()
	if h.shuttingDown != nil {
		go func() {
			select {
			case <-h.shuttingDown:
				stopStream()
			case <-streamCtx.Done():
			}
		}()
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, string(CodeInternal), "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Events and pings both write to w, so one mutex keeps them from
	// interleaving mid-event. The lock covers the flush as well as the write:
	// Flush and Write share net/http's buffered writer, so a flush on one
	// goroutine while the other writes corrupts the frame. The ping runs
	// separately because the main loop must block on the next event, not on
	// the ping interval, or a terminal event that arrives just after a batch
	// waits a full 15 seconds.
	var writeMu sync.Mutex
	// write sends one frame and flushes it, under the lock. Callers that send
	// a whole batch build the frames first and call this once, so a replay
	// of a long log costs one write and one flush instead of one per event.
	write := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := w.Write(data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	pingDone := make(chan struct{})
	var pingWait sync.WaitGroup
	pingWait.Add(1)
	defer func() {
		// Stop the ping before returning: net/http reuses the response writer
		// after ServeHTTP returns, and a ping still inside Write would race
		// with the next request on the same connection.
		close(pingDone)
		pingWait.Wait()
	}()
	go func() {
		defer pingWait.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				if err := write([]byte(": ping\n\n")); err != nil {
					return
				}
			}
		}
	}()

	for {
		events, done, err := stream.Next(streamCtx)
		if err != nil {
			return
		}
		if len(events) > 0 {
			// One buffer, one write, one flush per batch: replaying a long
			// log would otherwise cost a syscall and a packet per event.
			var batch bytes.Buffer
			for _, ev := range events {
				data, err := renderEvent(ev)
				if err != nil {
					return
				}
				batch.Write(data)
			}
			if err := write(batch.Bytes()); err != nil {
				return
			}
		}
		if done {
			return
		}
	}
}

// renderEvent formats one event in SSE form. Payloads differ by type:
// stdout/stderr carry {"data": "<chunk>"}, truncated carries {"limit_bytes"},
// and complete carries the full Execution.
func renderEvent(ev Event) ([]byte, error) {
	var payload any
	switch ev.Type {
	case EventStdout, EventStderr:
		payload = map[string]string{"data": string(ev.Data)}
	case EventTruncated:
		payload = map[string]int{"limit_bytes": ev.LimitBytes}
	case EventComplete:
		payload = ev.Execution
	default:
		payload = ev.Data
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("daemon: encode %s event: %w", ev.Type, err)
	}
	return fmt.Appendf(nil, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data), nil
}

// ---------------------------------------------------------------- helpers

// decodeJSON reads a bounded JSON body into v. A malformed or oversized body
// has already been answered when it returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty body is allowed for requests whose fields are all
			// optional; the destination keeps its zero values.
			return true
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, string(CodePayloadTooLarge),
				fmt.Sprintf("request body exceeds %d bytes", MaxRequestBodyBytes))
			return false
		}
		writeError(w, http.StatusBadRequest, string(CodeInvalidRequest), "malformed JSON body")
		return false
	}
	return true
}

// writeDaemonError maps a domain error to its contract status.
// writeDaemonError maps a domain error to its contract status. A backend
// failure is also logged with its cause: the client gets "container runtime
// failed", but an operator reading the server log needs the reason, and a
// failure that leaves no trace in the log is undiagnosable from the outside.
func writeDaemonError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		log.Printf("daemon: unexpected error: %v", err)
		writeError(w, http.StatusInternalServerError, string(CodeInternal), "unexpected server error")
		return
	}
	if e.Code == CodeBackendError || e.Code == CodeInternal {
		log.Printf("daemon: %s: %v", e.Code, e)
	}
	writeError(w, statusFor(e.Code), string(e.Code), e.Message)
}

// statusFor maps a contract code to its HTTP status.
func statusFor(code Code) int {
	switch code {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeNotFound:
		return http.StatusNotFound
	case CodeInvalidState, CodeBusy, CodeIdempotencyConflict:
		return http.StatusConflict
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeBackendError:
		return http.StatusBadGateway
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
