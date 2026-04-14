package main

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertResponsesRequestToChatCompletionsIgnoresCustomTools(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5-codex",
		"tools":[
			{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"},"justification":{"type":"string"}}}},
			{"type":"custom","name":"apply_patch","format":{"type":"grammar"}}
		],
		"input":"hi"
	}`)

	converted := convertResponsesRequestToChatCompletions("upstream-model", raw, false)
	if gjson.GetBytes(converted, "tools.#").Int() != 1 {
		t.Fatalf("tools=%s", converted)
	}
	if gjson.GetBytes(converted, "tools.0.function.name").String() != "exec_command" {
		t.Fatalf("tools=%s", converted)
	}
	if !gjson.GetBytes(converted, "tools.0.function.strict").Bool() {
		t.Fatalf("tools=%s", converted)
	}
	if !gjson.GetBytes(converted, `tools.0.function.parameters.required.#(=="cmd")`).Exists() {
		t.Fatalf("expected cmd in required, tools=%s", converted)
	}
	if !gjson.GetBytes(converted, `tools.0.function.parameters.required.#(=="justification")`).Exists() {
		t.Fatalf("expected justification in required, tools=%s", converted)
	}
}

func TestConvertResponsesRequestToChatCompletionsConvertsFunctionCallOutput(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5-codex",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	converted := convertResponsesRequestToChatCompletions("upstream-model", raw, false)
	if gjson.GetBytes(converted, "messages.0.tool_calls.0.function.name").String() != "exec_command" {
		t.Fatalf("messages=%s", converted)
	}
	if gjson.GetBytes(converted, "messages.1.role").String() != "tool" {
		t.Fatalf("messages=%s", converted)
	}
}

func TestConvertResponsesRequestToChatCompletionsGroupsParallelFunctionCalls(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5-codex",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call","call_id":"call_2","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"pwd ok"},
			{"type":"function_call_output","call_id":"call_2","output":"ls ok"}
		]
	}`)

	converted := convertResponsesRequestToChatCompletions("upstream-model", raw, false)
	if got := gjson.GetBytes(converted, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("first role=%q body=%s", got, converted)
	}
	if got := gjson.GetBytes(converted, "messages.0.tool_calls.#").Int(); got != 2 {
		t.Fatalf("tool_calls=%d body=%s", got, converted)
	}
	if got := gjson.GetBytes(converted, "messages.1.tool_call_id").String(); got != "call_1" {
		t.Fatalf("first tool output=%q body=%s", got, converted)
	}
	if got := gjson.GetBytes(converted, "messages.2.tool_call_id").String(); got != "call_2" {
		t.Fatalf("second tool output=%q body=%s", got, converted)
	}
}

func TestConvertResponsesRequestToChatCompletionsDowngradesOrphanFunctionCallOutput(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5-codex",
		"input":[
			{"type":"function_call_output","call_id":"call_missing","output":"orphan result"}
		]
	}`)

	converted := convertResponsesRequestToChatCompletions("upstream-model", raw, false)
	if got := gjson.GetBytes(converted, "messages.0.role").String(); got != "user" {
		t.Fatalf("role=%q body=%s", got, converted)
	}
	if got := gjson.GetBytes(converted, "messages.0.content").String(); got != "orphan result" {
		t.Fatalf("content=%q body=%s", got, converted)
	}
}

func TestConvertResponsesRequestToChatCompletionsDowngradesNonPrecedingFunctionCallOutput(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5-codex",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"next question"}]},
			{"type":"function_call_output","call_id":"call_1","output":"late result"}
		]
	}`)

	converted := convertResponsesRequestToChatCompletions("upstream-model", raw, false)
	if got := gjson.GetBytes(converted, "messages.2.role").String(); got != "user" {
		t.Fatalf("role=%q body=%s", got, converted)
	}
	if got := gjson.GetBytes(converted, "messages.2.content").String(); got != "late result" {
		t.Fatalf("content=%q body=%s", got, converted)
	}
}

func TestSanitizeChatToolMessagesDowngradesToolAfterUser(t *testing.T) {
	raw := `{
		"model":"upstream-model",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},
			{"role":"user","content":"interleaved"},
			{"role":"tool","tool_call_id":"call_1","content":"late result"}
		]
	}`

	converted := sanitizeChatToolMessages(raw)
	if got := gjson.Get(converted, "messages.2.role").String(); got != "user" {
		t.Fatalf("role=%q body=%s", got, converted)
	}
	if gjson.Get(converted, "messages.2.tool_call_id").Exists() {
		t.Fatalf("tool_call_id should be removed, body=%s", converted)
	}
}

func TestConvertResponsesRequestToChatCompletionsSanitizesBrokenFunctionArguments(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5-codex",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"cat README_JP.md | head -50\"{\"cmd\": \"cat README_JP.md | head -50\"}"}
		]
	}`)

	converted := convertResponsesRequestToChatCompletions("upstream-model", raw, false)
	if got := gjson.GetBytes(converted, "messages.0.tool_calls.0.function.arguments").String(); got != `{"cmd":"cat README_JP.md | head -50"}` {
		t.Fatalf("arguments=%q body=%s", got, converted)
	}
}

