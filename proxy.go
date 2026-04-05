package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var passthroughRequestHeaders = []string{
	"Accept",
	"OpenAI-Beta",
	"OpenAI-Organization",
	"OpenAI-Project",
	"User-Agent",
	"X-Stainless-Arch",
	"X-Stainless-Async",
	"X-Stainless-Helper-Method",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Raw-Response",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
}

type routeEntry struct {
	provider *ProviderConfig
	model    string
}

type Proxy struct {
	cfg              *Config
	routes           map[string]routeEntry
	client           *http.Client
	providerClients  map[string]*http.Client
	toolDiag         *toolDiagnosticLogger
	codexPools       map[string]*codexAccountPool
	codexOAuthStates *codexOAuthStateStore
	kiroOAuthStates  *kiroOAuthStateStore
}

type streamedToolCallLog struct {
	ID     string
	Name   string
	Args   strings.Builder
	Chunks int
	Logged bool
}

var requestSeq uint64

func NewProxy(cfg *Config) *Proxy {
	defaultClient, err := newHTTPClient("", "", false)
	if err != nil {
		log.Printf("default HTTP client setup failed, falling back to zero-value client: %v", err)
		defaultClient = &http.Client{}
	}
	toolDiag, err := newToolDiagnosticLogger(cfg.Server.ToolDiagnosticsLog)
	if err != nil {
		log.Printf("tool diagnostics disabled: %v", err)
	}
	p := &Proxy{
		cfg:              cfg,
		routes:           make(map[string]routeEntry),
		client:           defaultClient,
		providerClients:  make(map[string]*http.Client),
		toolDiag:         toolDiag,
		codexPools:       make(map[string]*codexAccountPool),
		codexOAuthStates: newCodexOAuthStateStore(),
		kiroOAuthStates:  newKiroOAuthStateStore(),
	}
	for i := range cfg.Providers {
		prov := &cfg.Providers[i]
		client := p.client
		if strings.TrimSpace(prov.Proxy) != "" {
			var err error
			client, err = newHTTPClient(prov.Proxy, prov.CAFile, prov.Insecure)
			if err != nil {
				log.Printf("provider client setup failed for %s, falling back to default client: %v", prov.Name, err)
				client = p.client
			}
		} else if strings.TrimSpace(prov.CAFile) != "" || prov.Insecure {
			var err error
			client, err = newHTTPClient("", prov.CAFile, prov.Insecure)
			if err != nil {
				log.Printf("provider client setup failed for %s, falling back to default client: %v", prov.Name, err)
				client = p.client
			}
		}
		p.providerClients[prov.Name] = client
		if strings.EqualFold(strings.TrimSpace(prov.Type), "codex") && strings.TrimSpace(prov.AuthDir) != "" {
			pool, err := newCodexAccountPool(prov.AuthDir, client)
			if err != nil {
				log.Printf("codex auth pool disabled for provider=%s: %v", prov.Name, err)
			} else if pool != nil {
				p.codexPools[prov.Name] = pool
			}
		}
		log.Printf("provider client configured provider=%s proxy=%s tls=%s", prov.Name, describeProxyMode(prov.Proxy), describeTLSMode(prov.CAFile, prov.Insecure))
		for _, m := range prov.Models {
			p.routes[m.Name] = routeEntry{provider: prov, model: m.Name}
			for _, extra := range providerModelAliases(*prov, m) {
				p.routes[extra] = routeEntry{provider: prov, model: m.Name}
			}
			for _, alias := range modelAliases(m) {
				p.routes[alias] = routeEntry{provider: prov, model: m.Name}
				for _, extra := range providerAliasVariants(*prov, alias) {
					p.routes[extra] = routeEntry{provider: prov, model: m.Name}
				}
			}
		}
	}
	return p
}

func (p *Proxy) Close() error {
	stopCodexCallbackForwarder()
	if p == nil || p.toolDiag == nil {
		return nil
	}
	return p.toolDiag.Close()
}

func (p *Proxy) findProviderByName(name string) *ProviderConfig {
	if p == nil {
		return nil
	}
	trimmed := strings.TrimSpace(name)
	for i := range p.cfg.Providers {
		if p.cfg.Providers[i].Name == trimmed {
			return &p.cfg.Providers[i]
		}
	}
	return nil
}

