package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	kiroOAuthAuthService = "https://prod.us-east-1.auth.desktop.kiro.dev"
	kiroOAuthSigninURL   = "https://app.kiro.dev/signin"
)

type kiroPendingLogin struct {
	ProviderName string
	CodeVerifier string
	RedirectURI  string
	Method       string
	RequestedAt  time.Time
}

type kiroOAuthStateStore struct {
	mu     sync.Mutex
	states map[string]kiroPendingLogin
}

func newKiroOAuthStateStore() *kiroOAuthStateStore {
	return &kiroOAuthStateStore{states: make(map[string]kiroPendingLogin)}
}

func (s *kiroOAuthStateStore) put(state string, login kiroPendingLogin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state] = login
}

func (s *kiroOAuthStateStore) take(state string) (kiroPendingLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	login, ok := s.states[state]
	if ok {
		delete(s.states, state)
	}
	return login, ok
}

func buildKiroLoginURL(redirectURI, method string) (string, string, string, error) {
	state, err := generateRandomURLSafe(24)
	if err != nil {
		return "", "", "", err
	}
	verifier, challenge, err := generatePKCECodes()
	if err != nil {
		return "", "", "", err
	}
	idp := "google"
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "", "google":
		idp = "google"
	case "github":
		idp = "github"
	default:
		return "", "", "", fmt.Errorf("unsupported kiro login method %q", method)
	}
	values := url.Values{
		"idp":                {idp},
		"redirectUri":        {redirectURI},
		"codeChallenge":      {challenge},
		"codeChallengeMethod": {"S256"},
		"state":              {state},
	}
	return kiroOAuthSigninURL + "?" + values.Encode(), verifier, state, nil
}

func exchangeKiroCode(ctx context.Context, client *http.Client, code, verifier, redirectURI string) (kiroCredentials, error) {
	reqBody := map[string]any{
		"code":          code,
		"code_verifier": verifier,
		"redirect_uri":  redirectURI,
	}
	payload, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroOAuthAuthService+"/oauth/token", strings.NewReader(string(payload)))
	if err != nil {
		return kiroCredentials{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "chimera/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return kiroCredentials{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
	if resp.StatusCode != http.StatusOK {
		return kiroCredentials{}, fmt.Errorf("kiro token exchange failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	var token struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(respBody, &token); err != nil {
		return kiroCredentials{}, err
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return kiroCredentials{}, fmt.Errorf("kiro token exchange response missing accessToken")
	}
	creds := kiroCredentials{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ProfileARN:   token.ProfileARN,
		AuthMethod:   "social",
		Region:       kiroDefaultRegion,
		ExpiresAt:    time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339),
	}
	if token.ExpiresIn == 0 {
		creds.ExpiresAt = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	}
	return creds, nil
}

func saveKiroCredentials(path string, creds kiroCredentials) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("kiro credentials path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing := kiroCredentials{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &existing)
	}
	mergeKiroCredentials(&existing, creds)
	body, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func (p *Proxy) handleKiroLoginStart(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Provider string `json:"provider"`
		Method   string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	provider := p.findProviderByName(payload.Provider)
	if provider == nil || !isKiroProviderType(provider.Type) {
		writeOpenAIError(w, http.StatusNotFound, "kiro provider not found")
		return
	}
	if strings.TrimSpace(provider.AuthDir) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "kiro provider auth-dir not configured")
		return
	}
	redirectURI := p.kiroCallbackURL(r)
	authURL, verifier, state, err := buildKiroLoginURL(redirectURI, payload.Method)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.kiroOAuthStates.put(state, kiroPendingLogin{
		ProviderName: provider.Name,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
		Method:       firstNonEmpty(payload.Method, "google"),
		RequestedAt:  time.Now().UTC(),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"provider":     provider.Name,
		"method":       firstNonEmpty(payload.Method, "google"),
		"state":        state,
		"auth_url":     authURL,
		"redirect_uri": redirectURI,
	})
}

func (p *Proxy) handleKiroLoginCallback(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if errCode := strings.TrimSpace(r.URL.Query().Get("error")); errCode != "" {
		http.Error(w, "kiro oauth error: "+errCode, http.StatusBadRequest)
		return
	}
	login, ok := p.kiroOAuthStates.take(state)
	if !ok || code == "" {
		http.Error(w, "invalid oauth callback", http.StatusBadRequest)
		return
	}
	provider := p.findProviderByName(login.ProviderName)
	if provider == nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}
	creds, err := exchangeKiroCode(r.Context(), p.httpClientForProvider(provider), code, login.CodeVerifier, login.RedirectURI)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	targetPath := strings.TrimSpace(provider.AuthDir)
	if info, err := os.Stat(targetPath); err == nil && info.IsDir() {
		targetPath = filepath.Join(targetPath, kiroAuthTokenFile)
	} else if strings.HasSuffix(targetPath, string(os.PathSeparator)) {
		targetPath = filepath.Join(targetPath, kiroAuthTokenFile)
	}
	if err := saveKiroCredentials(targetPath, creds); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<html><body><h1>Kiro login successful</h1><p>You can close this window.</p></body></html>")
}

func (p *Proxy) kiroCallbackURL(r *http.Request) string {
	base := strings.TrimSpace(p.cfg.Kiro.CallbackBaseURL)
	if base == "" {
		host := r.Host
		if host == "" {
			host = fmt.Sprintf("localhost:%d", p.cfg.Server.Port)
		}
		base = "http://" + host
	}
	return strings.TrimSuffix(base, "/") + "/oauth/callback"
}
