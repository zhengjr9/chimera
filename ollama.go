package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	ollamaProviderType        = "ollama"
	ollamaModelPrefix         = "ollama/"
	ollamaDefaultRetryMax     = 3
	ollamaInitialRetryBackoff = 500 * time.Millisecond
)

func isOllamaProviderType(providerType string) bool {
	return strings.EqualFold(strings.TrimSpace(providerType), ollamaProviderType)
}

func providerAPIKeys(prov ProviderConfig) []string {
	keys := make([]string, 0, len(prov.APIKeys)+1)
	if strings.TrimSpace(prov.APIKey) != "" {
		keys = append(keys, strings.TrimSpace(prov.APIKey))
	}
	for _, key := range prov.APIKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		duplicate := false
		for _, existing := range keys {
			if existing == key {
				duplicate = true
				break
			}
		}
		if !duplicate {
			keys = append(keys, key)
		}
	}
	return keys
}

func (p *Proxy) defaultOllamaProvider() *ProviderConfig {
	if p == nil || p.cfg == nil {
		return nil
	}
	var matches []*ProviderConfig
	for i := range p.cfg.Providers {
		prov := &p.cfg.Providers[i]
		if isOllamaProviderType(prov.Type) {
			matches = append(matches, prov)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return nil
}

func (p *Proxy) resolve(model string) (routeEntry, bool) {
	r, ok := p.routes[model]
	if ok {
		return r, true
	}
	trimmed := strings.TrimSpace(model)
	if strings.HasPrefix(trimmed, ollamaModelPrefix) {
		if prov := p.defaultOllamaProvider(); prov != nil {
			upstreamModel := strings.TrimSpace(strings.TrimPrefix(trimmed, ollamaModelPrefix))
			if upstreamModel != "" {
				return routeEntry{provider: prov, model: upstreamModel}, true
			}
		}
	}
	return routeEntry{}, false
}

func (p *Proxy) fetchOllamaModels(ctx context.Context, prov ProviderConfig) ([]string, error) {
	keys := providerAPIKeys(prov)
	if len(keys) == 0 {
		keys = []string{""}
	}
	var lastErr error
	for _, key := range keys {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(prov.BaseURL, "/")+"/v1/models", nil)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(key) != "" {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
		}
		for k, v := range prov.Headers {
			req.Header.Set(k, v)
		}
		resp, err := p.httpClientForProvider(&prov).Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("ollama model list failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
			continue
		}
		ids := make([]string, 0)
		for _, item := range gjson.GetBytes(respBody, "data").Array() {
			id := strings.TrimSpace(item.Get("id").String())
			if id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			lastErr = fmt.Errorf("ollama model list response missing data[].id")
			continue
		}
		return ids, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("ollama model list failed")
	}
	return nil, lastErr
}

func (p *Proxy) doOllamaRequestWithRetry(ctx context.Context, prov *ProviderConfig, inbound http.Header, method, url string, body []byte) (*http.Response, error) {
	if prov == nil {
		return nil, fmt.Errorf("ollama provider missing")
	}
	keys := providerAPIKeys(*prov)
	if len(keys) == 0 {
		keys = []string{""}
	}
	var lastErr error
	backoff := ollamaInitialRetryBackoff
	for attempt := 0; attempt < ollamaDefaultRetryMax; attempt++ {
		for _, key := range keys {
			req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			p.setProviderHeadersWithAPIKey(req, prov, inbound, key)
			resp, err := p.httpClientForProvider(prov).Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			if !shouldRetryOllamaStatus(resp.StatusCode) {
				return resp, nil
			}
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
			resp.Body.Close()
			lastErr = fmt.Errorf("ollama upstream failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		if attempt < ollamaDefaultRetryMax-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("ollama upstream failed")
	}
	return nil, lastErr
}

func shouldRetryOllamaStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}