func (p *Proxy) httpClientForProvider(prov *ProviderConfig) *http.Client {
	if p == nil {
		return &http.Client{}
	}
	if prov != nil {
		if client := p.providerClients[prov.Name]; client != nil {
			return client
		}
	}
	if p.client != nil {
		return p.client
	}
	return &http.Client{}
}

// POST /v1/messages — accept Claude protocol, proxy as OpenAI
func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	reqID := nextRequestID("claude")
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeClaudeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "read body failed")
		return
	}

	modelName := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	log.Printf("[%s] inbound method=%s path=%s model=%s stream=%t body_bytes=%d", reqID, r.Method, r.URL.Path, modelName, stream, len(body))
	logPayloadSummary(reqID, "claude_request", body)
	p.logToolDiagnosticsFromClaudeRequest(reqID, r.URL.Path, body)

	route, ok := p.resolve(modelName)
	if !ok {
		writeClaudeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("model %q not found", modelName))
		return
	}
	if strings.EqualFold(strings.TrimSpace(route.provider.Type), "codex") {
		p.handleClaudeViaCodex(reqID, w, r, route, body, stream, startedAt)
		return
	}
	if isKiroProviderType(route.provider.Type) {
		p.handleClaudeViaKiro(reqID, w, r, route, body, stream, startedAt)
		return
	}

	// Translate Claude -> OpenAI
	translateStartedAt := time.Now()
	openaiReq := claudeRequestToOpenAI(body, route.model, stream)
	log.Printf("[%s] translated claude->openai provider=%s upstream_model=%s req_bytes=%d took=%s", reqID, route.provider.Name, route.model, len(openaiReq), sinceMS(translateStartedAt))
	logPayloadSummary(reqID, "openai_request", openaiReq)
	p.logToolDiagnosticsFromOpenAIRequest(reqID, "claude_to_openai", r.URL.Path, openaiReq)

	upstreamURL := strings.TrimSuffix(route.provider.BaseURL, "/") + "/chat/completions"
	ctx, cancel := contextWithTimeout(r, defaultUpstreamTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(openaiReq))
	if err != nil {
		writeClaudeError(w, http.StatusInternalServerError, "api_error", "create request failed")
		return
	}
	p.setProviderHeaders(httpReq, route.provider, r.Header)

	upstreamStartedAt := time.Now()
	resp, err := p.httpClientForProvider(route.provider).Do(httpReq)
	if err != nil {
		log.Printf("[%s] upstream_error provider=%s url=%s took=%s err=%v", reqID, route.provider.Name, upstreamURL, sinceMS(upstreamStartedAt), err)
		writeClaudeError(w, http.StatusBadGateway, "api_error", explainUpstreamError(err))
		return
	}
	defer resp.Body.Close()
	log.Printf("[%s] upstream_headers provider=%s status=%d took=%s", reqID, route.provider.Name, resp.StatusCode, sinceMS(upstreamStartedAt))

	if stream {
		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
			log.Printf("[%s] stream_error status=%d resp_bytes=%d total=%s", reqID, resp.StatusCode, len(respBody), sinceMS(startedAt))
			writeClaudeError(w, resp.StatusCode, "api_error", string(respBody))
			return
		}
		p.streamClaudeResponse(reqID, w, io.LimitReader(resp.Body, maxRequestBodySize), startedAt, upstreamStartedAt)
	} else {
		readStartedAt := time.Now()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
		log.Printf("[%s] upstream_body_read bytes=%d took=%s", reqID, len(respBody), sinceMS(readStartedAt))
		logPayloadSummary(reqID, "openai_response", respBody)
		p.logToolDiagnosticsFromOpenAIResponse(reqID, "claude_nonstream", "/chat/completions", respBody)
		if resp.StatusCode != http.StatusOK {
			log.Printf("[%s] nonstream_error status=%d total=%s", reqID, resp.StatusCode, sinceMS(startedAt))
			writeClaudeError(w, resp.StatusCode, "api_error", string(respBody))
			return
		}
		convertStartedAt := time.Now()
		claudeResp := openaiResponseToClaude(respBody)
		log.Printf("[%s] translated openai->claude resp_bytes=%d took=%s total=%s", reqID, len(claudeResp), sinceMS(convertStartedAt), sinceMS(startedAt))
		w.Header().Set("Content-Type", "application/json")
		w.Write(claudeResp)
	}
}

