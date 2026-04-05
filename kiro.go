package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	kiroRefreshURL          = "https://prod.{{region}}.auth.desktop.kiro.dev/refreshToken"
	kiroRefreshIDCURL       = "https://oidc.{{region}}.amazonaws.com/token"
	kiroBaseURL             = "https://q.{{region}}.amazonaws.com/generateAssistantResponse"
	kiroVersion             = "0.11.63"
	kiroDefaultRegion       = "us-east-1"
	kiroDefaultIdentityPool = "us-east-1:820fd6d1-95c0-4ca4-bffb-3f01d32da842"
	kiroAuthTokenFile       = "kiro-auth-token.json"
	kiroOrigin              = "AI_EDITOR"
)

var (
	kiroModelMapping = map[string]string{
		"claude-haiku-4-5":           "claude-haiku-4.5",
		"claude-opus-4-6":            "claude-opus-4.6",
		"claude-sonnet-4-6":          "claude-sonnet-4.6",
		"claude-opus-4-5":            "claude-opus-4.5",
		"claude-opus-4-5-20251101":   "claude-opus-4.5",
		"claude-sonnet-4-5":          "claude-sonnet-4.5",
		"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	}
)

type kiroCredentials struct {
	AccessToken          string `json:"accessToken"`
	RefreshToken         string `json:"refreshToken"`
	ClientID             string `json:"clientId"`
	ClientSecret         string `json:"clientSecret"`
	ProfileARN           string `json:"profileArn"`
	Region               string `json:"region"`
	IDCRegion            string `json:"idcRegion"`
	AuthMethod           string `json:"authMethod"`
	ExpiresAt            string `json:"expiresAt"`
	UUID                 string `json:"uuid"`
	IdentityID           string `json:"identityId"`
	IdentityPoolID       string `json:"identityPoolId"`
	AccessKeyID          string `json:"accessKeyId"`
	SecretAccessKey      string `json:"secretAccessKey"`
	SessionToken         string `json:"sessionToken"`
	CredentialExpiration string `json:"credentialExpiration"`
}

type kiroRuntime struct {
	creds         kiroCredentials
	tokenFilePath string
	refreshURL    string
	refreshIDCURL string
	baseURL       string
	machineID     string
}

type kiroUserInputMessage struct {
	Content                 string                       `json:"content"`
	ModelID                 string                       `json:"modelId"`
	Origin                  string                       `json:"origin"`
	Images                  []map[string]any             `json:"images,omitempty"`
	UserInputMessageContext *kiroUserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type kiroUserInputMessageContext struct {
	ToolResults []map[string]any `json:"toolResults,omitempty"`
	Tools       []map[string]any `json:"tools,omitempty"`
}

type kiroAssistantResponseMessage struct {
	Content  string           `json:"content"`
	ToolUses []map[string]any `json:"toolUses,omitempty"`
}

type kiroConversationState struct {
	AgentTaskType   string           `json:"agentTaskType"`
	ChatTriggerType string           `json:"chatTriggerType"`
	ConversationID  string           `json:"conversationId"`
	History         []map[string]any `json:"history,omitempty"`
	CurrentMessage  map[string]any   `json:"currentMessage"`
}

type kiroGenerateRequest struct {
	ConversationState kiroConversationState `json:"conversationState"`
	ProfileARN        string                `json:"profileArn,omitempty"`
}

type kiroStreamEvent struct {
	Type                   string
	Content                string
	ToolName               string
	ToolUseID              string
	ToolInput              string
	ToolStop               bool
	ContextUsagePercentage int64
}

type kiroToolCall struct {
	ID   string
	Name string
	Args string
}

type kiroContentAccumulator struct {
	buffer                                string
	pendingTextBeforeThinking             string
	inThinking                            bool
	thinkingExtracted                     bool
	stripThinkingLeadingNewline           bool
	stripTextLeadingNewlinesAfterThinking bool
}

type kiroClaudeStreamState struct {
	msgID            string
	model            string
	started          bool
	nextBlockIdx     int
	textBlockIdx     int
	thinkingBlockIdx int
	toolBlockIdx     map[string]int
	currentToolID    string
	currentTool      *kiroToolCall
	stopped          map[int]bool
	accumulator      kiroContentAccumulator
	hasTool          bool
	outputTokens     int
	inputTokens      int
}

func isKiroProviderType(providerType string) bool {
	t := strings.ToLower(strings.TrimSpace(providerType))
	return t == "kiro" || t == "claude-kiro-oauth" || strings.Contains(t, "kiro")
}

func (p *Proxy) kiroRuntimeForProvider(ctx context.Context, prov *ProviderConfig) (*kiroRuntime, error) {
	if prov == nil {
		return nil, fmt.Errorf("kiro provider missing")
	}
	creds, tokenFilePath, err := loadKiroCredentials(prov.AuthDir)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(creds.Region) == "" {
		creds.Region = kiroDefaultRegion
	}
	if strings.TrimSpace(creds.IDCRegion) == "" {
		creds.IDCRegion = creds.Region
	}
	rt := &kiroRuntime{
		creds:         creds,
		tokenFilePath: tokenFilePath,
		refreshURL:    strings.ReplaceAll(kiroRefreshURL, "{{region}}", creds.Region),
		refreshIDCURL: strings.ReplaceAll(kiroRefreshIDCURL, "{{region}}", creds.IDCRegion),
		baseURL:       strings.ReplaceAll(kiroBaseURL, "{{region}}", creds.Region),
		machineID:     buildStableAccountID(firstNonEmpty(creds.UUID, creds.ProfileARN, creds.ClientID, "KIRO_DEFAULT_MACHINE")),
	}
	if strings.TrimSpace(prov.BaseURL) != "" {
		rt.baseURL = strings.TrimSpace(prov.BaseURL)
	}
	if creds.AccessToken == "" || kiroTokenExpired(creds.ExpiresAt) {
		if err := p.refreshKiroToken(ctx, prov, rt); err != nil {
			return nil, err
		}
	}
	return rt, nil
}

func loadKiroCredentials(authDir string) (kiroCredentials, string, error) {
	var merged kiroCredentials
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return merged, "", fmt.Errorf("kiro auth-dir is required")
	}
	targetPath := authDir
	if info, err := os.Stat(authDir); err == nil && info.IsDir() {
		targetPath = filepath.Join(authDir, kiroAuthTokenFile)
	}
	loadAndMerge := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var c kiroCredentials
		if err := json.Unmarshal(data, &c); err == nil {
			mergeKiroCredentials(&merged, c)
			return
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(repairKiroJSON(string(data))), &m); err == nil {
			buf, _ := json.Marshal(m)
			_ = json.Unmarshal(buf, &c)
			mergeKiroCredentials(&merged, c)
		}
	}
	loadAndMerge(targetPath)
	dir := filepath.Dir(targetPath)
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || filepath.Join(dir, entry.Name()) == targetPath {
				continue
			}
			loadAndMerge(filepath.Join(dir, entry.Name()))
		}
	}
	if merged.AccessToken == "" && merged.RefreshToken == "" {
		return merged, targetPath, fmt.Errorf("no kiro credentials found in %s", authDir)
	}
	return merged, targetPath, nil
}

