package main

import (
	"bytes"
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

type kiroRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kiroRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBuildKiroGenerateRequest(t *testing.T) {
	raw := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": "system rules",
		"tools": [
			{
				"name": "Read",
				"description": "read file",
				"input_schema": {
					"type": "object",
					"properties": {
						"path": {"type": "string"}
					}
				}
			}
		],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":[{"type":"text","text":"world"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"ok"},{"type":"text","text":"continue"}]}
		]
	}`)

	got, inputTokens, err := buildKiroGenerateRequest(raw, "claude-sonnet-4-5", kiroCredentials{AuthMethod: "social", ProfileARN: "arn:aws:test"})
	if err != nil {
		t.Fatalf("buildKiroGenerateRequest error: %v", err)
	}
	if inputTokens <= 0 {
		t.Fatalf("expected positive input tokens, got %d", inputTokens)
	}

	root := gjson.ParseBytes(got)
	if model := root.Get("conversationState.currentMessage.userInputMessage.modelId").String(); model != "claude-sonnet-4.5" {
		t.Fatalf("expected mapped model, got %q", model)
	}
	if !strings.Contains(root.Get("conversationState.history.0.userInputMessage.content").String(), "system rules") {
		t.Fatalf("expected system prompt merged into first history item, got %s", root.Get("conversationState.history.0.userInputMessage.content").Raw)
	}
	if toolName := root.Get("conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.0.toolSpecification.name").String(); toolName != "Read" {
		t.Fatalf("expected tool name Read, got %q", toolName)
	}
	if profileArn := root.Get("profileArn").String(); profileArn != "arn:aws:test" {
		t.Fatalf("expected profileArn propagated, got %q", profileArn)
	}
	if toolResultID := root.Get("conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults.0.toolUseId").String(); toolResultID != "tool_1" {
		t.Fatalf("expected tool result propagated, got %q", toolResultID)
	}
}

func TestBuildKiroGenerateRequestSingleUserMessage(t *testing.T) {
	raw := []byte(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 1024,
		"messages": [
			{"role":"user","content":"hello"}
		]
	}`)

	got, _, err := buildKiroGenerateRequest(raw, "claude-sonnet-4-5", kiroCredentials{})
	if err != nil {
		t.Fatalf("buildKiroGenerateRequest error: %v", err)
	}

	root := gjson.ParseBytes(got)
	content := root.Get("conversationState.currentMessage.userInputMessage.content").String()
	if !strings.Contains(content, "hello") {
		t.Fatalf("expected current message to contain user content, got %q", content)
	}
}

func TestBuildClaudeResponseFromKiro(t *testing.T) {
	raw := []byte(`xxxxx{"content":"before <thinking>reasoning</thinking>\n\nafter"}yyyy{"name":"Read","toolUseId":"tool_1","input":"{\"path\":\"a"}zz{"input":"\"}"}kk{"stop":true}`)

	got, err := buildClaudeResponseFromKiro("claude-sonnet-4-5", 12, raw)
	if err != nil {
		t.Fatalf("buildClaudeResponseFromKiro error: %v", err)
	}
	root := gjson.ParseBytes(got)
	if stop := root.Get("stop_reason").String(); stop != "tool_use" {
		t.Fatalf("expected tool_use stop_reason, got %q", stop)
	}
	if root.Get("usage.input_tokens").Int() != 12 {
		t.Fatalf("expected input tokens 12, got %d", root.Get("usage.input_tokens").Int())
	}
	if root.Get("content.0.type").String() != "thinking" && root.Get("content.1.type").String() != "thinking" {
		t.Fatalf("expected thinking block in content, got %s", root.Get("content").Raw)
	}
	if name := root.Get("content.#(type==\"tool_use\").name").String(); name != "Read" {
		t.Fatalf("expected tool_use Read, got %q", name)
	}
	if path := root.Get("content.#(type==\"tool_use\").input.path").String(); path != "a" {
		t.Fatalf("expected parsed tool args path=a, got %q", path)
	}
}