// streamClaudeResponse converts upstream SSE into Claude SSE events incrementally.
func (p *Proxy) streamClaudeResponse(reqID string, w http.ResponseWriter, body io.Reader, requestStartedAt, upstreamStartedAt time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeClaudeError(w, http.StatusInternalServerError, "api_error", "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	conv := newStreamConverter()
	toolCallLogs := newStreamedToolCallLogs()
	lineCount := 0
	eventCount := 0
	firstUpstreamDataLogged := false
	firstDownstreamEventLogged := false

	err := readSSE(body, func(data []byte) error {
		lineCount++
		if len(data) == 0 {
			return nil
		}
		if !firstUpstreamDataLogged {
			firstUpstreamDataLogged = true
			log.Printf("[%s] claude_stream_first_upstream_data since_upstream=%s since_request=%s", reqID, sinceMS(upstreamStartedAt), sinceMS(requestStartedAt))
		}
		logStreamChunkSummary(reqID, "upstream_chunk", data)
		finishReason := updateStreamedToolCallLogs(toolCallLogs, data)
		if finishReason != "" {
			logCompletedStreamedToolCalls(reqID, "upstream_summary", finishReason, toolCallLogs)
		}
		p.logToolDiagnosticsFromOpenAIStreamChunk(reqID, "claude_stream", "/chat/completions", data)
		events := conv.convert(data)
		if len(events) > 0 {
			eventCount++
			if !firstDownstreamEventLogged {
				firstDownstreamEventLogged = true
				log.Printf("[%s] claude_stream_first_downstream_event since_upstream=%s since_request=%s", reqID, sinceMS(upstreamStartedAt), sinceMS(requestStartedAt))
			}
			w.Write(events)
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		log.Printf("[%s] claude_stream_scan_error err=%v", reqID, err)
	}

	// Finalize
	events := conv.finalize()
	if len(events) > 0 {
		eventCount++
		w.Write(events)
		flusher.Flush()
	}
	logCompletedStreamedToolCalls(reqID, "upstream_summary", "stream_end", toolCallLogs)
	log.Printf("[%s] claude_stream_done upstream_lines=%d downstream_writes=%d total=%s", reqID, lineCount, eventCount, sinceMS(requestStartedAt))
}

// POST /v1/chat/completions — passthrough OpenAI protocol
func (p *Proxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	p.handleOpenAIPassthrough(w, r, "/chat/completions", "chat/completions", true)
}

// POST /v1/responses — passthrough OpenAI Responses protocol
func (p *Proxy) handleResponses(w http.ResponseWriter, r *http.Request) {
	reqID := nextRequestID("responses")
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read body failed")
		return
	}

	modelName := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	log.Printf("[%s] inbound method=%s path=%s model=%s stream=%t body_bytes=%d", reqID, r.Method, r.URL.Path, modelName, stream, len(body))

	route, ok := p.resolve(modelName)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", modelName))
		return
	}

	upstreamBody := convertResponsesRequestToChatCompletions(route.model, body, stream)
	logPayloadSummary(reqID, "responses_request", body)
	log.Printf("[%s] responses_request_full body=%s", reqID, string(body))
	logPayloadSummary(reqID, "responses_as_chat_completions", upstreamBody)
	log.Printf("[%s] responses_as_chat_completions_full body=%s", reqID, string(upstreamBody))
	p.logToolDiagnosticsFromOpenAIRequest(reqID, "responses_to_chat_completions", r.URL.Path, upstreamBody)

	upstreamURL := strings.TrimSuffix(route.provider.BaseURL, "/") + "/chat/completions"
	ctx, cancel := contextWithTimeout(r, defaultUpstreamTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "create request failed")
		return
	}
	p.setProviderHeaders(httpReq, route.provider, r.Header)

	upstreamStartedAt := time.Now()
	resp, err := p.httpClientForProvider(route.provider).Do(httpReq)
	if err != nil {
		log.Printf("[%s] upstream_error provider=%s url=%s took=%s err=%v", reqID, route.provider.Name, upstreamURL, sinceMS(upstreamStartedAt), err)
		writeOpenAIError(w, http.StatusBadGateway, explainUpstreamError(err))
		return
	}
	defer resp.Body.Close()
	log.Printf("[%s] upstream_headers provider=%s status=%d took=%s", reqID, route.provider.Name, resp.StatusCode, sinceMS(upstreamStartedAt))

	if stream {
		p.streamResponsesFromChatCompletions(reqID, w, resp, body, startedAt, upstreamStartedAt)
		return
	}

	readStartedAt := time.Now()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
	log.Printf("[%s] upstream_body_read bytes=%d took=%s", reqID, len(respBody), sinceMS(readStartedAt))
	logChatCompletionsResponseSummary(reqID, "responses_upstream_response", respBody)
	p.logToolDiagnosticsFromOpenAIResponse(reqID, "responses_nonstream", "/chat/completions", respBody)
	if resp.StatusCode != http.StatusOK {
		log.Printf("[%s] nonstream_error status=%d total=%s summary=%s", reqID, resp.StatusCode, sinceMS(startedAt), summarizeErrorBody(resp.Header.Get("Content-Type"), respBody))
		writeOpenAIError(w, resp.StatusCode, string(respBody))
		return
	}

	convertStartedAt := time.Now()
	converted := convertChatCompletionsResponseToResponses(body, respBody)
	logResponsesSummary(reqID, "responses_converted_response", []byte(converted))
	log.Printf("[%s] translated chat_completions->responses resp_bytes=%d took=%s total=%s", reqID, len(converted), sinceMS(convertStartedAt), sinceMS(startedAt))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(converted))
}

