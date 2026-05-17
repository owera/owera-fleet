package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerRegisterAndDispatch(t *testing.T) {
	s := NewServer("")
	s.Register("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var in map[string]any
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, NewError(CodeInvalidParams, err.Error())
		}
		return map[string]any{"echoed": in["msg"]}, nil
	})

	httpSrv := httptest.NewServer(s.mux)
	defer httpSrv.Close()

	resp := postRPC(t, httpSrv.URL, `{"jsonrpc":"2.0","method":"echo","params":{"msg":"hi"},"id":1}`)
	if resp.Error != nil {
		t.Fatalf("expected no error, got %v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["echoed"] != "hi" {
		t.Fatalf("expected echoed=hi, got %v", result)
	}
	if string(resp.ID) != "1" {
		t.Fatalf("expected id=1, got %s", string(resp.ID))
	}
}

func TestServerMethodNotFound(t *testing.T) {
	s := NewServer("")
	httpSrv := httptest.NewServer(s.mux)
	defer httpSrv.Close()

	resp := postRPC(t, httpSrv.URL, `{"jsonrpc":"2.0","method":"nope","params":{},"id":2}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("expected CodeMethodNotFound, got %d", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "nope") {
		t.Fatalf("expected method name in error message, got %q", resp.Error.Message)
	}
}

func TestServerMalformedJSON(t *testing.T) {
	s := NewServer("")
	httpSrv := httptest.NewServer(s.mux)
	defer httpSrv.Close()

	resp := postRPC(t, httpSrv.URL, `{not json`)
	if resp.Error == nil || resp.Error.Code != CodeParseError {
		t.Fatalf("expected parse error, got %v", resp.Error)
	}
}

func TestServerWrongJSONRPCVersion(t *testing.T) {
	s := NewServer("")
	httpSrv := httptest.NewServer(s.mux)
	defer httpSrv.Close()

	resp := postRPC(t, httpSrv.URL, `{"jsonrpc":"1.0","method":"x","id":1}`)
	if resp.Error == nil || resp.Error.Code != CodeInvalidRequest {
		t.Fatalf("expected invalid request, got %v", resp.Error)
	}
}

func TestServerHandlerReturnsRPCError(t *testing.T) {
	s := NewServer("")
	s.Register("badparams", func(_ context.Context, _ json.RawMessage) (any, error) {
		return nil, NewError(CodeInvalidParams, "tenant_id required")
	})
	httpSrv := httptest.NewServer(s.mux)
	defer httpSrv.Close()

	resp := postRPC(t, httpSrv.URL, `{"jsonrpc":"2.0","method":"badparams","params":{},"id":3}`)
	if resp.Error == nil || resp.Error.Code != CodeInvalidParams {
		t.Fatalf("expected invalid params, got %v", resp.Error)
	}
	if resp.Error.Message != "tenant_id required" {
		t.Fatalf("expected tenant_id required, got %q", resp.Error.Message)
	}
}

func TestServerHandlerReturnsGenericError(t *testing.T) {
	s := NewServer("")
	s.Register("boom", func(_ context.Context, _ json.RawMessage) (any, error) {
		return nil, errors.New("something exploded")
	})
	httpSrv := httptest.NewServer(s.mux)
	defer httpSrv.Close()

	resp := postRPC(t, httpSrv.URL, `{"jsonrpc":"2.0","method":"boom","params":{},"id":4}`)
	if resp.Error == nil || resp.Error.Code != CodeInternalError {
		t.Fatalf("expected internal error, got %v", resp.Error)
	}
	if resp.Error.Message != "something exploded" {
		t.Fatalf("expected exploded message, got %q", resp.Error.Message)
	}
}

func TestServerNoIDIsRejected(t *testing.T) {
	s := NewServer("")
	s.Register("noop", func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil })
	httpSrv := httptest.NewServer(s.mux)
	defer httpSrv.Close()

	resp := postRPC(t, httpSrv.URL, `{"jsonrpc":"2.0","method":"noop","params":{}}`)
	if resp.Error == nil || resp.Error.Code != CodeInvalidRequest {
		t.Fatalf("expected invalid request for missing id, got %v", resp.Error)
	}
}

func TestServerRejectsGET(t *testing.T) {
	s := NewServer("")
	httpSrv := httptest.NewServer(s.mux)
	defer httpSrv.Close()

	r, err := http.Get(httpSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", r.StatusCode)
	}
}

func TestServerDuplicateRegisterPanics(t *testing.T) {
	s := NewServer("")
	s.Register("dup", func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil })
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	s.Register("dup", func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil })
}

func TestServerListenAndServeShutdown(t *testing.T) {
	// Use ":0" to grab a free port, then verify the server actually
	// services a real network request before shutting down. Covers the
	// happy-path bind + shutdown lifecycle that production uses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("")
	s.Register("ping", func(_ context.Context, _ json.RawMessage) (any, error) {
		return "pong", nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ln) }()

	url := "http://" + ln.Addr().String()
	resp := postRPC(t, url, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	if resp.Result != "pong" {
		t.Fatalf("expected pong, got %v", resp.Result)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-errCh; err != nil && err != http.ErrServerClosed {
		t.Fatalf("serve returned unexpected error: %v", err)
	}
}

// postRPC posts a raw JSON-RPC body and decodes the envelope. Test helper.
func postRPC(t *testing.T, url, body string) response {
	t.Helper()
	r, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response %q: %v", string(raw), err)
	}
	return resp
}