func mergeKiroCredentials(dst *kiroCredentials, src kiroCredentials) {
	if strings.TrimSpace(src.AccessToken) != "" {
		dst.AccessToken = src.AccessToken
	}
	if strings.TrimSpace(src.RefreshToken) != "" {
		dst.RefreshToken = src.RefreshToken
	}
	if strings.TrimSpace(src.ClientID) != "" {
		dst.ClientID = src.ClientID
	}
	if strings.TrimSpace(src.ClientSecret) != "" {
		dst.ClientSecret = src.ClientSecret
	}
	if strings.TrimSpace(src.ProfileARN) != "" {
		dst.ProfileARN = src.ProfileARN
	}
	if strings.TrimSpace(src.Region) != "" {
		dst.Region = src.Region
	}
	if strings.TrimSpace(src.IDCRegion) != "" {
		dst.IDCRegion = src.IDCRegion
	}
	if strings.TrimSpace(src.AuthMethod) != "" {
		dst.AuthMethod = src.AuthMethod
	}
	if strings.TrimSpace(src.ExpiresAt) != "" {
		dst.ExpiresAt = src.ExpiresAt
	}
	if strings.TrimSpace(src.UUID) != "" {
		dst.UUID = src.UUID
	}
	if strings.TrimSpace(src.IdentityID) != "" {
		dst.IdentityID = src.IdentityID
	}
	if strings.TrimSpace(src.IdentityPoolID) != "" {
		dst.IdentityPoolID = src.IdentityPoolID
	}
	if strings.TrimSpace(src.AccessKeyID) != "" {
		dst.AccessKeyID = src.AccessKeyID
	}
	if strings.TrimSpace(src.SecretAccessKey) != "" {
		dst.SecretAccessKey = src.SecretAccessKey
	}
	if strings.TrimSpace(src.SessionToken) != "" {
		dst.SessionToken = src.SessionToken
	}
	if strings.TrimSpace(src.CredentialExpiration) != "" {
		dst.CredentialExpiration = src.CredentialExpiration
	}
}

func repairKiroJSON(in string) string {
	re := regexp.MustCompile(`,\s*([}\]])`)
	out := re.ReplaceAllString(in, "$1")
	re = regexp.MustCompile(`([{,]\s*)([a-zA-Z0-9_]+?)\s*:`)
	out = re.ReplaceAllString(out, `$1"$2":`)
	return out
}

func kiroTokenExpired(expiresAt string) bool {
	expiresAt = strings.TrimSpace(expiresAt)
	if expiresAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return true
	}
	return time.Until(t) <= 30*time.Second
}