// POST /v1/responses/compact — passthrough OpenAI compact responses protocol
func (p *Proxy) handleResponsesCompact(w http.ResponseWriter, r *http.Request) {
	reqID := nextRequestID("responses-compact")
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read body failed")
		return
	}

	modelName := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	if stream {
		writeOpenAIError(w, http.StatusBadRequest, "streaming not supported for this endpoint")
		return
	}
	log.Printf("[%s] inbound method=%s path=%s model=%s stream=%t body_bytes=%d", reqID, r.Method, r.URL.Path, modelName, stream, len(body))

	route, ok := p.resolve(modelName)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", modelName))
		return
	}

	upstreamBody := convertResponsesRequestToChatCompletions(route.model, body, false)
	logPayloadSummary(reqID, "responses_compact_request", body)
	log.Printf("[%s] responses_compact_request_full body=%s", reqID, string(body))
	logPayloadSummary(reqID, "responses_compact_as_chat_completions", upstreamBody)
	log.Printf("[%s] responses_compact_as_chat_completions_full body=%s", reqID, string(upstreamBody))

	upstreamURL := strings.TrimSuffix(route.provider.BaseURL, "/") + "/chat/completions"
	ctx, cancel := contextWithTimeout(r, defaultUpstreamTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "create request failed")
		return
	}
	p.setProviderHeaders(httpReq, route.provider, r.Header)

	resp, err := p.httpClientForProvider(route.provider).Do(httpReq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, explainUpstreamError(err))
		return
	}
	defer resp.Body.Close()
	log.Printf("[%s] upstream_headers provider=%s status=%d took=%s", reqID, route.provider.Name, resp.StatusCode, sinceMS(startedAt))

	readStartedAt := time.Now()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
	log.Printf("[%s] upstream_body_read bytes=%d took=%s", reqID, len(respBody), sinceMS(readStartedAt))
	logChatCompletionsResponseSummary(reqID, "responses_compact_upstream_response", respBody)
	if resp.StatusCode != http.StatusOK {
		log.Printf("[%s] nonstream_error status=%d total=%s summary=%s", reqID, resp.StatusCode, sinceMS(startedAt), summarizeErrorBody(resp.Header.Get("Content-Type"), respBody))
		writeOpenAIError(w, resp.StatusCode, string(respBody))
		return
	}

	convertStartedAt := time.Now()
	converted := convertChatCompletionsResponseToResponses(body, respBody)
	logResponsesSummary(reqID, "responses_compact_converted_response", []byte(converted))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(converted))
	log.Printf("[%s] translated chat_completions->responses_compact resp_bytes=%d took=%s total=%s", reqID, len(converted), sinceMS(convertStartedAt), sinceMS(startedAt))
}

