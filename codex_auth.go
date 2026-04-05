package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	codexOAuthAuthURL  = "https://auth.openai.com/oauth/authorize"
	codexOAuthTokenURL = "https://auth.openai.com/oauth/token"
	codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexRedirectURI   = "http://localhost:1455/auth/callback"
)

type codexPendingLogin struct {
	ProviderName string
	CodeVerifier string
	RedirectURI  string
	RequestedAt  time.Time
}

type codexOAuthStateStore struct {
	mu     sync.Mutex
	states map[string]codexPendingLogin
}

type codexTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

type codexJWTClaims struct {
	Email string `json:"email"`
	Auth  struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

func newCodexOAuthStateStore() *codexOAuthStateStore {
	return &codexOAuthStateStore{states: make(map[string]codexPendingLogin)}
}

func (s *codexOAuthStateStore) put(state string, login codexPendingLogin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state] = login
}

func (s *codexOAuthStateStore) take(state string) (codexPendingLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	login, ok := s.states[state]
	if ok {
		delete(s.states, state)
	}
	return login, ok
}

func buildCodexLoginURL() (string, string, string, error) {
	state, err := generateRandomURLSafe(24)
	if err != nil {
		return "", "", "", err
	}
	verifier, challenge, err := generatePKCECodes()
	if err != nil {
		return "", "", "", err
	}
	values := url.Values{
		"client_id":                  {codexOAuthClientID},
		"response_type":              {"code"},
		"redirect_uri":               {codexRedirectURI},
		"scope":                      {"openid profile email offline_access api.connectors.read api.connectors.invoke"},
		"state":                      {state},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"codex_cli_rs"},
	}
	return fmt.Sprintf("%s?%s", codexOAuthAuthURL, values.Encode()), verifier, state, nil
}

func exchangeCodexCode(ctx context.Context, client *http.Client, code, verifier string) (*codexAccount, error) {
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexOAuthClientID},
		"code":          {code},
		"redirect_uri":  {codexRedirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex token exchange failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	var token codexTokenResponse
	if err := json.Unmarshal(respBody, &token); err != nil {
		return nil, err
	}
	email, accountID := parseCodexIDToken(token.IDToken)
	now := time.Now().UTC()
	return &codexAccount{
		ID:           buildCodexAccountID(email, accountID),
		Email:        email,
		AccountID:    accountID,
		IDToken:      token.IDToken,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    now.Add(time.Duration(token.ExpiresIn) * time.Second),
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

func refreshCodexAccount(ctx context.Context, client *http.Client, account *codexAccount) (*codexAccount, error) {
	values := url.Values{
		"client_id":     {codexOAuthClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {account.RefreshToken},
		"scope":         {"openid profile email"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex token refresh failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	var token codexTokenResponse
	if err := json.Unmarshal(respBody, &token); err != nil {
		return nil, err
	}
	email, accountID := parseCodexIDToken(token.IDToken)
	updated := *account
	updated.IDToken = token.IDToken
	updated.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		updated.RefreshToken = token.RefreshToken
	}
	if email != "" {
		updated.Email = email
	}
	if accountID != "" {
		updated.AccountID = accountID
	}
	updated.ExpiresAt = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second)
	updated.UpdatedAt = time.Now().UTC()
	return &updated, nil
}

func parseCodexIDToken(idToken string) (string, string) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims codexJWTClaims
	if err := json.Unmarshal(data, &claims); err != nil {
		return "", ""
	}
	return strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Auth.AccountID)
}

func buildCodexAccountID(email, accountID string) string {
	seed := strings.TrimSpace(strings.ToLower(email))
	if seed == "" {
		seed = strings.TrimSpace(accountID)
	}
	if seed == "" {
		seed = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return buildStableAccountID(seed)
}