func (p *Proxy) refreshKiroToken(ctx context.Context, prov *ProviderConfig, rt *kiroRuntime) error {
	if strings.TrimSpace(rt.creds.RefreshToken) == "" {
		return fmt.Errorf("kiro refresh token missing")
	}
	payload := map[string]any{
		"refreshToken": rt.creds.RefreshToken,
	}
	refreshURL := rt.refreshURL
	if !strings.EqualFold(strings.TrimSpace(rt.creds.AuthMethod), "social") {
		refreshURL = rt.refreshIDCURL
		payload["clientId"] = rt.creds.ClientID
		payload["clientSecret"] = rt.creds.ClientSecret
		payload["grantType"] = "refresh_token"
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClientForProvider(prov).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kiro token refresh failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	var token struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(respBody, &token); err != nil {
		return err
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return fmt.Errorf("kiro token refresh response missing accessToken")
	}
	rt.creds.AccessToken = token.AccessToken
	if strings.TrimSpace(token.RefreshToken) != "" {
		rt.creds.RefreshToken = token.RefreshToken
	}
	if strings.TrimSpace(token.ProfileARN) != "" {
		rt.creds.ProfileARN = token.ProfileARN
	}
	if token.ExpiresIn > 0 {
		rt.creds.ExpiresAt = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	if rt.tokenFilePath != "" {
		_ = os.MkdirAll(filepath.Dir(rt.tokenFilePath), 0o755)
		saved, _ := json.MarshalIndent(rt.creds, "", "  ")
		_ = os.WriteFile(rt.tokenFilePath, saved, 0o600)
	}
	return nil
}

func (p *Proxy) handleClaudeViaKiro(reqID string, w http.ResponseWriter, r *http.Request, route routeEntry, body []byte, stream bool, startedAt time.Time) {
	ctx, cancel := contextWithTimeout(r, defaultUpstreamTimeout)
	defer cancel()

	rt, err := p.kiroRuntimeForProvider(ctx, route.provider)
	if err != nil {
		writeClaudeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	translateStartedAt := time.Now()
	upstreamBody, inputTokens, err := buildKiroGenerateRequest(body, route.model, rt.creds)
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	log.Printf("[%s] translated claude->kiro provider=%s upstream_model=%s req_bytes=%d took=%s", reqID, route.provider.Name, route.model, len(upstreamBody), sinceMS(translateStartedAt))
	logPayloadSummary(reqID, "kiro_request", upstreamBody)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.baseURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeClaudeError(w, http.StatusInternalServerError, "api_error", "create request failed")
		return
	}
	setKiroHeaders(httpReq, rt, true)

	upstreamStartedAt := time.Now()
	resp, err := p.httpClientForProvider(route.provider).Do(httpReq)
	if err != nil {
		writeClaudeError(w, http.StatusBadGateway, "api_error", explainUpstreamError(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
		writeClaudeError(w, resp.StatusCode, "api_error", string(respBody))
		return
	}

	if stream {
		p.streamKiroToClaude(reqID, w, resp.Body, route.model, inputTokens, startedAt, upstreamStartedAt)
		return
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
	claudeResp, err := buildClaudeResponseFromKiro(route.model, inputTokens, respBody)
	if err != nil {
		writeClaudeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(claudeResp)
}

func setKiroHeaders(req *http.Request, rt *kiroRuntime, stream bool) {
	platform := runtime.GOOS
	if platform == "" {
		platform = "linux"
	}
	goVersion := strings.TrimPrefix(runtime.Version(), "go")
	req.Header.Set("Authorization", "Bearer "+rt.creds.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("amz-sdk-invocation-id", buildStableAccountID(time.Now().UTC().Format(time.RFC3339Nano)))
	req.Header.Set("amz-sdk-request", "attempt=1; max=3")
	req.Header.Set("x-amz-user-agent", fmt.Sprintf("aws-sdk-js/1.0.34 KiroIDE-%s-%s", kiroVersion, rt.machineID))
	req.Header.Set("User-Agent", fmt.Sprintf("aws-sdk-js/1.0.34 ua/2.1 os/%s lang/go md/go#%s api/codewhispererstreaming#1.0.34 m/E KiroIDE-%s-%s", platform, goVersion, kiroVersion, rt.machineID))
	req.Header.Set("Connection", "close")
	if stream {
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	}
}

func buildKiroGenerateRequest(raw []byte, model string, creds kiroCredentials) ([]byte, int, error) {
	root := gjson.ParseBytes(raw)
	msgs := root.Get("messages")
	if !msgs.Exists() || !msgs.IsArray() || len(msgs.Array()) == 0 {
		return nil, 0, fmt.Errorf("no messages found")
	}

	systemPrompt := buildKiroSystemPrompt(root.Get("system"), root.Get("thinking"))
	codewhispererModel := kiroModelMapping[model]
	if codewhispererModel == "" {
		codewhispererModel = model
	}
	toolsContext := buildKiroTools(root.Get("tools"))

	processed := msgs.Array()
	history := make([]map[string]any, 0, len(processed))
	startIndex := 0
	mergedSystemIntoFirstUser := false
	if systemPrompt != "" {
		if processed[0].Get("role").String() == "user" {
			if len(processed) > 1 {
				first := processed[0]
				combined := strings.TrimSpace(systemPrompt + "\n\n" + kiroMessageText(first))
				history = append(history, map[string]any{
					"userInputMessage": kiroUserInputMessage{
						Content: combined,
						ModelID: codewhispererModel,
						Origin:  kiroOrigin,
					},
				})
				startIndex = 1
			} else {
				mergedSystemIntoFirstUser = true
			}
		} else {
			history = append(history, map[string]any{
				"userInputMessage": kiroUserInputMessage{
					Content: systemPrompt,
					ModelID: codewhispererModel,
					Origin:  kiroOrigin,
				},
			})
		}
	}

	merged := mergeAdjacentClaudeMessages(processed[startIndex:])
	if len(merged) == 0 {
		return nil, 0, fmt.Errorf("no messages found")
	}

	for i := 0; i < len(merged)-1; i++ {
		item := merged[i]
		role := item.Get("role").String()
		switch role {
		case "user":
			history = append(history, map[string]any{
				"userInputMessage": buildKiroUserInputMessage(item, codewhispererModel, nil),
			})
		case "assistant":
			history = append(history, map[string]any{
				"assistantResponseMessage": buildKiroAssistantResponseMessage(item),
			})
		}
	}

	current := merged[len(merged)-1]
	var currentMessage map[string]any
	if current.Get("role").String() == "assistant" {
		history = append(history, map[string]any{
			"assistantResponseMessage": buildKiroAssistantResponseMessage(current),
		})
		ctx := &kiroUserInputMessageContext{}
		if len(toolsContext) > 0 {
			ctx.Tools = toolsContext
		}
		currentMessage = map[string]any{
			"userInputMessage": kiroUserInputMessage{
				Content:                 "Continue",
				ModelID:                 codewhispererModel,
				Origin:                  kiroOrigin,
				UserInputMessageContext: ctx,
			},
		}
	} else {
		if len(history) > 0 {
			if _, ok := history[len(history)-1]["assistantResponseMessage"]; !ok {
				history = append(history, map[string]any{
					"assistantResponseMessage": kiroAssistantResponseMessage{Content: "Continue"},
				})
			}
		}
		userInput := buildKiroUserInputMessage(current, codewhispererModel, toolsContext)
		if mergedSystemIntoFirstUser {
			userInput.Content = strings.TrimSpace(systemPrompt + "\n\n" + userInput.Content)
		}
		currentMessage = map[string]any{
			"userInputMessage": userInput,
		}
	}

	request := kiroGenerateRequest{
		ConversationState: kiroConversationState{
			AgentTaskType:   "vibe",
			ChatTriggerType: "MANUAL",
			ConversationID:  buildStableAccountID(time.Now().UTC().Format(time.RFC3339Nano)),
			CurrentMessage:  currentMessage,
		},
	}
	if len(history) > 0 {
		request.ConversationState.History = history
	}
	if strings.EqualFold(strings.TrimSpace(creds.AuthMethod), "social") && strings.TrimSpace(creds.ProfileARN) != "" {
		request.ProfileARN = creds.ProfileARN
	}
	out, err := json.Marshal(request)
	if err != nil {
		return nil, 0, err
	}
	return out, countClaudeInputTokens(raw), nil
}

func buildKiroSystemPrompt(system, thinking gjson.Result) string {
	parts := []string{}
	prefix := kiroThinkingPrefix(thinking)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	sysText := kiroContentToPlainText(system)
	if strings.TrimSpace(sysText) != "" {
		parts = append(parts, sysText)
	}
	return strings.Join(parts, "\n")
}

func kiroThinkingPrefix(thinking gjson.Result) string {
	if !thinking.Exists() || !thinking.IsObject() {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Get("type").String())) {
	case "enabled":
		budget := thinking.Get("budget_tokens").Int()
		if budget < 1024 {
			budget = 1024
		}
		if budget > 24576 {
			budget = 24576
		}
		return fmt.Sprintf("<thinking_mode>enabled</thinking_mode><max_thinking_length>%d</max_thinking_length>", budget)
	case "adaptive":
		effort := strings.ToLower(strings.TrimSpace(thinking.Get("effort").String()))
		if effort != "low" && effort != "medium" && effort != "high" {
			effort = "high"
		}
		return fmt.Sprintf("<thinking_mode>adaptive</thinking_mode><thinking_effort>%s</thinking_effort>", effort)
	default:
		return ""
	}
}

func buildKiroTools(tools gjson.Result) []map[string]any {
	var out []map[string]any
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, t gjson.Result) bool {
			name := strings.TrimSpace(t.Get("name").String())
			if name == "" {
				return true
			}
			lower := strings.ToLower(name)
			if lower == "web_search" || lower == "websearch" {
				return true
			}
			desc := strings.TrimSpace(t.Get("description").String())
			if desc == "" {
				return true
			}
			if len(desc) > 9216 {
				desc = desc[:9216] + "..."
			}
			var inputSchema any = map[string]any{}
			if raw := strings.TrimSpace(t.Get("input_schema").Raw); raw != "" && gjson.Valid(raw) {
				_ = json.Unmarshal([]byte(raw), &inputSchema)
			}
			out = append(out, map[string]any{
				"toolSpecification": map[string]any{
					"name":        name,
					"description": desc,
					"inputSchema": map[string]any{
						"json": inputSchema,
					},
				},
			})
			return true
		})
	}
	if len(out) == 0 {
		out = append(out, map[string]any{
			"toolSpecification": map[string]any{
				"name":        "no_tool_available",
				"description": "This is a placeholder tool when no other tools are available. It does nothing.",
				"inputSchema": map[string]any{
					"json": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
			},
		})
	}
	return out
}

func mergeAdjacentClaudeMessages(messages []gjson.Result) []gjson.Result {
	if len(messages) == 0 {
		return nil
	}
	out := []gjson.Result{messages[0]}
	for i := 1; i < len(messages); i++ {
		prev := out[len(out)-1]
		cur := messages[i]
		if prev.Get("role").String() != cur.Get("role").String() {
			out = append(out, cur)
			continue
		}
		merged := prev.Raw
		prevContent := prev.Get("content")
		curContent := cur.Get("content")
		switch {
		case prevContent.IsArray() && curContent.IsArray():
			arr := prevContent.Raw
			curContent.ForEach(func(_, item gjson.Result) bool {
				arr, _ = sjson.SetRaw(arr, "-1", item.Raw)
				return true
			})
			merged, _ = sjson.SetRaw(merged, "content", arr)
		case prevContent.Type == gjson.String && curContent.Type == gjson.String:
			merged, _ = sjson.Set(merged, "content", prevContent.String()+"\n"+curContent.String())
		case prevContent.IsArray() && curContent.Type == gjson.String:
			arr := prevContent.Raw
			item := fmt.Sprintf(`{"type":"text","text":%q}`, curContent.String())
			arr, _ = sjson.SetRaw(arr, "-1", item)
			merged, _ = sjson.SetRaw(merged, "content", arr)
		case prevContent.Type == gjson.String && curContent.IsArray():
			arr := fmt.Sprintf(`[{"type":"text","text":%q}]`, prevContent.String())
			curContent.ForEach(func(_, item gjson.Result) bool {
				arr, _ = sjson.SetRaw(arr, "-1", item.Raw)
				return true
			})
			merged, _ = sjson.SetRaw(merged, "content", arr)
		default:
			out = append(out, cur)
			continue
		}
		out[len(out)-1] = gjson.Parse(merged)
	}
	return out
}

func buildKiroUserInputMessage(msg gjson.Result, model string, tools []map[string]any) kiroUserInputMessage {
	out := kiroUserInputMessage{
		Content: kiroMessageText(msg),
		ModelID: model,
		Origin:  kiroOrigin,
	}
	var images []map[string]any
	var toolResults []map[string]any
	content := msg.Get("content")
	if content.Exists() && content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "tool_result":
				toolResults = append(toolResults, map[string]any{
					"content":   []map[string]string{{"text": toolResultContentToString(part.Get("content"))}},
					"status":    "success",
					"toolUseId": part.Get("tool_use_id").String(),
				})
			case "image":
				if img := kiroImagePayload(part); img != nil {
					images = append(images, img)
				}
			}
			return true
		})
	}
	if len(images) > 0 {
		out.Images = images
	}
	ctx := &kiroUserInputMessageContext{}
	if len(toolResults) > 0 {
		ctx.ToolResults = dedupeKiroToolResults(toolResults)
	}
	if len(tools) > 0 {
		ctx.Tools = tools
	}
	if len(ctx.ToolResults) > 0 || len(ctx.Tools) > 0 {
		out.UserInputMessageContext = ctx
	}
	if strings.TrimSpace(out.Content) == "" {
		if len(toolResults) > 0 {
			out.Content = "Tool results provided."
		} else {
			out.Content = "Continue"
		}
	}
	return out
}

