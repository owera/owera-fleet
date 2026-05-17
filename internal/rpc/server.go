// Package rpc implements a minimal JSON-RPC 2.0 server for the operator
// plane. It is the surface owera-cloud (the customer plane) calls into
// over a private Cloudflare tunnel.
//
// Wire format follows the JSON-RPC 2.0 spec:
//
//	→ {"jsonrpc":"2.0","method":"fleet.SubmitJob","params":{...},"id":1}
//	← {"jsonrpc":"2.0","result":{...},"id":1}
//	← {"jsonrpc":"2.0","error":{"code":-32601,"message":"method not found"},"id":1}
//
// Design goals:
//
//   - Stdlib only (net/http + encoding/json). No third-party RPC framework
//     so we don't inherit middleware churn from a dependency.
//   - Handler registration via [Server.Register] is the stable contract
//     other operator-plane subsystems (cancel, health, ...) build on.
//   - No auth layer. Production gates the bind port behind the Cloudflare
//     tunnel, and the default bind is 127.0.0.1 so a misconfigured deploy
//     does not silently expose the surface to the public internet.
//   - Single endpoint ("/" by default). The method dispatch happens inside
//     the JSON-RPC envelope, not in URL routing — clients only need to
//     POST one URL.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// JSON-RPC 2.0 standard error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Handler processes one JSON-RPC request. params is the raw `params`
// field from the envelope; handlers unmarshal it into their own concrete
// request type. The returned value is marshalled into the `result` field;
// errors are translated into the `error` field. To control the wire-level
// error code, return an [*Error]; any other error type collapses to
// [CodeInternalError] with the error's message.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Error is a typed JSON-RPC error. Handlers return *Error when they want
// to set the wire-level error code (e.g. CodeInvalidParams for a malformed
// request, CodeMethodNotFound is reserved for the framework itself).
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// NewError constructs an *Error with the given code and message.
func NewError(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}

// request is the JSON-RPC 2.0 wire envelope for an incoming call.
//
// id is preserved as json.RawMessage so we can echo back the exact bytes
// (the spec allows string, number, or null). Notifications (no id) are
// rejected — this server does not support fire-and-forget.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      json.RawMessage `json:"id"`
}

// response is the JSON-RPC 2.0 wire envelope for a reply. Exactly one of
// Result or Error is populated.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

// Server is a JSON-RPC 2.0 dispatcher served over HTTP. The zero value is
// not ready to use; call [NewServer].
type Server struct {
	addr     string
	mux      *http.ServeMux
	handlers map[string]Handler
	mu       sync.RWMutex

	// httpSrv is set on ListenAndServe / Serve so Shutdown can stop the
	// listener cleanly.
	httpSrv *http.Server

	// readTimeout / writeTimeout bound a single request so a slow-loris
	// client cannot pin a goroutine indefinitely. 30 s is generous for
	// the operator plane — SubmitJob does not block on the underlying
	// fleet dispatch, it returns the task_id immediately.
	readTimeout  time.Duration
	writeTimeout time.Duration
}

// NewServer returns a Server bound to addr (e.g. "127.0.0.1:9091"). The
// returned Server has no handlers registered; call [Server.Register]
// before [Server.ListenAndServe] or every request will 404 at the
// JSON-RPC layer with CodeMethodNotFound.
func NewServer(addr string) *Server {
	if addr == "" {
		addr = "127.0.0.1:9091"
	}
	s := &Server{
		addr:         addr,
		mux:          http.NewServeMux(),
		handlers:     make(map[string]Handler),
		readTimeout:  30 * time.Second,
		writeTimeout: 30 * time.Second,
	}
	s.mux.HandleFunc("/", s.handleHTTP)
	return s
}

// Addr returns the bind address the server was constructed with. After a
// successful Serve on a ":0" address, callers can inspect the actual port
// via [Server.ListenerAddr] instead.
func (s *Server) Addr() string { return s.addr }

// Register adds a JSON-RPC method handler. Method names are exact-match
// strings (no wildcards). A duplicate registration panics so a copy-paste
// bug fails loudly at startup rather than silently shadowing a method.
//
// This signature is the stable contract other subsystems build on.
func (s *Server) Register(method string, h Handler) {
	if method == "" {
		panic("rpc: Register called with empty method name")
	}
	if h == nil {
		panic("rpc: Register called with nil handler for " + method)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.handlers[method]; dup {
		panic("rpc: duplicate registration for method " + method)
	}
	s.handlers[method] = h
}

// Handler returns the registered handler for method, or nil if none.
// Exposed for tests; production code dispatches via the HTTP path.
func (s *Server) Handler(method string) Handler {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handlers[method]
}

// ListenAndServe binds the configured addr and blocks serving requests
// until the listener errors or [Server.Shutdown] is called. Returns
// http.ErrServerClosed on a clean shutdown.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("rpc: listen %s: %w", s.addr, err)
	}
	return s.Serve(ln)
}

// Serve drives the server with a caller-supplied listener. Used by tests
// (with a ":0" listener) and by callers that want to bind the port
// separately for privilege-drop or socket-activation patterns.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.httpSrv = &http.Server{
		Handler:      s.mux,
		ReadTimeout:  s.readTimeout,
		WriteTimeout: s.writeTimeout,
	}
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	return s.httpSrv.Serve(ln)
}

// Shutdown gracefully stops the server. Safe to call before Serve.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.RLock()
	srv := s.httpSrv
	s.mu.RUnlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// handleHTTP is the single HTTP entry point. All JSON-RPC traffic comes
// in here; the method dispatch happens inside the envelope.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "rpc: POST required", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB request cap
	if err != nil {
		writeResponse(w, nil, nil, NewError(CodeParseError, "read body: "+err.Error()))
		return
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeResponse(w, nil, nil, NewError(CodeParseError, "parse: "+err.Error()))
		return
	}
	if req.JSONRPC != "2.0" {
		writeResponse(w, req.ID, nil, NewError(CodeInvalidRequest, `jsonrpc must be "2.0"`))
		return
	}
	if req.Method == "" {
		writeResponse(w, req.ID, nil, NewError(CodeInvalidRequest, "method is required"))
		return
	}
	if len(req.ID) == 0 {
		// Notifications (no id) are not supported. The operator plane
		// always expects a task_id back, so a fire-and-forget shape is
		// almost certainly a client bug.
		writeResponse(w, nil, nil, NewError(CodeInvalidRequest, "notifications (no id) not supported"))
		return
	}

	h := s.Handler(req.Method)
	if h == nil {
		writeResponse(w, req.ID, nil, NewError(CodeMethodNotFound, "method not found: "+req.Method))
		return
	}

	result, err := h(r.Context(), req.Params)
	if err != nil {
		var rpcErr *Error
		if errors.As(err, &rpcErr) {
			writeResponse(w, req.ID, nil, rpcErr)
			return
		}
		writeResponse(w, req.ID, nil, NewError(CodeInternalError, err.Error()))
		return
	}
	writeResponse(w, req.ID, result, nil)
}

// writeResponse marshals the envelope and writes it. id may be nil for
// pre-parse errors where we never saw a request id.
func writeResponse(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *Error) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := response{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr}
	w.Header().Set("Content-Type", "application/json")
	// JSON-RPC errors return HTTP 200 — error transport is in the
	// envelope, not the status code. Only true HTTP-level failures
	// (wrong method, body too big) use non-200.
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