func (p *Proxy) streamResponsesFromChatCompletions(reqID string, w http.ResponseWriter, resp *http.Response, originalRequest []byte, requestStartedAt, upstreamStartedAt time.Time) {
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
		log.Printf("[%s] responses_stream_error status=%d resp_bytes=%d total=%s summary=%s", reqID, resp.StatusCode, len(respBody), sinceMS(requestStartedAt), summarizeErrorBody(resp.Header.Get("Content-Type"), respBody))
		writeOpenAIError(w, resp.StatusCode, string(respBody))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var state any
	toolCallLogs := newStreamedToolCallLogs()
	lineCount := 0
	eventCount := 0
	firstUpstreamDataLogged := false
	firstDownstreamEventLogged := false
	err := readSSE(resp.Body, func(data []byte) error {
		lineCount++
		if len(data) == 0 {
			return nil
		}
		if !firstUpstreamDataLogged {
			firstUpstreamDataLogged = true
			log.Printf("[%s] responses_stream_first_upstream_data since_upstream=%s since_request=%s", reqID, sinceMS(upstreamStartedAt), sinceMS(requestStartedAt))
		}
		logStreamChunkSummary(reqID, "responses_upstream_chunk", data)
		finishReason := updateStreamedToolCallLogs(toolCallLogs, data)
		if finishReason != "" {
			logCompletedStreamedToolCalls(reqID, "responses_upstream_summary", finishReason, toolCallLogs)
		}
		p.logToolDiagnosticsFromOpenAIStreamChunk(reqID, "responses_stream", "/chat/completions", data)
		line := append([]byte("data: "), data...)
		events := convertChatCompletionsStreamToResponses(originalRequest, line, &state)
		for _, event := range events {
			eventCount++
			if !firstDownstreamEventLogged {
				firstDownstreamEventLogged = true
				log.Printf("[%s] responses_stream_first_downstream_event since_upstream=%s since_request=%s", reqID, sinceMS(upstreamStartedAt), sinceMS(requestStartedAt))
			}
			logResponsesEventSummary(reqID, "responses_downstream_event", []byte(event))
			_, _ = w.Write([]byte(event))
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		log.Printf("[%s] responses_stream_error upstream_lines=%d downstream_writes=%d total=%s err=%v", reqID, lineCount, eventCount, sinceMS(requestStartedAt), err)
		return
	}
	logCompletedStreamedToolCalls(reqID, "responses_upstream_summary", "stream_end", toolCallLogs)
	log.Printf("[%s] responses_stream_done upstream_lines=%d downstream_writes=%d total=%s", reqID, lineCount, eventCount, sinceMS(requestStartedAt))
}

func (p *Proxy) handleOpenAIPassthrough(w http.ResponseWriter, r *http.Request, upstreamPath, logLabel string, allowStreaming bool) {
	reqID := nextRequestID("openai")
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read body failed")
		return
	}

	modelName := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	if stream && !allowStreaming {
		writeOpenAIError(w, http.StatusBadRequest, "streaming not supported for this endpoint")
		return
	}
	log.Printf("[%s] inbound method=%s path=%s model=%s stream=%t body_bytes=%d", reqID, r.Method, r.URL.Path, modelName, stream, len(body))
	logPayloadSummary(reqID, "openai_passthrough_request "+logLabel, body)
	p.logToolDiagnosticsFromOpenAIRequest(reqID, "openai_passthrough_"+strings.ReplaceAll(logLabel, "/", "_"), r.URL.Path, body)

	route, ok := p.resolve(modelName)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", modelName))
		return
	}

	upstreamBody := rewriteModelInRequest(body, route.model)

	upstreamURL := strings.TrimSuffix(route.provider.BaseURL, "/") + upstreamPath
	ctx, cancel := contextWithTimeout(r, defaultUpstreamTimeout)
	defer cancel()

	upstreamStartedAt := time.Now()
	var resp *http.Response
	if isOllamaProviderType(route.provider.Type) {
		resp, err = p.doOllamaRequestWithRetry(ctx, route.provider, r.Header, http.MethodPost, upstreamURL, upstreamBody)
		if err != nil {
			log.Printf("[%s] upstream_error provider=%s url=%s took=%s err=%v", reqID, route.provider.Name, upstreamURL, sinceMS(upstreamStartedAt), err)
			writeOpenAIError(w, http.StatusBadGateway, explainUpstreamError(err))
			return
		}
	} else {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "create request failed")
			return
		}
		p.setProviderHeaders(httpReq, route.provider, r.Header)
		resp, err = p.httpClientForProvider(route.provider).Do(httpReq)
		if err != nil {
			log.Printf("[%s] upstream_error provider=%s url=%s took=%s err=%v", reqID, route.provider.Name, upstreamURL, sinceMS(upstreamStartedAt), err)
			writeOpenAIError(w, http.StatusBadGateway, explainUpstreamError(err))
			return
		}
	}
	defer resp.Body.Close()
	log.Printf("[%s] upstream_headers provider=%s status=%d took=%s", reqID, route.provider.Name, resp.StatusCode, sinceMS(upstreamStartedAt))

	// Forward upstream headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if stream {
		copyStreamResponse(reqID, w, resp.Body, startedAt, upstreamStartedAt)
		return
	}
	n, _ := io.Copy(w, resp.Body)
	log.Printf("[%s] nonstream_done resp_bytes=%d total=%s", reqID, n, sinceMS(startedAt))
}

