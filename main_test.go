package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddlewareDisabledByConfig(t *testing.T) {
	handler := authMiddleware(false, []string{"secret"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestAuthMiddlewareRejectsMissingKeyWhenEnabled(t *testing.T) {
	handler := authMiddleware(true, []string{"secret"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddlewareAcceptsBearerTokenWhenEnabled(t *testing.T) {
	handler := authMiddleware(true, []string{"secret"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestHandleModelsIncludesKiroPrefixedAliases(t *testing.T) {
	p := NewProxy(&Config{
		Providers: []ProviderConfig{
			{
				Name: "kiro-oauth",
				Type: "kiro",
				Models: []ModelConfig{
					{Name: "claude-sonnet-4-5", Alias: "claude-sonnet-4.5"},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
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
	if !seen["kiro/claude-sonnet-4-5"] {
		t.Fatalf("expected kiro-prefixed model in list, got %+v", body.Data)
	}
	if !seen["kiro/claude-sonnet-4.5"] {
		t.Fatalf("expected kiro-prefixed alias in list, got %+v", body.Data)
	}
}
