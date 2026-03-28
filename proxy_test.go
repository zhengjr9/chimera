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
