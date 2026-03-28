package main

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestFixJSONParsesTaggedToolArguments(t *testing.T) {
	raw := `<tool_call>weather<arg_key>city</arg_key><arg_value>Hangzhou</arg_value></tool_call>`
	got := fixJSON(raw)
	if got != `{"city":"Hangzhou"}` {
		t.Fatalf("expected parsed tool args, got %q", got)
	}
}

func TestToolCallToClaudeBlockParsesTaggedArguments(t *testing.T) {
	raw := `{"id":"tool_1","function":{"name":"weather","arguments":"<tool_call>weather<arg_key>city</arg_key><arg_value>Hangzhou</arg_value></tool_call>"}}`
	block := toolCallToClaudeBlock(parseJSON(raw))
	if !strings.Contains(block, `"input":{"city":"Hangzhou"}`) {
		t.Fatalf("expected Claude tool input to contain parsed city, got %s", block)
	}
}

func TestMapFinishReasonTreatsStopAsToolUseWhenToolPresent(t *testing.T) {
	if got := mapFinishReason("stop", true); got != "tool_use" {
		t.Fatalf("expected tool_use, got %q", got)
	}
}

func parseJSON(raw string) gjson.Result {
	return gjson.Parse(raw)
}

func TestClaudeRequestToOpenAIBasic(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"max_tokens": 1024,
		"temperature": 0.7,
		"messages": [
			{"role": "user", "content": "Hello, world!"}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)

	if parsed.Get("model").String() != "gpt-4" {
		t.Errorf("expected model gpt-4, got %q", parsed.Get("model").String())
	}
	if parsed.Get("max_tokens").Int() != 1024 {
		t.Errorf("expected max_tokens 1024, got %d", parsed.Get("max_tokens").Int())
	}
	if parsed.Get("temperature").Float() != 0.7 {
		t.Errorf("expected temperature 0.7, got %f", parsed.Get("temperature").Float())
	}
	if parsed.Get("stream").Bool() != false {
		t.Errorf("expected stream false, got %v", parsed.Get("stream").Bool())
	}

	messages := parsed.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Get("role").String() != "user" {
		t.Errorf("expected role user, got %q", messages[0].Get("role").String())
	}
}

func TestClaudeRequestToOpenAIWithSystem(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)

	messages := parsed.Get("messages").Array()
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(messages))
	}
	if messages[0].Get("role").String() != "system" {
		t.Errorf("expected first message role system, got %q", messages[0].Get("role").String())
	}
}

func TestClaudeRequestToOpenAIWithStopSequences(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"stop_sequences": ["END", "STOP"],
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)

	stops := parsed.Get("stop").Array()
	if len(stops) != 2 {
		t.Errorf("expected 2 stop sequences, got %d", len(stops))
	}
}

func TestClaudeRequestToOpenAIWithSingleStopSequence(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"stop_sequences": ["END"],
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)

	if parsed.Get("stop").String() != "END" {
		t.Errorf("expected stop string END, got %q", parsed.Get("stop").String())
	}
}

func TestClaudeRequestToOpenAIWithThinking(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"thinking": {"type": "enabled"},
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)

	if parsed.Get("reasoning_effort").String() != "high" {
		t.Errorf("expected reasoning_effort high, got %q", parsed.Get("reasoning_effort").String())
	}
}

func TestClaudeRequestToOpenAIWithTools(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"tools": [
			{
				"name": "get_weather",
				"description": "Get the weather",
				"input_schema": {
					"type": "object",
					"properties": {
						"location": {"type": "string"}
					}
				}
			}
		],
		"messages": [
			{"role": "user", "content": "What's the weather?"}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)

	tools := parsed.Get("tools").Array()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Get("function.name").String() != "get_weather" {
		t.Errorf("expected function name get_weather, got %q", tools[0].Get("function.name").String())
	}
}

func TestClaudeRequestToOpenAIWithToolChoice(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "auto",
			input: `{
				"model": "claude-3-opus",
				"tool_choice": {"type": "auto"},
				"messages": [{"role": "user", "content": "Hello"}]
			}`,
			expected: "auto",
		},
		{
			name: "any",
			input: `{
				"model": "claude-3-opus",
				"tool_choice": {"type": "any"},
				"messages": [{"role": "user", "content": "Hello"}]
			}`,
			expected: "required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			openaiReq := claudeRequestToOpenAI([]byte(tt.input), "gpt-4", false)
			parsed := gjson.ParseBytes(openaiReq)

			if parsed.Get("tool_choice").String() != tt.expected {
				t.Errorf("expected tool_choice %q, got %q", tt.expected, parsed.Get("tool_choice").String())
			}
		})
	}
}

