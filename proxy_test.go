package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type flushRecorder struct {
	header     http.Header
	statusCode int
	writes     chan []byte
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{
		header: http.Header{},
		writes: make(chan []byte, 16),
	}
}

func (r *flushRecorder) Header() http.Header {
	return r.header
}

func (r *flushRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
}

func (r *flushRecorder) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	r.writes <- cp
	return len(p), nil
}

func (r *flushRecorder) Flush() {}

func TestHandleMessagesStreamsBeforeUpstreamCompletes(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "glm-5", Alias: "claude-test"}},
		}},
	}
	p := NewProxy(cfg)

	pr, pw := io.Pipe()
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})

	go func() {
		io.WriteString(pw, "data: {\"id\":\"msg_1\",\"model\":\"glm-5\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		time.Sleep(300 * time.Millisecond)
		io.WriteString(pw, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		io.WriteString(pw, "data: [DONE]\n\n")
		pw.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := newFlushRecorder()

	done := make(chan struct{})
	go func() {
		p.handleMessages(rec, req)
		close(done)
	}()

	select {
	case first := <-rec.writes:
		if !strings.Contains(string(first), "message_start") && !strings.Contains(string(first), "content_block_start") {
			t.Fatalf("expected first streamed event, got %q", string(first))
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("expected streamed response before upstream completed")
	}

	<-done
}

func TestHandleMessagesParsesMultilineSSEData(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "glm-5", Alias: "claude-test"}},
		}},
	}
	p := NewProxy(cfg)

	pr, pw := io.Pipe()
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})

	go func() {
		io.WriteString(pw, "event: message\n")
		io.WriteString(pw, "data: {\"id\":\"msg_1\",\"model\":\"glm-5\",\"choices\":[{\"delta\":{\"content\":\"hello")
		io.WriteString(pw, "\"}}]}\n\n")
		io.WriteString(pw, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		io.WriteString(pw, "data: [DONE]\n\n")
		pw.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := newFlushRecorder()

	done := make(chan struct{})
	go func() {
		p.handleMessages(rec, req)
		close(done)
	}()

	var joined strings.Builder
	for {
		select {
		case chunk := <-rec.writes:
			joined.Write(chunk)
			if strings.Contains(joined.String(), "message_stop") {
				goto messageDone
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for streamed chunks, got %q", joined.String())
		}
	}
messageDone:

	if !strings.Contains(joined.String(), `"text":"hello"`) {
		t.Fatalf("expected multiline SSE payload to be parsed into text delta, got %q", joined.String())
	}

	<-done
}

func TestHandleChatCompletionsFlushesStreamingChunks(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "glm-5"}},
		}},
	}
	p := NewProxy(cfg)

	pr, pw := io.Pipe()
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})

	go func() {
		io.WriteString(pw, "data: first\n\n")
		time.Sleep(300 * time.Millisecond)
		io.WriteString(pw, "data: second\n\n")
		pw.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := newFlushRecorder()

	done := make(chan struct{})
	go func() {
		p.handleChatCompletions(rec, req)
		close(done)
	}()

	select {
	case first := <-rec.writes:
		if string(first) != "data: first\n\n" {
			t.Fatalf("expected first chunk to flush immediately, got %q", string(first))
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("expected first upstream chunk to be flushed before stream completion")
	}

	<-done
}

func TestHandleChatCompletionsRewritesAliasModelForUpstream(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "glm-5", Alias: "claude-sonnet-4"}},
		}},
	}
	p := NewProxy(cfg)

	var gotBody string
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		gotBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)),
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	p.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(gotBody, `"model":"glm-5"`) {
		t.Fatalf("expected upstream request model to be rewritten, got %s", gotBody)
	}
	if strings.Contains(gotBody, `"model":"claude-sonnet-4"`) {
		t.Fatalf("expected alias model to be removed from upstream request, got %s", gotBody)
	}
}