func dedupeKiroToolResults(results []map[string]any) []map[string]any {
	seen := map[string]bool{}
	out := make([]map[string]any, 0, len(results))
	for _, item := range results {
		id, _ := item["toolUseId"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, item)
	}
	return out
}

func buildKiroAssistantResponseMessage(msg gjson.Result) kiroAssistantResponseMessage {
	out := kiroAssistantResponseMessage{Content: kiroMessageText(msg)}
	var thinking strings.Builder
	var toolUses []map[string]any
	content := msg.Get("content")
	if content.Exists() && content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "thinking":
				thinking.WriteString(part.Get("thinking").String())
			case "redacted_thinking":
				thinking.WriteString(extractClaudeThinkingText(part))
			case "tool_use":
				var input any = map[string]any{}
				if raw := part.Get("input").Raw; raw != "" && gjson.Valid(raw) {
					_ = json.Unmarshal([]byte(raw), &input)
				}
				toolUses = append(toolUses, map[string]any{
					"input":     input,
					"name":      part.Get("name").String(),
					"toolUseId": part.Get("id").String(),
				})
			}
			return true
		})
	}
	if thinking.Len() > 0 {
		if out.Content != "" {
			out.Content = "<thinking>" + thinking.String() + "</thinking>\n\n" + out.Content
		} else {
			out.Content = "<thinking>" + thinking.String() + "</thinking>"
		}
	}
	if len(toolUses) > 0 {
		out.ToolUses = toolUses
	}
	return out
}