// GET /v1/models
func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	var models []modelEntry
	seen := make(map[string]bool)

	for _, prov := range p.cfg.Providers {
		for _, m := range prov.Models {
			names := []string{m.Name}
			names = append(names, providerModelAliases(prov, m)...)
			names = append(names, modelAliases(m)...)
			for _, alias := range modelAliases(m) {
				names = append(names, providerAliasVariants(prov, alias)...)
			}
			for _, name := range names {
				if seen[name] {
					continue
				}
				seen[name] = true
				models = append(models, modelEntry{
					ID:      name,
					Object:  "model",
					Created: time.Now().Unix(),
					OwnedBy: prov.Name,
				})
			}
		}
		if isOllamaProviderType(prov.Type) && strings.TrimSpace(prov.BaseURL) != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			dynamic, err := p.fetchOllamaModels(ctx, prov)
			cancel()
			if err != nil {
				log.Printf("ollama dynamic model list failed provider=%s err=%v", prov.Name, err)
			} else {
				for _, name := range dynamic {
					for _, modelID := range []string{name, ollamaModelPrefix + name} {
						if seen[modelID] {
							continue
						}
						seen[modelID] = true
						models = append(models, modelEntry{
							ID:      modelID,
							Object:  "model",
							Created: time.Now().Unix(),
							OwnedBy: prov.Name,
						})
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

func modelAliases(m ModelConfig) []string {
	if len(m.Aliases) > 0 {
		return m.Aliases
	}
	if m.Alias != "" {
		return []string{m.Alias}
	}
	return nil
}

func providerModelAliases(prov ProviderConfig, m ModelConfig) []string {
	return providerAliasVariants(prov, m.Name)
}

func providerAliasVariants(prov ProviderConfig, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if isKiroProviderType(prov.Type) {
		return []string{"kiro/" + name}
	}
	if isOllamaProviderType(prov.Type) {
		return []string{ollamaModelPrefix + name}
	}
	return nil
}

// -- helpers --

func (p *Proxy) setProviderHeaders(req *http.Request, prov *ProviderConfig, inbound http.Header) {
	req.Header.Set("Content-Type", "application/json")
	p.setProviderHeadersWithAPIKey(req, prov, inbound, prov.APIKey)
}

func (p *Proxy) setProviderHeadersWithAPIKey(req *http.Request, prov *ProviderConfig, inbound http.Header, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	for _, key := range passthroughRequestHeaders {
		for _, value := range inbound.Values(key) {
			req.Header.Add(key, value)
		}
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	// if the request already has a shorter deadline, use that
	ctx := r.Context()
	if deadline, ok := ctx.Deadline(); ok {
		if time.Until(deadline) < d {
			return ctx, func() {}
		}
	}
	return context.WithTimeout(ctx, d)
}

func copyStreamResponse(reqID string, w http.ResponseWriter, body io.Reader, requestStartedAt, upstreamStartedAt time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		io.Copy(w, body)
		return
	}

	buf := make([]byte, 32*1024)
	chunkCount := 0
	firstChunkLogged := false
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunkCount++
			if !firstChunkLogged {
				firstChunkLogged = true
				log.Printf("[%s] openai_stream_first_chunk since_upstream=%s since_request=%s chunk_bytes=%d", reqID, sinceMS(upstreamStartedAt), sinceMS(requestStartedAt), n)
			}
			w.Write(buf[:n])
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				log.Printf("[%s] openai_stream_done chunks=%d total=%s", reqID, chunkCount, sinceMS(requestStartedAt))
			} else {
				log.Printf("[%s] openai_stream_error chunks=%d total=%s err=%v", reqID, chunkCount, sinceMS(requestStartedAt), err)
			}
			if err != io.EOF {
				return
			}
			return
		}
	}
}

func explainUpstreamError(err error) string {
	if err == nil {
		return "upstream: unknown error"
	}
	msg := err.Error()
	if strings.Contains(msg, "proxyconnect tcp") {
		hint := "upstream proxy connection failed; check HTTPS_PROXY/https_proxy/ALL_PROXY and NO_PROXY settings"
		if strings.Contains(msg, "lookup http ") || strings.Contains(msg, "lookup http:") {
			hint += " (the proxy URL may be malformed, for example a duplicated `http://` prefix)"
		}
		return fmt.Sprintf("%s; original error: %s", hint, msg)
	}
	return "upstream: " + msg
}

func nextRequestID(prefix string) string {
	return fmt.Sprintf("%s-%06d", prefix, atomic.AddUint64(&requestSeq, 1))
}

func sinceMS(startedAt time.Time) string {
	return time.Since(startedAt).Round(time.Millisecond).String()
}

func newStreamedToolCallLogs() map[int]*streamedToolCallLog {
	return make(map[int]*streamedToolCallLog)
}

func updateStreamedToolCallLogs(state map[int]*streamedToolCallLog, data []byte) string {
	root := gjson.ParseBytes(data)
	if tcs := root.Get("choices.0.delta.tool_calls"); tcs.Exists() && tcs.IsArray() {
		tcs.ForEach(func(_, tc gjson.Result) bool {
			idx := int(tc.Get("index").Int())
			entry, ok := state[idx]
			if !ok {
				entry = &streamedToolCallLog{}
				state[idx] = entry
			}
			if id := strings.TrimSpace(tc.Get("id").String()); id != "" {
				entry.ID = id
			}
			if name := strings.TrimSpace(tc.Get("function.name").String()); name != "" {
				entry.Name = name
			}
			if args := tc.Get("function.arguments").String(); args != "" {
				entry.Args.WriteString(args)
				entry.Chunks++
			}
			return true
		})
	}
	return strings.TrimSpace(root.Get("choices.0.finish_reason").String())
}

func logCompletedStreamedToolCalls(reqID, label, finishReason string, state map[int]*streamedToolCallLog) {
	for idx, entry := range state {
		if entry == nil || entry.Logged {
			continue
		}
		log.Printf("[%s] %s tool_call_complete index=%d id=%s name=%s arg_chunks=%d finish_reason=%s args=%s",
			reqID,
			label,
			idx,
			trimForLog(entry.ID),
			entry.Name,
			entry.Chunks,
			finishReason,
			trimForLog(entry.Args.String()),
		)
		entry.Logged = true
	}
}

func logPayloadSummary(reqID, label string, body []byte) {
	root := gjson.ParseBytes(body)

	if model := root.Get("model").String(); model != "" {
		log.Printf("[%s] %s model=%s", reqID, label, model)
	}

	if msgs := root.Get("messages"); msgs.Exists() && msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "tool_use":
					log.Printf("[%s] %s tool_use role=%s id=%s name=%s input=%s", reqID, label, role, trimForLog(part.Get("id").String()), part.Get("name").String(), trimForLog(part.Get("input").Raw))
				case "tool_result":
					log.Printf("[%s] %s tool_result role=%s tool_use_id=%s is_error=%t content=%s", reqID, label, role, trimForLog(part.Get("tool_use_id").String()), part.Get("is_error").Bool(), trimForLog(part.Get("content").Raw))
				}
				return true
			})
			return true
		})
	}

	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			name := tool.Get("name").String()
			if name == "" {
				name = tool.Get("function.name").String()
			}
			log.Printf("[%s] %s declared_tool name=%s schema=%s", reqID, label, name, trimForLog(firstNonEmpty(tool.Get("input_schema").Raw, tool.Get("function.parameters").Raw)))
			return true
		})
	}
}