func TestClaudeRequestToOpenAIWithToolResult(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"messages": [
			{"role": "user", "content": "What's the weather?"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "tool_1", "name": "get_weather", "input": {"location": "SF"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tool_1", "content": "Sunny, 72F"}
			]}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)

	messages := parsed.Get("messages").Array()
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	// Tool result should become a tool message
	lastMsg := messages[len(messages)-1]
	if lastMsg.Get("role").String() != "tool" {
		t.Errorf("expected last message role tool, got %q", lastMsg.Get("role").String())
	}
	if lastMsg.Get("tool_call_id").String() != "tool_1" {
		t.Errorf("expected tool_call_id tool_1, got %q", lastMsg.Get("tool_call_id").String())
	}
}

func TestClaudeRequestToOpenAIWithErrorToolResult(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "tool_1", "name": "update_file", "input": {"path": "proxy_test.go"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tool_1", "is_error": true, "content": [{"type":"text","text":"Error editing file"}]}
			]}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)
	messages := parsed.Get("messages").Array()
	lastMsg := messages[len(messages)-1]

	if lastMsg.Get("role").String() != "tool" {
		t.Fatalf("expected tool role, got %q", lastMsg.Get("role").String())
	}
	if got := lastMsg.Get("content").String(); got != "ERROR: Error editing file" {
		t.Fatalf("expected error-prefixed tool content, got %q", got)
	}
}

func TestToolResultContentToStringObjectPreservesJSON(t *testing.T) {
	content := gjson.Parse(`{"error":"Error editing file","path":"proxy_test.go"}`)
	got := toolResultContentToString(content)
	if got != `{"error":"Error editing file","path":"proxy_test.go"}` {
		t.Fatalf("expected raw object JSON, got %q", got)
	}
}