func kiroImagePayload(part gjson.Result) map[string]any {
	src := part.Get("source")
	if !src.Exists() {
		return nil
	}
	switch src.Get("type").String() {
	case "base64":
		mediaType := src.Get("media_type").String()
		format := mediaType
		if idx := strings.Index(format, "/"); idx >= 0 {
			format = format[idx+1:]
		}
		if format == "" {
			format = "png"
		}
		return map[string]any{
			"format": format,
			"source": map[string]any{
				"bytes": src.Get("data").String(),
			},
		}
	default:
		return nil
	}
}

func kiroMessageText(msg gjson.Result) string {
	return kiroContentToPlainText(msg.Get("content"))
}

func kiroContentToPlainText(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var parts []string
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text":
			txt := part.Get("text").String()
			if txt != "" {
				parts = append(parts, txt)
			}
		case "thinking":
			txt := part.Get("thinking").String()
			if txt != "" {
				parts = append(parts, txt)
			}
		case "redacted_thinking":
			parts = append(parts, extractClaudeThinkingText(part))
		}
		return true
	})
	return strings.Join(parts, "")
}

func buildClaudeResponseFromKiro(model string, inputTokens int, raw []byte) ([]byte, error) {
	text, toolCalls := parseKiroResponse(raw)
	messageID := "msg_" + buildStableAccountID(time.Now().UTC().Format(time.RFC3339Nano))
	resp := fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","model":%q,"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d},"content":[]}`,
		messageID, model, inputTokens, countTextTokensApprox(text))
	if blocks := kiroTextToClaudeBlocks(text); len(blocks) > 0 {
		for _, block := range blocks {
			resp, _ = sjson.SetRaw(resp, "content.-1", block)
		}
	}
	if len(toolCalls) > 0 {
		resp, _ = sjson.Set(resp, "stop_reason", "tool_use")
		for _, call := range toolCalls {
			block := fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, call.ID, call.Name)
			if parsed, ok := parseToolArgumentsObject(call.Args); ok {
				block, _ = sjson.SetRaw(block, "input", parsed)
			} else if strings.TrimSpace(call.Args) != "" {
				block, _ = sjson.Set(block, "input.raw_arguments", call.Args)
			}
			resp, _ = sjson.SetRaw(resp, "content.-1", block)
		}
	}
	return []byte(resp), nil
}

func parseKiroResponse(raw []byte) (string, []kiroToolCall) {
	buffer := string(raw)
	var out strings.Builder
	var toolCalls []kiroToolCall
	var current *kiroToolCall
	for len(buffer) > 0 {
		events, rest := parseKiroEventBuffer(buffer)
		if len(events) == 0 && rest == buffer {
			break
		}
		buffer = rest
		for _, event := range events {
			switch event.Type {
			case "content":
				out.WriteString(event.Content)
			case "toolUse":
				if current != nil && current.ID != event.ToolUseID {
					toolCalls = append(toolCalls, *current)
				}
				if current == nil || current.ID != event.ToolUseID {
					current = &kiroToolCall{ID: event.ToolUseID, Name: event.ToolName}
				}
				current.Args += event.ToolInput
			case "toolUseInput":
				if current != nil {
					current.Args += event.ToolInput
				}
			case "toolUseStop":
				if current != nil {
					toolCalls = append(toolCalls, *current)
					current = nil
				}
			}
		}
	}
	if current != nil {
		toolCalls = append(toolCalls, *current)
	}
	toolCalls = dedupeKiroToolCalls(toolCalls)
	text := cleanupKiroToolCallText(out.String(), toolCalls)
	return text, toolCalls
}

func parseKiroEventBuffer(buffer string) ([]kiroStreamEvent, string) {
	var events []kiroStreamEvent
	searchStart := 0
	for {
		candidates := []int{
			strings.Index(buffer[searchStart:], `{"content":`),
			strings.Index(buffer[searchStart:], `{"name":`),
			strings.Index(buffer[searchStart:], `{"followupPrompt":`),
			strings.Index(buffer[searchStart:], `{"input":`),
			strings.Index(buffer[searchStart:], `{"stop":`),
			strings.Index(buffer[searchStart:], `{"contextUsagePercentage":`),
		}
		best := -1
		for _, c := range candidates {
			if c >= 0 && (best == -1 || c < best) {
				best = c
			}
		}
		if best == -1 {
			if searchStart > 0 {
				return events, buffer[searchStart:]
			}
			return events, buffer
		}
		start := searchStart + best
		end := findMatchingJSONObjectEnd(buffer, start)
		if end < 0 {
			return events, buffer[start:]
		}
		payload := buffer[start : end+1]
		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err == nil {
			switch {
			case data["content"] != nil && data["followupPrompt"] == nil:
				events = append(events, kiroStreamEvent{Type: "content", Content: fmt.Sprint(data["content"])})
			case data["name"] != nil && data["toolUseId"] != nil:
				events = append(events, kiroStreamEvent{
					Type:      "toolUse",
					ToolName:  fmt.Sprint(data["name"]),
					ToolUseID: fmt.Sprint(data["toolUseId"]),
					ToolInput: anyToString(data["input"]),
					ToolStop:  anyToBool(data["stop"]),
				})
			case data["input"] != nil && data["name"] == nil:
				events = append(events, kiroStreamEvent{Type: "toolUseInput", ToolInput: anyToString(data["input"])})
			case data["stop"] != nil && data["contextUsagePercentage"] == nil:
				events = append(events, kiroStreamEvent{Type: "toolUseStop", ToolStop: anyToBool(data["stop"])})
			case data["contextUsagePercentage"] != nil:
				events = append(events, kiroStreamEvent{Type: "contextUsage", ContextUsagePercentage: anyToInt64(data["contextUsagePercentage"])})
			}
		}
		searchStart = end + 1
		if searchStart >= len(buffer) {
			return events, ""
		}
	}
}

func findMatchingJSONObjectEnd(s string, start int) int {
	if start < 0 || start >= len(s) || s[start] != '{' {
		return -1
	}
	depth := 0
	inString := false
	escapeNext := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if escapeNext {
			escapeNext = false
			continue
		}
		if ch == '\\' {
			escapeNext = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func dedupeKiroToolCalls(calls []kiroToolCall) []kiroToolCall {
	seen := map[string]bool{}
	out := make([]kiroToolCall, 0, len(calls))
	for _, call := range calls {
		key := call.Name + "\x00" + call.Args
		if seen[key] {
			continue
		}
		seen[key] = true
		if strings.TrimSpace(call.ID) == "" {
			call.ID = "toolu_" + buildStableAccountID(key)
		}
		out = append(out, call)
	}
	return out
}

func cleanupKiroToolCallText(text string, calls []kiroToolCall) string {
	out := text
	for _, call := range calls {
		pat := regexp.MustCompile(`(?s)\[Called\s+` + regexp.QuoteMeta(call.Name) + `\s+with\s+args:\s*\{.*?\}\]`)
		out = pat.ReplaceAllString(out, "")
	}
	out = strings.TrimSpace(strings.Join(strings.Fields(out), " "))
	return out
}

func kiroTextToClaudeBlocks(text string) []string {
	start := strings.Index(text, "<thinking>")
	if start < 0 {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		block, _ := sjson.Set(`{"type":"text","text":""}`, "text", text)
		return []string{block}
	}
	end := strings.Index(text[start+len("<thinking>"):], "</thinking>")
	if end < 0 {
		block, _ := sjson.Set(`{"type":"text","text":""}`, "text", text)
		return []string{block}
	}
	end += start + len("<thinking>")
	var blocks []string
	before := strings.TrimSpace(text[:start])
	thinking := text[start+len("<thinking>") : end]
	after := strings.TrimSpace(text[end+len("</thinking>"):])
	if before != "" {
		block, _ := sjson.Set(`{"type":"text","text":""}`, "text", before)
		blocks = append(blocks, block)
	}
	tb, _ := sjson.Set(`{"type":"thinking","thinking":""}`, "thinking", thinking)
	blocks = append(blocks, tb)
	if after != "" {
		block, _ := sjson.Set(`{"type":"text","text":""}`, "text", after)
		blocks = append(blocks, block)
	}
	return blocks
}

func (p *Proxy) streamKiroToClaude(reqID string, w http.ResponseWriter, body io.Reader, model string, inputTokens int, requestStartedAt, upstreamStartedAt time.Time) {
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

	state := &kiroClaudeStreamState{
		msgID:            "msg_" + buildStableAccountID(time.Now().UTC().Format(time.RFC3339Nano)),
		model:            model,
		textBlockIdx:     -1,
		thinkingBlockIdx: -1,
		toolBlockIdx:     map[string]int{},
		stopped:          map[int]bool{},
		inputTokens:      inputTokens,
	}
	_, _ = w.Write([]byte(fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":%d,\"output_tokens\":0}}}\n\n", state.msgID, model, inputTokens)))
	flusher.Flush()
	state.started = true

	buf := make([]byte, 32*1024)
	var raw strings.Builder
	firstChunkLogged := false
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if !firstChunkLogged {
				firstChunkLogged = true
				log.Printf("[%s] kiro_stream_first_chunk since_upstream=%s since_request=%s chunk_bytes=%d", reqID, sinceMS(upstreamStartedAt), sinceMS(requestStartedAt), n)
			}
			raw.Write(buf[:n])
			events, remaining := parseKiroEventBuffer(raw.String())
			raw.Reset()
			raw.WriteString(remaining)
			for _, event := range events {
				for _, line := range state.consumeKiroEvent(event) {
					_, _ = w.Write([]byte(line))
					flusher.Flush()
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[%s] kiro_stream_error total=%s err=%v", reqID, sinceMS(requestStartedAt), err)
			}
			break
		}
	}
	for _, line := range state.finalizeKiroStream() {
		_, _ = w.Write([]byte(line))
		flusher.Flush()
	}
	log.Printf("[%s] kiro_stream_done total=%s", reqID, sinceMS(requestStartedAt))
}

func (s *kiroClaudeStreamState) consumeKiroEvent(event kiroStreamEvent) []string {
	switch event.Type {
	case "content":
		return s.consumeKiroContent(event.Content)
	case "toolUse":
		return s.consumeKiroToolUse(event.ToolUseID, event.ToolName, event.ToolInput, event.ToolStop)
	case "toolUseInput":
		if s.currentTool == nil {
			return nil
		}
		s.currentTool.Args += event.ToolInput
		s.outputTokens += countTextTokensApprox(event.ToolInput)
		return []string{s.contentBlockDelta(s.toolBlockIdx[s.currentTool.ID], "input_json_delta", "partial_json", event.ToolInput)}
	case "toolUseStop":
		return s.finishCurrentTool()
	default:
		return nil
	}
}

func (s *kiroClaudeStreamState) consumeKiroContent(content string) []string {
	s.outputTokens += countTextTokensApprox(content)
	acc := &s.accumulator
	acc.buffer += content
	var out []string
	for len(acc.buffer) > 0 {
		if !acc.inThinking && !acc.thinkingExtracted {
			start := strings.Index(acc.buffer, "<thinking>")
			if start >= 0 {
				before := acc.pendingTextBeforeThinking + acc.buffer[:start]
				if strings.TrimSpace(before) != "" {
					out = append(out, s.ensureTextBlockStart()...)
					out = append(out, s.contentBlockDelta(s.textBlockIdx, "text_delta", "text", before))
				}
				acc.pendingTextBeforeThinking = ""
				acc.buffer = acc.buffer[start+len("<thinking>"):]
				acc.inThinking = true
				acc.stripThinkingLeadingNewline = true
				continue
			}
			safeLen := maxInt(0, len(acc.buffer)-len("<thinking>"))
			if safeLen > 0 {
				safe := acc.buffer[:safeLen]
				if strings.TrimSpace(safe) == "" {
					acc.pendingTextBeforeThinking += safe
				} else {
					text := acc.pendingTextBeforeThinking + safe
					acc.pendingTextBeforeThinking = ""
					out = append(out, s.ensureTextBlockStart()...)
					out = append(out, s.contentBlockDelta(s.textBlockIdx, "text_delta", "text", text))
				}
				acc.buffer = acc.buffer[safeLen:]
			}
			break
		}
		if acc.inThinking {
			if acc.stripThinkingLeadingNewline {
				if strings.HasPrefix(acc.buffer, "\r\n") {
					acc.buffer = acc.buffer[2:]
				} else if strings.HasPrefix(acc.buffer, "\n") {
					acc.buffer = acc.buffer[1:]
				}
				acc.stripThinkingLeadingNewline = false
			}
			end := strings.Index(acc.buffer, "</thinking>")
			if end >= 0 {
				thinking := acc.buffer[:end]
				out = append(out, s.ensureThinkingBlockStart()...)
				if thinking != "" {
					out = append(out, s.contentBlockDelta(s.thinkingBlockIdx, "thinking_delta", "thinking", thinking))
				}
				out = append(out, s.contentBlockStop(s.thinkingBlockIdx))
				s.stopped[s.thinkingBlockIdx] = true
				s.thinkingBlockIdx = -1
				acc.buffer = acc.buffer[end+len("</thinking>"):]
				acc.inThinking = false
				acc.thinkingExtracted = true
				acc.stripTextLeadingNewlinesAfterThinking = true
				continue
			}
			safeLen := maxInt(0, len(acc.buffer)-len("</thinking>"))
			if safeLen > 0 {
				thinking := acc.buffer[:safeLen]
				out = append(out, s.ensureThinkingBlockStart()...)
				out = append(out, s.contentBlockDelta(s.thinkingBlockIdx, "thinking_delta", "thinking", thinking))
				acc.buffer = acc.buffer[safeLen:]
			}
			break
		}
		if acc.thinkingExtracted {
			rest := acc.buffer
			acc.buffer = ""
			if acc.stripTextLeadingNewlinesAfterThinking {
				rest = strings.TrimPrefix(rest, "\r\n\r\n")
				rest = strings.TrimPrefix(rest, "\n\n")
				acc.stripTextLeadingNewlinesAfterThinking = false
			}
			if rest != "" {
				out = append(out, s.ensureTextBlockStart()...)
				out = append(out, s.contentBlockDelta(s.textBlockIdx, "text_delta", "text", rest))
			}
			break
		}
	}
	return out
}

func (s *kiroClaudeStreamState) consumeKiroToolUse(id, name, input string, stop bool) []string {
	var out []string
	if s.textBlockIdx >= 0 && !s.stopped[s.textBlockIdx] {
		out = append(out, s.contentBlockStop(s.textBlockIdx))
		s.stopped[s.textBlockIdx] = true
		s.textBlockIdx = -1
	}
	if s.currentTool != nil && s.currentTool.ID != id {
		out = append(out, s.finishCurrentTool()...)
	}
	if s.currentTool == nil || s.currentTool.ID != id {
		s.currentTool = &kiroToolCall{ID: firstNonEmpty(id, "toolu_"+buildStableAccountID(name+time.Now().UTC().Format(time.RFC3339Nano))), Name: name}
		s.toolBlockIdx[s.currentTool.ID] = s.nextBlockIdx
		out = append(out, fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%q,\"input\":{}}}\n\n", s.nextBlockIdx, s.currentTool.ID, s.currentTool.Name))
		s.nextBlockIdx++
		s.hasTool = true
	}
	if input != "" {
		s.currentTool.Args += input
		s.outputTokens += countTextTokensApprox(input)
		out = append(out, s.contentBlockDelta(s.toolBlockIdx[s.currentTool.ID], "input_json_delta", "partial_json", input))
	}
	if stop {
		out = append(out, s.finishCurrentTool()...)
	}
	return out
}

func (s *kiroClaudeStreamState) finishCurrentTool() []string {
	if s.currentTool == nil {
		return nil
	}
	idx := s.toolBlockIdx[s.currentTool.ID]
	delete(s.toolBlockIdx, s.currentTool.ID)
	s.currentTool = nil
	return []string{s.contentBlockStop(idx)}
}

func (s *kiroClaudeStreamState) ensureTextBlockStart() []string {
	if s.textBlockIdx >= 0 && !s.stopped[s.textBlockIdx] {
		return nil
	}
	if s.thinkingBlockIdx >= 0 && !s.stopped[s.thinkingBlockIdx] {
		return []string{s.contentBlockStop(s.thinkingBlockIdx)}
	}
	s.textBlockIdx = s.nextBlockIdx
	s.nextBlockIdx++
	s.stopped[s.textBlockIdx] = false
	return []string{fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", s.textBlockIdx)}
}

func (s *kiroClaudeStreamState) ensureThinkingBlockStart() []string {
	if s.thinkingBlockIdx >= 0 && !s.stopped[s.thinkingBlockIdx] {
		return nil
	}
	if s.textBlockIdx >= 0 && !s.stopped[s.textBlockIdx] {
		s.stopped[s.textBlockIdx] = true
		stop := s.contentBlockStop(s.textBlockIdx)
		s.textBlockIdx = -1
		s.thinkingBlockIdx = s.nextBlockIdx
		s.nextBlockIdx++
		s.stopped[s.thinkingBlockIdx] = false
		return []string{
			stop,
			fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n", s.thinkingBlockIdx),
		}
	}
	s.thinkingBlockIdx = s.nextBlockIdx
	s.nextBlockIdx++
	s.stopped[s.thinkingBlockIdx] = false
	return []string{fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n", s.thinkingBlockIdx)}
}

func (s *kiroClaudeStreamState) contentBlockDelta(idx int, deltaType, field, value string) string {
	return fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":%q,%q:%q}}\n\n", idx, deltaType, field, value)
}

func (s *kiroClaudeStreamState) contentBlockStop(idx int) string {
	return fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", idx)
}

func (s *kiroClaudeStreamState) finalizeKiroStream() []string {
	var out []string
	acc := &s.accumulator
	if acc.inThinking && acc.buffer != "" {
		out = append(out, s.ensureThinkingBlockStart()...)
		out = append(out, s.contentBlockDelta(s.thinkingBlockIdx, "thinking_delta", "thinking", acc.buffer))
		acc.buffer = ""
	}
	if !acc.inThinking {
		remaining := acc.pendingTextBeforeThinking + acc.buffer
		if remaining != "" {
			out = append(out, s.ensureTextBlockStart()...)
			out = append(out, s.contentBlockDelta(s.textBlockIdx, "text_delta", "text", remaining))
		}
	}
	if s.currentTool != nil {
		out = append(out, s.finishCurrentTool()...)
	}
	for _, idx := range []int{s.thinkingBlockIdx, s.textBlockIdx} {
		if idx >= 0 && !s.stopped[idx] {
			out = append(out, s.contentBlockStop(idx))
			s.stopped[idx] = true
		}
	}
	stopReason := "end_turn"
	if s.hasTool {
		stopReason = "tool_use"
	}
	out = append(out, fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q,\"stop_sequence\":null},\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d}}\n\n", stopReason, s.inputTokens, s.outputTokens))
	out = append(out, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return out
}

func countClaudeInputTokens(raw []byte) int {
	root := gjson.ParseBytes(raw)
	var total strings.Builder
	if sys := root.Get("system"); sys.Exists() {
		total.WriteString(kiroContentToPlainText(sys))
	}
	if msgs := root.Get("messages"); msgs.Exists() && msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			total.WriteString(kiroMessageText(msg))
			return true
		})
	}
	if tools := root.Get("tools"); tools.Exists() {
		total.WriteString(tools.Raw)
	}
	return countTextTokensApprox(total.String())
}

func countTextTokensApprox(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	runes := len([]rune(text))
	return maxInt(1, (runes+3)/4)
}

func anyToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func anyToBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	default:
		return false
	}
}

func anyToInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		i, _ := x.Int64()
		return i
	default:
		return 0
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (p *Proxy) handleClaudeCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeClaudeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "read body failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": countClaudeInputTokens(body)})
}

func parseSingleBracketToolCall(text string) (kiroToolCall, bool) {
	re := regexp.MustCompile(`(?s)\[Called\s+(\w+)\s+with\s+args:\s*(\{.*\})\]`)
	m := re.FindStringSubmatch(text)
	if len(m) != 3 {
		return kiroToolCall{}, false
	}
	args := repairKiroJSON(m[2])
	if !gjson.Valid(args) {
		return kiroToolCall{}, false
	}
	return kiroToolCall{
		ID:   "toolu_" + buildStableAccountID(m[1]+args),
		Name: m[1],
		Args: args,
	}, true
}

func parseBracketToolCallsFromText(text string) []kiroToolCall {
	re := regexp.MustCompile(`(?s)\[Called\s+\w+\s+with\s+args:\s*\{.*?\}\]`)
	matches := re.FindAllString(text, -1)
	var out []kiroToolCall
	for _, match := range matches {
		if call, ok := parseSingleBracketToolCall(match); ok {
			out = append(out, call)
		}
	}
	return out
}