func logStreamChunkSummary(reqID, label string, data []byte) {
	root := gjson.ParseBytes(data)

	if errMsg := firstNonEmpty(root.Get("error.message").String(), root.Get("message").String()); errMsg != "" && root.Get("error").Exists() {
		log.Printf("[%s] %s error=%s", reqID, label, trimForLog(errMsg))
	}

	if tcs := root.Get("choices.0.delta.tool_calls"); tcs.Exists() && tcs.IsArray() {
		tcs.ForEach(func(_, tc gjson.Result) bool {
			log.Printf("[%s] %s tool_call index=%d id=%s name=%s args=%s", reqID, label, tc.Get("index").Int(), trimForLog(tc.Get("id").String()), tc.Get("function.name").String(), trimForLog(tc.Get("function.arguments").String()))
			return true
		})
	}

	if fr := root.Get("choices.0.finish_reason").String(); fr != "" {
		log.Printf("[%s] %s finish_reason=%s", reqID, label, fr)
	}
}

func logChatCompletionsResponseSummary(reqID, label string, body []byte) {
	root := gjson.ParseBytes(body)
	if id := root.Get("id").String(); id != "" {
		log.Printf("[%s] %s id=%s", reqID, label, trimForLog(id))
	}
	if model := root.Get("model").String(); model != "" {
		log.Printf("[%s] %s model=%s", reqID, label, model)
	}
	if tcs := root.Get("choices.0.message.tool_calls"); tcs.Exists() && tcs.IsArray() {
		tcs.ForEach(func(_, tc gjson.Result) bool {
			log.Printf("[%s] %s tool_call id=%s name=%s args=%s", reqID, label, trimForLog(tc.Get("id").String()), tc.Get("function.name").String(), trimForLog(tc.Get("function.arguments").String()))
			return true
		})
	}
	if content := root.Get("choices.0.message.content").String(); content != "" {
		log.Printf("[%s] %s content=%s", reqID, label, trimForLog(content))
	}
	if fr := root.Get("choices.0.finish_reason").String(); fr != "" {
		log.Printf("[%s] %s finish_reason=%s", reqID, label, fr)
	}
}