func TestHandleResponsesUsesChatCompletionsEndpoint(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub/v1",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "gpt-5-codex"}},
		}},
	}
	p := NewProxy(cfg)

	var gotPath string
	var gotAuth string
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_123","object":"response","model":"gpt-5-codex"}`)),
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"hi"}`))
	rec := httptest.NewRecorder()

	p.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want %q", gotPath, "/v1/chat/completions")
	}
	if gotAuth != "Bearer token" {
		t.Fatalf("authorization = %q, want %q", gotAuth, "Bearer token")
	}
	if !strings.Contains(rec.Body.String(), `"object":"response"`) {
		t.Fatalf("unexpected body %q", rec.Body.String())
	}
}

func TestHandleResponsesFlushesStreamingChunks(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "gpt-5-codex"}},
		}},
	}
	p := NewProxy(cfg)

	pr, pw := io.Pipe()
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})

	go func() {
		io.WriteString(pw, "data: {\"id\":\"chatcmpl_123\",\"object\":\"chat.completion.chunk\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		time.Sleep(300 * time.Millisecond)
		io.WriteString(pw, "data: {\"id\":\"chatcmpl_123\",\"object\":\"chat.completion.chunk\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		pw.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","stream":true,"input":"hi"}`))
	rec := newFlushRecorder()

	done := make(chan struct{})
	go func() {
		p.handleResponses(rec, req)
		close(done)
	}()

	select {
	case first := <-rec.writes:
		if !strings.Contains(string(first), "response.created") {
			t.Fatalf("expected converted responses event, got %q", string(first))
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("expected first upstream chunk to be flushed before stream completion")
	}

	<-done
}

func TestHandleResponsesParsesMultilineSSEData(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "gpt-5-codex"}},
		}},
	}
	p := NewProxy(cfg)

	pr, pw := io.Pipe()
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})

	go func() {
		io.WriteString(pw, "data: {\"id\":\"chatcmpl_123\",\"object\":\"chat.completion.chunk\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he")
		io.WriteString(pw, "llo\"},\"finish_reason\":null}]}\n\n")
		io.WriteString(pw, "data: {\"id\":\"chatcmpl_123\",\"object\":\"chat.completion.chunk\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		pw.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","stream":true,"input":"hi"}`))
	rec := newFlushRecorder()

	done := make(chan struct{})
	go func() {
		p.handleResponses(rec, req)
		close(done)
	}()

	var joined strings.Builder
	for {
		select {
		case chunk := <-rec.writes:
			joined.Write(chunk)
			if strings.Contains(joined.String(), "response.completed") {
				goto responsesDone
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for converted response stream, got %q", joined.String())
		}
	}
responsesDone:

	if !strings.Contains(joined.String(), `"delta":"hello"`) {
		t.Fatalf("expected multiline SSE payload to be converted, got %q", joined.String())
	}

	<-done
}

func TestHandleResponsesCompactUsesChatCompletionsEndpoint(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub/v1",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "gpt-5-codex"}},
		}},
	}
	p := NewProxy(cfg)

	var gotPath string
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_123","object":"response","output_text":"hi"}`)),
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5-codex","input":"hi"}`))
	rec := httptest.NewRecorder()

	p.handleResponsesCompact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want %q", gotPath, "/v1/chat/completions")
	}
}

func TestHandleResponsesCompactRejectsStreaming(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "stub",
			BaseURL: "http://stub/v1",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "gpt-5-codex"}},
		}},
	}
	p := NewProxy(cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5-codex","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()

	p.handleResponsesCompact(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "streaming not supported") {
		t.Fatalf("unexpected body %q", rec.Body.String())
	}
}