func TestHandleClaudeCountTokens(t *testing.T) {
	p := &Proxy{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"system":"abc","messages":[{"role":"user","content":"hello world"}]}`))
	w := httptest.NewRecorder()

	p.handleClaudeCountTokens(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["input_tokens"] <= 0 {
		t.Fatalf("expected positive input_tokens, got %v", body)
	}
}

func TestHandleKiroLoginStart(t *testing.T) {
	p := NewProxy(&Config{
		Server: ServerConfig{Port: 8080},
		Providers: []ProviderConfig{
			{
				Name:    "kiro-oauth",
				Type:    "kiro",
				AuthDir: "/tmp/kiro-auth",
				Models:  []ModelConfig{{Name: "claude-sonnet-4-5"}},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/auth/kiro/login", strings.NewReader(`{"provider":"kiro-oauth","method":"google"}`))
	req.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()

	p.handleKiroLoginStart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"auth_url":"https://app.kiro.dev/signin?`) {
		t.Fatalf("expected auth_url in response, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"redirect_uri":"http://127.0.0.1:8080/oauth/callback"`) {
		t.Fatalf("expected callback redirect_uri, got %s", w.Body.String())
	}
}

func TestHandleKiroLoginCallback(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Server: ServerConfig{Port: 8080},
		Providers: []ProviderConfig{
			{
				Name:    "kiro-oauth",
				Type:    "kiro",
				AuthDir: dir,
				Models:  []ModelConfig{{Name: "claude-sonnet-4-5"}},
			},
		},
	}
	p := NewProxy(cfg)
	p.providerClients["kiro-oauth"] = &http.Client{
		Transport: kiroRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{"accessToken":"access","refreshToken":"refresh","profileArn":"arn:aws:test","expiresIn":3600}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(body)),
			}, nil
		}),
	}
	p.kiroOAuthStates.put("state-1", kiroPendingLogin{
		ProviderName: "kiro-oauth",
		CodeVerifier: "verifier",
		RedirectURI:  "http://127.0.0.1:8080/oauth/callback",
		Method:       "google",
	})

	req := httptest.NewRequest(http.MethodGet, "/oauth/callback?state=state-1&code=abc", nil)
	w := httptest.NewRecorder()
	p.handleKiroLoginCallback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	tokenPath := filepath.Join(dir, kiroAuthTokenFile)
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read saved token: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("unmarshal saved token: %v", err)
	}
	if saved["accessToken"] != "access" {
		t.Fatalf("expected saved accessToken, got %v", saved["accessToken"])
	}
	if saved["refreshToken"] != "refresh" {
		t.Fatalf("expected saved refreshToken, got %v", saved["refreshToken"])
	}
}

func TestPollKiroDeviceToken(t *testing.T) {
	oldClient := http.DefaultClient
	defer func() { http.DefaultClient = oldClient }()

	attempts := 0
	http.DefaultClient = &http.Client{
		Transport: kiroRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			body := `{"error":"authorization_pending"}`
			if attempts >= 2 {
				body = `{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(body)),
			}, nil
		}),
	}

	got, err := pollKiroDeviceToken(t.Context(), "us-east-1", "cid", "secret", "device-code", 1, 2)
	if err != nil {
		t.Fatalf("pollKiroDeviceToken error: %v", err)
	}
	if got.AccessToken != "access" {
		t.Fatalf("expected access token, got %q", got.AccessToken)
	}
	if got.RefreshToken != "refresh" {
		t.Fatalf("expected refresh token, got %q", got.RefreshToken)
	}
	if got.ClientID != "cid" || got.ClientSecret != "secret" {
		t.Fatalf("expected client credentials propagated, got %#v", got)
	}
	if got.IdentityPoolID != kiroDefaultIdentityPool {
		t.Fatalf("expected default identity pool id, got %q", got.IdentityPoolID)
	}
}

func TestFetchKiroCognitoCredentials(t *testing.T) {
	client := &http.Client{
		Transport: kiroRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://cognito-identity.us-east-1.amazonaws.com/" {
				t.Fatalf("unexpected url %s", req.URL.String())
			}
			if req.Header.Get("X-Amz-Target") != "AWSCognitoIdentityService.GetCredentialsForIdentity" {
				t.Fatalf("unexpected target header %q", req.Header.Get("X-Amz-Target"))
			}
			body := `{"IdentityId":"us-east-1:622b0cc5-1444-c67a-238f-abb1a861974b","Credentials":{"AccessKeyId":"AKIA_TEST","SecretKey":"SECRET_TEST","SessionToken":"SESSION_TEST","Expiration":1777777777}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(body)),
			}, nil
		}),
	}

	got, err := fetchKiroCognitoCredentials(t.Context(), client, "us-east-1", "us-east-1:622b0cc5-1444-c67a-238f-abb1a861974b")
	if err != nil {
		t.Fatalf("fetchKiroCognitoCredentials error: %v", err)
	}
	if got.AccessKeyID != "AKIA_TEST" || got.SecretAccessKey != "SECRET_TEST" || got.SessionToken != "SESSION_TEST" {
		t.Fatalf("unexpected cognito credentials: %#v", got)
	}
	if got.IdentityID != "us-east-1:622b0cc5-1444-c67a-238f-abb1a861974b" {
		t.Fatalf("unexpected identity id %q", got.IdentityID)
	}
	if got.CredentialExpiration == "" {
		t.Fatalf("expected credential expiration, got empty")
	}
}

