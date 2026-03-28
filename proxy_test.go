package main

import (
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