func TestHandleChatCompletionsExplainsProxyErrors(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "minimax",
			BaseURL: "https://api.minimax.io/v1",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "MiniMax-M2.7"}},
		}},
	}
	p := NewProxy(cfg)

	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New(`Post "https://api.minimax.io/v1/chat/completions": proxyconnect tcp: dial tcp: lookup http on 33.128.32.3:53: server misbehaving`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"MiniMax-M2.7","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	p.handleChatCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream proxy connection failed") {
		t.Fatalf("expected proxy hint in body, got %q", body)
	}
	if !strings.Contains(body, "duplicated `http://` prefix") {
		t.Fatalf("expected malformed proxy hint in body, got %q", body)
	}
}

func TestHandleMessagesExplainsProxyErrors(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "minimax",
			BaseURL: "https://api.minimax.io/v1",
			APIKey:  "token",
			Models:  []ModelConfig{{Name: "MiniMax-M2.7", Alias: "claude-test"}},
		}},
	}
	p := NewProxy(cfg)

	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New(`Post "https://api.minimax.io/v1/chat/completions": proxyconnect tcp: dial tcp: lookup http on 33.128.32.3:53: server misbehaving`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	p.handleMessages(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream proxy connection failed") {
		t.Fatalf("expected proxy hint in body, got %q", body)
	}
	if !strings.Contains(body, "duplicated `http://` prefix") {
		t.Fatalf("expected malformed proxy hint in body, got %q", body)
	}
}

func TestHandleChatCompletionsUsesProviderSpecificClient(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{
				Name:    "default",
				BaseURL: "http://default",
				APIKey:  "default-token",
				Models:  []ModelConfig{{Name: "default-model"}},
			},
			{
				Name:    "minimax",
				BaseURL: "https://api.minimax.io/v1",
				APIKey:  "minimax-token",
				Proxy:   "direct",
				Models:  []ModelConfig{{Name: "MiniMax-M2.7"}},
			},
		},
	}
	p := NewProxy(cfg)

	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("default client should not be used for minimax")
	})
	p.providerClients["minimax"] = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer minimax-token" {
			t.Fatalf("authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)),
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"MiniMax-M2.7","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	p.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleModelsIncludesDynamicOllamaModels(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "ollama",
			Type:    "ollama",
			BaseURL: "http://ollama",
			APIKey:  "k1",
		}},
	}
	p := NewProxy(cfg)
	p.providerClients["ollama"] = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "http://ollama/v1/models" {
			t.Fatalf("unexpected models url %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"llama3.2"},{"id":"qwen2.5-coder"}]}`)),
		}, nil
	})}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	seen := map[string]bool{}
	for _, item := range body.Data {
		seen[item.ID] = true
	}
	if !seen["ollama/llama3.2"] || !seen["ollama/qwen2.5-coder"] {
		t.Fatalf("expected dynamic ollama-prefixed models, got %+v", body.Data)
	}
}

func TestHandleChatCompletionsResolvesOllamaPrefixedModel(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "ollama",
			Type:    "ollama",
			BaseURL: "http://ollama",
			APIKey:  "k1",
		}},
	}
	p := NewProxy(cfg)
	var gotBody string
	p.providerClients["ollama"] = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		gotBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)),
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"ollama/llama3.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotBody, `"model":"llama3.2"`) {
		t.Fatalf("expected stripped upstream model, got %s", gotBody)
	}
}

func TestHandleChatCompletionsRetriesOllamaWithMultipleAPIKeys(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name:    "ollama",
			Type:    "ollama",
			BaseURL: "http://ollama",
			APIKeys: []string{"k1", "k2"},
		}},
	}
	p := NewProxy(cfg)
	attempts := 0
	var authHeaders []string
	p.providerClients["ollama"] = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)),
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"ollama/llama3.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if attempts < 2 {
		t.Fatalf("expected retry attempts, got %d", attempts)
	}
	if len(authHeaders) < 2 || authHeaders[0] != "Bearer k1" || authHeaders[1] != "Bearer k2" {
		t.Fatalf("expected api key rotation, got %v", authHeaders)
	}
}
