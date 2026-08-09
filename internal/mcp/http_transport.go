package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"bruce-go/internal/config"
)

const httpTransportTimeout = 30 * time.Second

type HTTPTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	nextID  atomic.Int64
}

var _ Transport = (*HTTPTransport)(nil)

func NewHTTPTransport(cfg config.MCPServerSetting) *HTTPTransport {
	return &HTTPTransport{
		url:     cfg.URL,
		headers: cfg.Headers,
		client:  &http.Client{Timeout: httpTransportTimeout},
	}
}

func (t *HTTPTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	resp, err := t.post(ctx, rpcRequest{JSONRPC: "2.0", ID: t.nextID.Add(1), Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("MCP HTTP %d", resp.StatusCode)
	}
	return decodeRPCResponse(resp.Body)
}

func (t *HTTPTransport) Notify(ctx context.Context, method string, params any) error {
	resp, err := t.post(ctx, rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("MCP HTTP %d", resp.StatusCode)
	}
	return nil
}

func (t *HTTPTransport) post(ctx context.Context, message any) (*http.Response, error) {
	if strings.TrimSpace(t.url) == "" {
		return nil, errors.New("MCP HTTP URL must not be empty")
	}
	reqBody, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	return t.client.Do(req)
}

func (*HTTPTransport) Close() error   { return nil }
func (*HTTPTransport) Logs() []string { return nil }

func decodeRPCResponse(r io.Reader) (json.RawMessage, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if bytes.HasPrefix(data, []byte("data:")) {
		lines := bytes.Split(data, []byte{'\n'})
		for _, line := range lines {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				data = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				break
			}
		}
	}
	var resp rpcResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, errors.New(resp.Error.Message)
	}
	return resp.Result, nil
}