func TestClaudeRequestToOpenAIWithImage(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "What's in this image?"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "base64data"}}
				]
			}
		]
	}`

	openaiReq := claudeRequestToOpenAI([]byte(claudeReq), "gpt-4", false)
	parsed := gjson.ParseBytes(openaiReq)

	messages := parsed.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	content := messages[0].Get("content").Array()
	if len(content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(content))
	}

	// Second part should be image_url
	if content[1].Get("type").String() != "image_url" {
		t.Errorf("expected type image_url, got %q", content[1].Get("type").String())
	}
	imageURL := content[1].Get("image_url.url").String()
	if !strings.HasPrefix(imageURL, "data:image/png;base64,") {
		t.Errorf("expected data URL prefix, got %q", imageURL[:50])
	}
}

func TestOpenAIResponseToClaudeBasic(t *testing.T) {
	openaiResp := `{
		"id": "chatcmpl-123",
		"model": "gpt-4",
		"choices": [
			{
				"message": {
					"role": "assistant",
					"content": "Hello, I'm Claude!"
				},
				"finish_reason": "stop"
			}
		],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5
		}
	}`

	claudeResp := openaiResponseToClaude([]byte(openaiResp))
	parsed := gjson.ParseBytes(claudeResp)

	if parsed.Get("type").String() != "message" {
		t.Errorf("expected type message, got %q", parsed.Get("type").String())
	}
	if parsed.Get("role").String() != "assistant" {
		t.Errorf("expected role assistant, got %q", parsed.Get("role").String())
	}
	if parsed.Get("stop_reason").String() != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %q", parsed.Get("stop_reason").String())
	}

	content := parsed.Get("content").Array()
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	if content[0].Get("type").String() != "text" {
		t.Errorf("expected content type text, got %q", content[0].Get("type").String())
	}
}

func TestOpenAIResponseToClaudeWithToolCalls(t *testing.T) {
	openaiResp := `{
		"id": "chatcmpl-123",
		"model": "gpt-4",
		"choices": [
			{
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [
						{
							"id": "call_1",
							"function": {
								"name": "get_weather",
								"arguments": "{\"location\": \"SF\"}"
							}
						}
					]
				},
				"finish_reason": "tool_calls"
			}
		],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5
		}
	}`

	claudeResp := openaiResponseToClaude([]byte(openaiResp))
	parsed := gjson.ParseBytes(claudeResp)

	if parsed.Get("stop_reason").String() != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %q", parsed.Get("stop_reason").String())
	}

	content := parsed.Get("content").Array()
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	if content[0].Get("type").String() != "tool_use" {
		t.Errorf("expected content type tool_use, got %q", content[0].Get("type").String())
	}
	if content[0].Get("name").String() != "get_weather" {
		t.Errorf("expected tool name get_weather, got %q", content[0].Get("name").String())
	}
}

func TestOpenAIResponseToClaudeWithReasoning(t *testing.T) {
	openaiResp := `{
		"id": "chatcmpl-123",
		"model": "gpt-4",
		"choices": [
			{
				"message": {
					"role": "assistant",
					"content": "Final answer",
					"reasoning_content": "Let me think about this..."
				},
				"finish_reason": "stop"
			}
		],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5
		}
	}`

	claudeResp := openaiResponseToClaude([]byte(openaiResp))
	parsed := gjson.ParseBytes(claudeResp)

	content := parsed.Get("content").Array()
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d", len(content))
	}
	if content[0].Get("type").String() != "thinking" {
		t.Errorf("expected first content type thinking, got %q", content[0].Get("type").String())
	}
	if content[1].Get("type").String() != "text" {
		t.Errorf("expected second content type text, got %q", content[1].Get("type").String())
	}
}

func TestOpenAIResponseToClaudeWithReasoningField(t *testing.T) {
	openaiResp := `{
		"id": "chatcmpl-123",
		"model": "gpt-4",
		"choices": [
			{
				"message": {
					"role": "assistant",
					"content": "Final answer",
					"reasoning": "Let me think about this..."
				},
				"finish_reason": "stop"
			}
		],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5
		}
	}`

	claudeResp := openaiResponseToClaude([]byte(openaiResp))
	parsed := gjson.ParseBytes(claudeResp)

	content := parsed.Get("content").Array()
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d", len(content))
	}
	if content[0].Get("type").String() != "thinking" {
		t.Errorf("expected first content type thinking, got %q", content[0].Get("type").String())
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		reason  string
		hasTool bool
		want    string
	}{
		{"stop", false, "end_turn"},
		{"stop", true, "tool_use"},
		{"length", false, "max_tokens"},
		{"tool_calls", false, "tool_use"},
		{"function_call", false, "tool_use"},
		{"unknown", false, "end_turn"},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			got := mapFinishReason(tt.reason, tt.hasTool)
			if got != tt.want {
				t.Errorf("mapFinishReason(%q, %v) = %q, want %q", tt.reason, tt.hasTool, got, tt.want)
			}
		})
	}
}

func TestFixJSONValidInput(t *testing.T) {
	input := `{"key": "value"}`
	got := fixJSON(input)
	if got != input {
		t.Errorf("expected unchanged valid JSON, got %q", got)
	}
}

func TestFixJSONTruncatedObject(t *testing.T) {
	input := `{"key": "value"`
	got := fixJSON(input)
	if !gjson.Valid(got) {
		t.Errorf("expected valid JSON, got %q", got)
	}
}

func TestFixJSONTruncatedArray(t *testing.T) {
	input := `{"key": [1, 2`
	got := fixJSON(input)
	if !gjson.Valid(got) {
		t.Errorf("expected valid JSON, got %q", got)
	}
}

func TestFixJSONEmpty(t *testing.T) {
	got := fixJSON("")
	if got != "{}" {
		t.Errorf("expected empty object, got %q", got)
	}
}

func TestConvertContentPartText(t *testing.T) {
	part := `{"type": "text", "text": "Hello"}`
	result, ok := convertContentPart(gjson.Parse(part))
	if !ok {
		t.Fatal("expected ok to be true")
	}
	if !strings.Contains(result, `"type":"text"`) {
		t.Errorf("expected text type, got %q", result)
	}
}

func TestConvertContentPartEmptyText(t *testing.T) {
	part := `{"type": "text", "text": "   "}`
	_, ok := convertContentPart(gjson.Parse(part))
	if ok {
		t.Fatal("expected ok to be false for empty text")
	}
}

func TestConvertContentPartImageBase64(t *testing.T) {
	part := `{
		"type": "image",
		"source": {
			"type": "base64",
			"media_type": "image/jpeg",
			"data": "base64encodeddata"
		}
	}`
	result, ok := convertContentPart(gjson.Parse(part))
	if !ok {
		t.Fatal("expected ok to be true")
	}
	if !strings.Contains(result, `"type":"image_url"`) {
		t.Errorf("expected image_url type, got %q", result)
	}
	if !strings.Contains(result, "data:image/jpeg;base64,") {
		t.Errorf("expected data URL, got %q", result)
	}
}

func TestConvertContentPartImageURL(t *testing.T) {
	part := `{
		"type": "image",
		"source": {
			"type": "url",
			"url": "https://example.com/image.png"
		}
	}`
	result, ok := convertContentPart(gjson.Parse(part))
	if !ok {
		t.Fatal("expected ok to be true")
	}
	if !strings.Contains(result, "https://example.com/image.png") {
		t.Errorf("expected URL in result, got %q", result)
	}
}

func TestToolResultContentToString(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "string content",
			content: `"result string"`,
			want:    "result string",
		},
		{
			name:    "array content",
			content: `[{"type": "text", "text": "part1"}, {"type": "text", "text": "part2"}]`,
			want:    "part1\n\npart2",
		},
		{
			name:    "object with text",
			content: `{"text": "result"}`,
			want:    "result",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolResultContentToString(gjson.Parse(tt.content))
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCollectTexts(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
	}{
		{
			name:    "simple string",
			input:   `"hello"`,
			wantLen: 1,
		},
		{
			name:    "array of strings",
			input:   `["one", "two"]`,
			wantLen: 2,
		},
		{
			name:    "object with text",
			input:   `{"text": "content"}`,
			wantLen: 1,
		},
		{
			name:    "empty string",
			input:   `""`,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collectTexts(gjson.Parse(tt.input))
			if len(got) != tt.wantLen {
				t.Errorf("got %d texts, want %d", len(got), tt.wantLen)
			}
		})
	}
}
