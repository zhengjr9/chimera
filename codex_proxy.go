package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

func (p *Proxy) handleClaudeViaCodex(reqID string, w http.ResponseWriter, r *http.Request, route routeEntry, body []byte, stream bool, startedAt time.Time) {
	translateStartedAt := time.Now()
	upstreamBody := claudeRequestToCodexResponses(route.model, body)
	log.Printf("[%s] translated claude->codex provider=%s upstream_model=%s req_bytes=%d took=%s", reqID, route.provider.Name, route.model, len(upstreamBody), sinceMS(translateStartedAt))
	logPayloadSummary(reqID, "codex_request", upstreamBody)
	upstreamURL := strings.TrimSuffix(route.provider.BaseURL, "/")
	if upstreamURL == "" {
		upstreamURL = "https://chatgpt.com/backend-api/codex"
	}
	upstreamURL += "/responses"
	ctx, cancel := contextWithTimeout(r, defaultUpstreamTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeClaudeError(w, http.StatusInternalServerError, "api_error", "create request failed")
		return
	}
	if err := p.setCodexProviderHeaders(httpReq, route.provider, true); err != nil {
		writeClaudeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	upstreamStartedAt := time.Now()
	resp, err := p.httpClientForProvider(route.provider).Do(httpReq)
	if err != nil {
		log.Printf("[%s] codex_upstream_error provider=%s took=%s err=%v", reqID, route.provider.Name, sinceMS(upstreamStartedAt), err)
		writeClaudeError(w, http.StatusBadGateway, "api_error", explainUpstreamError(err))
		return
	}
	defer resp.Body.Close()
	log.Printf("[%s] codex_upstream_headers provider=%s status=%d took=%s", reqID, route.provider.Name, resp.StatusCode, sinceMS(upstreamStartedAt))
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
		writeClaudeError(w, resp.StatusCode, "api_error", string(respBody))
		return
	}
	if stream {
		p.streamCodexToClaude(reqID, w, resp.Body, body, startedAt, upstreamStartedAt)
		return
	}
	readStartedAt := time.Now()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
	log.Printf("[%s] codex_upstream_body_read bytes=%d took=%s", reqID, len(respBody), sinceMS(readStartedAt))
	completed := lastSSEDataByType(respBody, "response.completed")
	if len(completed) == 0 {
		writeClaudeError(w, http.StatusBadGateway, "api_error", "codex stream ended without response.completed")
		return
	}
	convertStartedAt := time.Now()
	converted := convertCodexResponseToClaudeNonStream(ctx, body, completed)
	log.Printf("[%s] translated codex->claude resp_bytes=%d took=%s total=%s", reqID, len(converted), sinceMS(convertStartedAt), sinceMS(startedAt))
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(converted))
}

func (p *Proxy) streamCodexToClaude(reqID string, w http.ResponseWriter, body io.Reader, originalRequest []byte, requestStartedAt, upstreamStartedAt time.Time) {
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

	lineCount := 0
	eventCount := 0
	firstUpstreamDataLogged := false
	firstDownstreamEventLogged := false
	var state any
	err := readSSE(body, func(data []byte) error {
		lineCount++
		if len(data) == 0 {
			return nil
		}
		if !firstUpstreamDataLogged {
			firstUpstreamDataLogged = true
			log.Printf("[%s] codex_stream_first_upstream_data since_upstream=%s since_request=%s", reqID, sinceMS(upstreamStartedAt), sinceMS(requestStartedAt))
		}
		line := append([]byte("data: "), data...)
		events := convertCodexStreamToClaude(context.Background(), originalRequest, line, &state)
		for _, event := range events {
			eventCount++
			if !firstDownstreamEventLogged {
				firstDownstreamEventLogged = true
				log.Printf("[%s] codex_stream_first_downstream_event since_upstream=%s since_request=%s", reqID, sinceMS(upstreamStartedAt), sinceMS(requestStartedAt))
			}
			_, _ = w.Write([]byte(event))
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		log.Printf("[%s] codex_stream_error err=%v", reqID, err)
	}
	log.Printf("[%s] codex_stream_done upstream_lines=%d downstream_writes=%d total=%s", reqID, lineCount, eventCount, sinceMS(requestStartedAt))
}

func lastSSEDataByType(raw []byte, wantType string) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 50*1024*1024)
	var last []byte
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(data, "type").String() == wantType {
			last = bytes.Clone(data)
		}
	}
	return last
}

func (p *Proxy) setCodexProviderHeaders(req *http.Request, prov *ProviderConfig, stream bool) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if !stream {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("Version", "0.101.0")
	req.Header.Set("User-Agent", "codex_cli_rs/0.101.0 (chimera)")
	req.Header.Set("Originator", "codex_cli_rs")
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
		return nil
	}
	pool := p.codexPools[prov.Name]
	account, err := pool.pick(req.Context())
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	req.Header.Set("Session_id", buildStableAccountID(time.Now().UTC().Format(time.RFC3339Nano)))
	if account.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", account.AccountID)
	}
	return nil
}

func (p *Proxy) handleCodexLoginStart(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	provider := p.findProviderByName(payload.Provider)
	if provider == nil || !strings.EqualFold(strings.TrimSpace(provider.Type), "codex") {
		writeOpenAIError(w, http.StatusNotFound, "codex provider not found")
		return
	}
	redirectURI := p.codexCallbackURL(r)
	if err := startCodexCallbackForwarder(redirectURI); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	authURL, verifier, state, err := buildCodexLoginURL()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.codexOAuthStates.put(state, codexPendingLogin{ProviderName: provider.Name, CodeVerifier: verifier, RedirectURI: redirectURI, RequestedAt: time.Now().UTC()})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"provider": provider.Name, "state": state, "auth_url": authURL, "redirect_uri": codexRedirectURI, "callback_target": redirectURI})
}

func (p *Proxy) handleCodexLoginCallback(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	login, ok := p.codexOAuthStates.take(state)
	if !ok || code == "" {
		http.Error(w, "invalid oauth callback", http.StatusBadRequest)
		return
	}
	provider := p.findProviderByName(login.ProviderName)
	if provider == nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}
	account, err := exchangeCodexCode(r.Context(), p.httpClientForProvider(provider), code, login.CodeVerifier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	pool := p.codexPools[provider.Name]
	if pool == nil {
		http.Error(w, "provider auth-dir not configured", http.StatusBadRequest)
		return
	}
	if err := pool.save(account); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<html><body><h1>Codex login successful</h1><p>You can close this window.</p></body></html>")
}

func (p *Proxy) handleCodexAccounts(w http.ResponseWriter, r *http.Request) {
	providerName := strings.TrimSpace(r.URL.Query().Get("provider"))
	pool := p.codexPools[providerName]
	if pool == nil {
		writeOpenAIError(w, http.StatusNotFound, "codex provider auth pool not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"provider": providerName, "data": pool.list()})
}

func (p *Proxy) codexCallbackURL(r *http.Request) string {
	base := strings.TrimSpace(p.cfg.Codex.CallbackBaseURL)
	if base == "" {
		host := r.Host
		if host == "" {
			host = fmt.Sprintf("127.0.0.1:%d", p.cfg.Server.Port)
		}
		base = "http://" + host
	}
	return strings.TrimSuffix(base, "/") + "/auth/codex/callback"
}