func logResponsesSummary(reqID, label string, body []byte) {
	root := gjson.ParseBytes(body)
	if id := root.Get("id").String(); id != "" {
		log.Printf("[%s] %s id=%s", reqID, label, trimForLog(id))
	}
	if model := root.Get("model").String(); model != "" {
		log.Printf("[%s] %s model=%s", reqID, label, model)
	}
	if output := root.Get("output"); output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "function_call":
				log.Printf("[%s] %s function_call id=%s call_id=%s name=%s args=%s", reqID, label, trimForLog(item.Get("id").String()), trimForLog(item.Get("call_id").String()), item.Get("name").String(), trimForLog(item.Get("arguments").String()))
			case "message":
				log.Printf("[%s] %s message role=%s text=%s", reqID, label, item.Get("role").String(), trimForLog(item.Get("content.0.text").String()))
			}
			return true
		})
	}
}

func logResponsesEventSummary(reqID, label string, body []byte) {
	eventLine := string(body)
	if strings.HasPrefix(eventLine, "event: ") {
		if newline := strings.IndexByte(eventLine, '\n'); newline >= 0 {
			eventName := strings.TrimSpace(eventLine[len("event: "):newline])
			log.Printf("[%s] %s type=%s", reqID, label, eventName)
			if dataIndex := strings.Index(eventLine, "\ndata: "); dataIndex >= 0 {
				logStreamChunkSummary(reqID, label+" "+eventName, []byte(eventLine[dataIndex+7:]))
			}
		}
	}
}

func summarizeErrorBody(contentType string, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if strings.Contains(strings.ToLower(contentType), "application/json") || gjson.ValidBytes(body) {
		if msg := firstNonEmpty(
			gjson.GetBytes(body, "error.message").String(),
			gjson.GetBytes(body, "message").String(),
			gjson.GetBytes(body, "detail").String(),
		); msg != "" {
			return trimForLog(msg)
		}
	}
	return trimForLog(string(body))
}

func trimForLog(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const maxLen = 240
	if len(s) > maxLen {
		return s[:maxLen] + "...(truncated)"
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func rewriteModelInRequest(body []byte, model string) []byte {
	if model == "" {
		return body
	}
	if !gjson.ValidBytes(body) {
		return body
	}
	rewritten, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return body
	}
	return rewritten
}

func readSSE(r io.Reader, onData func([]byte) error) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	var dataLines [][]byte

	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := bytes.Join(dataLines, []byte("\n"))
		dataLines = dataLines[:0]
		return onData(bytes.TrimSpace(payload))
	}

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if err2 := flush(); err2 != nil {
				return err2
			}
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data := line[len("data:"):]
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
			dataLines = append(dataLines, append([]byte(nil), data...))
		}

		if errors.Is(err, io.EOF) {
			if err2 := flush(); err2 != nil {
				return err2
			}
			return nil
		}
	}
}

func writeClaudeError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, errType, msg)
}

func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"server_error","param":null,"code":null}}`, msg)
}
