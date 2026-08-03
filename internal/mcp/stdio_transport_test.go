package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStdioTransportSendsNotificationWithoutPendingResponse(t *testing.T) {
	transport, requests, _ := newPipeStdioTransport(t)
	done := make(chan error, 1)
	go func() {
		done <- transport.Notify(context.Background(), "notifications/initialized", map[string]any{})
	}()

	if !requests.Scan() {
		t.Fatal("missing stdio notification")
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(requests.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	if _, ok := message["id"]; ok {
		t.Fatalf("notification contains id: %s", requests.Bytes())
	}
	var method string
	if err := json.Unmarshal(message["method"], &method); err != nil || method != "notifications/initialized" {
		t.Fatalf("notification method = %q, err = %v", method, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("notification waited for a response")
	}
	transport.stateMu.Lock()
	pending := len(transport.pending)
	transport.stateMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending responses = %d", pending)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := transport.Notify(ctx, "notifications/initialized", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled notification error = %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	if err := transport.Notify(context.Background(), "notifications/initialized", nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed notification error = %v", err)
	}
}

func TestStdioTransportDispatchesConcurrentResponsesByID(t *testing.T) {
	transport, requests, responses := newPipeStdioTransport(t)
	type callResult struct {
		method string
		raw    json.RawMessage
		err    error
	}
	results := make(chan callResult, 2)
	for _, method := range []string{"first", "second"} {
		method := method
		go func() {
			raw, err := transport.Call(context.Background(), method, nil)
			results <- callResult{method: method, raw: raw, err: err}
		}()
	}
	requestByMethod := map[string]rpcRequest{}
	for range 2 {
		if !requests.Scan() {
			t.Fatal("missing stdio request")
		}
		var request rpcRequest
		if err := json.Unmarshal(requests.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		requestByMethod[request.Method] = request
	}
	for _, method := range []string{"second", "first"} {
		request := requestByMethod[method]
		response, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"method":"` + method + `"}`)})
		if _, err := responses.Write(append(response, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		result := <-results
		if result.err != nil || !strings.Contains(string(result.raw), result.method) {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestStdioTransportIgnoresLateCanceledResponse(t *testing.T) {
	transport, requests, responses := newPipeStdioTransport(t)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := transport.Call(ctx, "first", nil)
		firstDone <- err
	}()
	if !requests.Scan() {
		t.Fatal("missing first request")
	}
	var first rpcRequest
	if err := json.Unmarshal(requests.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first call error = %v", err)
	}
	late, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: first.ID, Result: json.RawMessage(`{"late":true}`)})
	if _, err := responses.Write(append(late, '\n')); err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan struct {
		raw json.RawMessage
		err error
	}, 1)
	go func() {
		raw, err := transport.Call(context.Background(), "second", nil)
		secondDone <- struct {
			raw json.RawMessage
			err error
		}{raw: raw, err: err}
	}()
	if !requests.Scan() {
		t.Fatal("missing second request")
	}
	var second rpcRequest
	if err := json.Unmarshal(requests.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: second.ID, Result: json.RawMessage(`{"ok":true}`)})
	if _, err := responses.Write(append(response, '\n')); err != nil {
		t.Fatal(err)
	}
	result := <-secondDone
	if result.err != nil || !strings.Contains(string(result.raw), `"ok":true`) {
		t.Fatalf("second result = %+v", result)
	}
}

func TestStdioTransportCloseReleasesPendingCalls(t *testing.T) {
	transport, requests, _ := newPipeStdioTransport(t)
	done := make(chan error, 1)
	go func() {
		_, err := transport.Call(context.Background(), "blocked", nil)
		done <- err
	}()
	if !requests.Scan() {
		t.Fatal("missing request")
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("pending call error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call was not released")
	}
}

func newPipeStdioTransport(t *testing.T) (*StdioTransport, *bufio.Scanner, io.Writer) {
	t.Helper()
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	responseScanner := bufio.NewScanner(responseReader)
	transport := &StdioTransport{
		stdin:   requestWriter,
		scanner: responseScanner,
		logs:    NewLogRingBuffer(32),
		pending: map[int64]chan stdioResult{},
	}
	go transport.readLoop()
	t.Cleanup(func() {
		_ = transport.Close()
		_ = requestReader.Close()
		_ = requestWriter.Close()
		_ = responseWriter.Close()
		_ = responseReader.Close()
	})
	return transport, bufio.NewScanner(requestReader), responseWriter
}

func TestExpandMCPVarsReplacesKnownVariables(t *testing.T) {
	cases := []struct {
		input     string
		workspace string
		home      string
		want      string
	}{
		{"/some/path", "/ws", "/home/user", "/some/path"},
		{"${PROJECT_DIR}/src", "/ws", "/home/user", "/ws/src"},
		{"${HOME}/.bruce", "/ws", "/home/user", "/home/user/.bruce"},
		{"${PROJECT_DIR},${HOME}", "/ws", "/home", "/ws,/home"},
		{"${UNKNOWN}", "/ws", "/home", "${UNKNOWN}"},
		{"", "/ws", "/home", ""},
		{"npx -y @scope/pkg ${PROJECT_DIR}", "/app", "/h", "npx -y @scope/pkg /app"},
	}
	for _, tc := range cases {
		got := expandMCPVars(tc.input, tc.workspace, tc.home)
		if got != tc.want {
			t.Errorf("expandMCPVars(%q, %q, %q) = %q, want %q", tc.input, tc.workspace, tc.home, got, tc.want)
		}
	}
}
