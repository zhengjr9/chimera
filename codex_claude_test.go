package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeRequestToCodexResponses(t *testing.T) {
	req := []byte(`{
		"model":"claude-sonnet-4",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"Read","input":{"file_path":"/tmp/a"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"done"}]}
		],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]
	}`)
	got := claudeRequestToCodexResponses("gpt-5-codex", req)
	parsed := gjson.ParseBytes(got)
	if parsed.Get("model").String() != "gpt-5-codex" {
		t.Fatalf("got=%s", got)
	}
	if parsed.Get("input.1.type").String() != "function_call" {
		t.Fatalf("got=%s", got)
	}
	if parsed.Get("input.1.arguments").Type != gjson.String || parsed.Get("input.1.arguments").String() != `{"file_path":"/tmp/a"}` {
		t.Fatalf("arguments=%s got=%s", parsed.Get("input.1.arguments").Raw, got)
	}
	if parsed.Get("input.2.type").String() != "function_call_output" {
		t.Fatalf("got=%s", got)
	}
}

func TestClaudeRequestToCodexResponses_StrengthensEditDescription(t *testing.T) {
	req := []byte(`{
		"model":"claude-sonnet-4",
		"messages":[{"role":"user","content":[{"type":"text","text":"edit file"}]}],
		"tools":[{"name":"Edit","description":"edit file contents","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]
	}`)
	got := claudeRequestToCodexResponses("gpt-5-codex", req)
	parsed := gjson.ParseBytes(got)
	desc := parsed.Get("tools.0.description").String()
	if !strings.Contains(desc, "read the target file first") || !strings.Contains(desc, "old_string must match") {
		t.Fatalf("description=%q", desc)
	}
}

func TestConvertCodexResponseToClaudeNonStream(t *testing.T) {
	resp := []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5-codex","usage":{"input_tokens":3,"output_tokens":5},"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"/tmp/a\"}"}]}}`)
	req := []byte(`{"tools":[{"name":"Read"}]}`)
	got := convertCodexResponseToClaudeNonStream(context.Background(), req, resp)
	parsed := gjson.Parse(got)
	if parsed.Get("content.0.type").String() != "thinking" || parsed.Get("content.1.type").String() != "text" || parsed.Get("content.2.type").String() != "tool_use" {
		t.Fatalf("got=%s", got)
	}
}

func TestHandleClaudeViaCodex(t *testing.T) {
	transport := claudeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
			t.Fatalf("authorization=%q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "model").String() != "gpt-5-codex" {
			t.Fatalf("body=%s", string(body))
		}
		sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\"}}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from codex\"}]}]}}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})
	dir := t.TempDir()
	pool, err := newCodexAccountPool(dir, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.save(&codexAccount{ID: "acc1", Email: "a@example.com", AccessToken: "codex-token"}); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Providers: []ProviderConfig{{
		Name:    "codex-upstream",
		Type:    "codex",
		BaseURL: "https://chatgpt.com/backend-api/codex",
		AuthDir: dir,
		Models:  []ModelConfig{{Name: "gpt-5-codex", Alias: "claude-sonnet-4"}},
	}}}
	p := NewProxy(cfg)
	p.client = &http.Client{Transport: transport}
	p.providerClients["codex-upstream"] = p.client
	p.codexPools["codex-upstream"] = pool
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.handleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from codex") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestNewCodexAccountPool_LoadsNestedTokensFormat(t *testing.T) {
	dir := t.TempDir()
	payload := map[string]any{
		"last_refresh": "2026-03-23T09:05:54Z",
		"tokens": map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"id_token":      "",
			"account_id":    "acct-1",
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	pool, err := newCodexAccountPool(dir, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	accounts := pool.list()
	if len(accounts) != 1 {
		t.Fatalf("accounts=%d", len(accounts))
	}
	if accounts[0].AccessToken != "access-token" {
		t.Fatalf("access_token=%q", accounts[0].AccessToken)
	}
	if accounts[0].RefreshToken != "refresh-token" {
		t.Fatalf("refresh_token=%q", accounts[0].RefreshToken)
	}
	if accounts[0].AccountID != "acct-1" {
		t.Fatalf("account_id=%q", accounts[0].AccountID)
	}
}

type claudeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f claudeRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