func TestConvertResponsesRequestToChatCompletionsPlainStringContent(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5-codex",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_text","text":"first"},
				{"type":"input_image","image_url":"data:image/png;base64,abc"},
				{"type":"input_text","text":"second"}
			]
		}]
	}`)

	converted := convertResponsesRequestToChatCompletions("upstream-model", raw, false, true)
	if gjson.GetBytes(converted, "messages.0.content").Type != gjson.String {
		t.Fatalf("expected plain string content, got %s", converted)
	}
	if got := gjson.GetBytes(converted, "messages.0.content").String(); got != "first\n\nsecond" {
		t.Fatalf("content=%q body=%s", got, converted)
	}
}

func TestConvertResponsesRequestToChatCompletionsPreservesImages(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5-codex",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_text","text":"describe"},
				{"type":"input_image","image_url":"data:image/png;base64,abc"}
			]
		}]
	}`)

	converted := convertResponsesRequestToChatCompletions("upstream-model", raw, false, false)
	if got := gjson.GetBytes(converted, "messages.0.content.0.type").String(); got != "text" {
		t.Fatalf("first content type=%q body=%s", got, converted)
	}
	if got := gjson.GetBytes(converted, "messages.0.content.1.type").String(); got != "image_url" {
		t.Fatalf("second content type=%q body=%s", got, converted)
	}
	if got := gjson.GetBytes(converted, "messages.0.content.1.image_url.url").String(); got != "data:image/png;base64,abc" {
		t.Fatalf("image url=%q body=%s", got, converted)
	}
}

func TestConvertChatCompletionsResponseToResponsesToolCall(t *testing.T) {
	req := []byte(`{"model":"gpt-5-codex"}`)
	resp := []byte(`{
		"id":"chatcmpl_123",
		"object":"chat.completion",
		"created":123,
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}]
			},
			"finish_reason":"tool_calls"
		}]
	}`)

	converted := convertChatCompletionsResponseToResponses(req, resp)
	if gjson.Get(converted, "output.0.name").String() != "exec_command" {
		t.Fatalf("converted=%s", converted)
	}
}

func TestConvertChatCompletionsResponseToResponsesIncludesReasoningAndRequestFields(t *testing.T) {
	req := []byte(`{"model":"gpt-5-codex","instructions":"be precise","parallel_tool_calls":true,"reasoning":{"effort":"medium"}}`)
	resp := []byte(`{
		"id":"chatcmpl_123",
		"object":"chat.completion",
		"created":123,
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"reasoning_content":"thinking",
				"content":"answer"
			},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}
	}`)

	converted := convertChatCompletionsResponseToResponses(req, resp)
	if gjson.Get(converted, "instructions").String() != "be precise" {
		t.Fatalf("converted=%s", converted)
	}
	if !gjson.Get(converted, "parallel_tool_calls").Bool() {
		t.Fatalf("converted=%s", converted)
	}
	if gjson.Get(converted, "output.0.type").String() != "reasoning" {
		t.Fatalf("converted=%s", converted)
	}
	if gjson.Get(converted, "output.1.content.0.text").String() != "answer" {
		t.Fatalf("converted=%s", converted)
	}
}

func TestConvertChatCompletionsStreamToResponsesEmitsFunctionArgumentDelta(t *testing.T) {
	var state any
	req := []byte(`{"model":"gpt-5-codex"}`)
	chunk := []byte(`data: {"id":"chatcmpl_123","object":"chat.completion.chunk","created":123,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":null}]}`)

	events := convertChatCompletionsStreamToResponses(req, chunk, &state)
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "response.function_call_arguments.delta") {
		t.Fatalf("events=%s", joined)
	}
	if !strings.Contains(joined, `"delta":"{\"cmd\":\"pwd\"}"`) {
		t.Fatalf("events=%s", joined)
	}
}

func TestConvertChatCompletionsStreamToResponsesKeepsMultipleToolCallsSeparate(t *testing.T) {
	var state any
	req := []byte(`{"model":"gpt-5-codex"}`)
	chunk1 := []byte("data: {\"id\":\"chatcmpl_123\",\"object\":\"chat.completion.chunk\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"exec_command\",\"arguments\":\"{\\\"cmd\\\":\\\"cat README.md\\\"}\"}},{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"exec_command\",\"arguments\":\"{\\\"cmd\\\":\\\"cat README_CN.md\\\"}\"}}]},\"finish_reason\":null}]}")
	chunk2 := []byte("data: {\"id\":\"chatcmpl_123\",\"object\":\"chat.completion.chunk\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}")

	events1 := convertChatCompletionsStreamToResponses(req, chunk1, &state)
	events2 := convertChatCompletionsStreamToResponses(req, chunk2, &state)
	joined := strings.Join(append(events1, events2...), "\n")
	if strings.Count(joined, "response.output_item.added") < 2 {
		t.Fatalf("events=%s", joined)
	}
	if strings.Count(joined, "response.function_call_arguments.done") < 2 {
		t.Fatalf("events=%s", joined)
	}
}

func TestConvertChatCompletionsStreamToResponsesEmitsReasoningAndCompletedFields(t *testing.T) {
	var state any
	req := []byte(`{"model":"gpt-5-codex","instructions":"be precise","reasoning":{"effort":"medium"}}`)
	chunk1 := []byte(`data: {"id":"chatcmpl_123","object":"chat.completion.chunk","created":123,"choices":[{"index":0,"delta":{"reasoning_content":"think ","content":"done"},"finish_reason":null}]}`)
	chunk2 := []byte(`data: {"id":"chatcmpl_123","object":"chat.completion.chunk","created":123,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)

	events1 := convertChatCompletionsStreamToResponses(req, chunk1, &state)
	events2 := convertChatCompletionsStreamToResponses(req, chunk2, &state)
	joined := strings.Join(append(events1, events2...), "\n")
	if !strings.Contains(joined, "response.reasoning_summary_text.delta") {
		t.Fatalf("events=%s", joined)
	}
	if !strings.Contains(joined, `"instructions":"be precise"`) {
		t.Fatalf("events=%s", joined)
	}
	if !strings.Contains(joined, `"parallel_tool_calls"`) && strings.Contains(string(req), "parallel_tool_calls") {
		t.Fatalf("events=%s", joined)
	}
}