func TestMergeKiroCredentialsIncludesIdentityFields(t *testing.T) {
	dst := kiroCredentials{
		AccessToken: "access",
	}
	src := kiroCredentials{
		IdentityID:           "us-east-1:test",
		IdentityPoolID:       kiroDefaultIdentityPool,
		AccessKeyID:          "AKIA_TEST",
		SecretAccessKey:      "SECRET_TEST",
		SessionToken:         "SESSION_TEST",
		CredentialExpiration: "2026-04-05T00:00:00Z",
	}

	mergeKiroCredentials(&dst, src)

	if dst.IdentityID != src.IdentityID || dst.AccessKeyID != src.AccessKeyID || dst.SecretAccessKey != src.SecretAccessKey || dst.SessionToken != src.SessionToken {
		t.Fatalf("identity credential fields not merged: %#v", dst)
	}
}

func TestMapKiroCLIKey(t *testing.T) {
	if got := mapKiroCLIKey("social"); got != "kirocli:social:token" {
		t.Fatalf("unexpected social key %q", got)
	}
	if got := mapKiroCLIKey("builder-id"); got != "kirocli:odic:token" {
		t.Fatalf("unexpected builder key %q", got)
	}
	if got := mapKiroCLIKey("external-idp"); got != "kirocli:external-idp:token" {
		t.Fatalf("unexpected external key %q", got)
	}
}

func TestConvertKiroCLITokenSocial(t *testing.T) {
	raw := []byte(`{
		"access_token":"access",
		"refresh_token":"refresh",
		"profile_arn":"arn:aws:codewhisperer:us-east-1:123:profile/ABC",
		"expires_at":"2026-04-05T14:51:39.948155Z",
		"provider":"google"
	}`)
	got, err := convertKiroCLIToken("kirocli:social:token", raw)
	if err != nil {
		t.Fatalf("convertKiroCLIToken social error: %v", err)
	}
	if got.AccessToken != "access" || got.RefreshToken != "refresh" || got.ProfileARN == "" || got.AuthMethod != "social" {
		t.Fatalf("unexpected converted social creds: %#v", got)
	}
}

func TestConvertKiroCLITokenBuilderID(t *testing.T) {
	raw := []byte(`{
		"access_token":"access",
		"refresh_token":"refresh",
		"client_id":"cid",
		"client_secret":"secret",
		"expires_at":"2026-04-05T14:51:39.948155Z",
		"region":"us-east-1"
	}`)
	got, err := convertKiroCLIToken("kirocli:odic:token", raw)
	if err != nil {
		t.Fatalf("convertKiroCLIToken builder-id error: %v", err)
	}
	if got.ClientID != "cid" || got.ClientSecret != "secret" || got.AuthMethod != "builder-id" {
		t.Fatalf("unexpected converted builder creds: %#v", got)
	}
}
