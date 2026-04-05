package main

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func claudeRequestToCodexResponses(modelName string, inputRawJSON []byte) []byte {
	root := gjson.ParseBytes(inputRawJSON)
	out := `{"model":"","instructions":"","input":[],"parallel_tool_calls":true,"reasoning":{"effort":"medium","summary":"auto"},"stream":true,"store":false,"include":["reasoning.encrypted_content"]}`
	out, _ = sjson.Set(out, "model", modelName)

	if systems := root.Get("system"); systems.Exists() && systems.IsArray() {
		msg := `{"type":"message","role":"developer","content":[]}`
		i := 0
		systems.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				msg, _ = sjson.Set(msg, fmt.Sprintf("content.%d.type", i), "input_text")
				msg, _ = sjson.Set(msg, fmt.Sprintf("content.%d.text", i), part.Get("text").String())
				i++
			}
			return true
		})
		if i > 0 {
			out, _ = sjson.SetRaw(out, "input.-1", msg)
		}
	}

	if thinking := root.Get("thinking"); thinking.Exists() && thinking.IsObject() {
		switch strings.TrimSpace(thinking.Get("type").String()) {
		case "disabled":
			out, _ = sjson.Set(out, "reasoning.effort", "low")
		case "adaptive":
			out, _ = sjson.Set(out, "reasoning.effort", "high")
		case "enabled":
			budget := thinking.Get("budget_tokens").Int()
			switch {
			case budget <= 2048:
				out, _ = sjson.Set(out, "reasoning.effort", "low")
			case budget <= 8192:
				out, _ = sjson.Set(out, "reasoning.effort", "medium")
			default:
				out, _ = sjson.Set(out, "reasoning.effort", "high")
			}
		}
	}

	toolMap := buildClaudeOriginalToShortToolMap(inputRawJSON)
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, message gjson.Result) bool {
			role := message.Get("role").String()
			newMsg := func() string {
				msg := `{"type":"message","role":"","content":[]}`
				msg, _ = sjson.Set(msg, "role", role)
				return msg
			}
			msg := newMsg()
			contentIdx := 0
			hasContent := false
			flush := func() {
				if hasContent {
					out, _ = sjson.SetRaw(out, "input.-1", msg)
					msg = newMsg()
					contentIdx = 0
					hasContent = false
				}
			}
			appendText := func(text string) {
				if text == "" {
					return
				}
				partType := "input_text"
				if role == "assistant" {
					partType = "output_text"
				}
				msg, _ = sjson.Set(msg, fmt.Sprintf("content.%d.type", contentIdx), partType)
				msg, _ = sjson.Set(msg, fmt.Sprintf("content.%d.text", contentIdx), text)
				contentIdx++
				hasContent = true
			}

			content := message.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					switch part.Get("type").String() {
					case "text":
						appendText(part.Get("text").String())
					case "tool_use":
						flush()
						call := `{"type":"function_call"}`
						call, _ = sjson.Set(call, "call_id", part.Get("id").String())
						name := part.Get("name").String()
						if short, ok := toolMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						call, _ = sjson.Set(call, "name", name)
						args := strings.TrimSpace(part.Get("input").Raw)
						if args == "" || args == "null" {
							args = "{}"
						}
						call, _ = sjson.Set(call, "arguments", args)
						out, _ = sjson.SetRaw(out, "input.-1", call)
					case "tool_result":
						flush()
						result := `{"type":"function_call_output"}`
						result, _ = sjson.Set(result, "call_id", part.Get("tool_use_id").String())
						result, _ = sjson.Set(result, "output", toolResultContentToString(part.Get("content")))
						out, _ = sjson.SetRaw(out, "input.-1", result)
					case "thinking":
						if role == "assistant" {
							msg, _ = sjson.Set(msg, "reasoning_content", part.Get("thinking").String())
						}
					}
					return true
				})
				flush()
			} else if content.Type == gjson.String {
				appendText(content.String())
				flush()
			}
			return true
		})
	}

	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "web_search_20250305" {
				out, _ = sjson.SetRaw(out, "tools.-1", `{"type":"web_search"}`)
				return true
			}
			t := tool.Raw
			t, _ = sjson.Set(t, "type", "function")
			name := tool.Get("name").String()
			if short, ok := toolMap[name]; ok {
				t, _ = sjson.Set(t, "name", short)
			} else {
				t, _ = sjson.Set(t, "name", shortenNameIfNeeded(name))
			}
			t, _ = sjson.Set(t, "description", codexToolDescription(name, tool.Get("description").String()))
			t, _ = sjson.SetRaw(t, "parameters", normalizeToolParameters(tool.Get("input_schema").Raw))
			t, _ = sjson.Delete(t, "input_schema")
			t, _ = sjson.Set(t, "strict", false)
			out, _ = sjson.SetRaw(out, "tools.-1", t)
			return true
		})
		out, _ = sjson.Set(out, "tool_choice", "auto")
	}

	return []byte(out)
}

func codexToolDescription(name, description string) string {
	description = strings.TrimSpace(description)
	switch strings.TrimSpace(name) {
	case "Edit":
		const suffix = " Before calling Edit, read the target file first. file_path must point to an existing file. old_string must match the existing file content exactly and should be a long unique snippet, not a short guess. If the target text appears multiple times, set replace_all=true. Do not use Edit to create a new file."
		if description == "" {
			return strings.TrimSpace(suffix)
		}
		return description + suffix
	default:
		return description
	}
}

func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		if idx := strings.LastIndex(name, "__"); idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > limit {
				return cand[:limit]
			}
			return cand
		}
	}
	return name[:limit]
}

func buildClaudeOriginalToShortToolMap(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	out := map[string]string{}
	if !tools.IsArray() {
		return out
	}
	var names []string
	tools.ForEach(func(_, tool gjson.Result) bool {
		if name := tool.Get("name").String(); name != "" {
			names = append(names, name)
		}
		return true
	})
	shortMap := buildShortNameMap(names)
	for k, v := range shortMap {
		out[k] = v
	}
	return out
}

func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	out := map[string]string{}
	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			if idx := strings.LastIndex(n, "__"); idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}
	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			tmp := cand
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp += suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}
	for _, name := range names {
		cand := makeUnique(baseCandidate(name))
		used[cand] = struct{}{}
		out[name] = cand
	}
	return out
}

func buildCodexShortToOriginalToolMap(original []byte) map[string]string {
	out := map[string]string{}
	for orig, short := range buildClaudeOriginalToShortToolMap(original) {
		out[short] = orig
	}
	return out
}

type codexToClaudeState struct {
	HasToolCall bool
	BlockIndex  int
}

func convertCodexStreamToClaude(_ context.Context, originalRequestRawJSON, rawJSON []byte, param *any) []string {
	if *param == nil {
		*param = &codexToClaudeState{}
	}
	st := (*param).(*codexToClaudeState)
	if !bytes.HasPrefix(rawJSON, []byte("data:")) {
		return nil
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])
	root := gjson.ParseBytes(rawJSON)
	switch root.Get("type").String() {
	case "response.created":
		j := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`
		j, _ = sjson.Set(j, "message.id", root.Get("response.id").String())
		j, _ = sjson.Set(j, "message.model", root.Get("response.model").String())
		return []string{"event: message_start\ndata: " + j + "\n\n"}
	case "response.reasoning_summary_part.added":
		j := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, st.BlockIndex)
		return []string{"event: content_block_start\ndata: " + j + "\n\n"}
	case "response.reasoning_summary_text.delta":
		j := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%q}}`, st.BlockIndex, root.Get("delta").String())
		return []string{"event: content_block_delta\ndata: " + j + "\n\n"}
	case "response.reasoning_summary_part.done":
		j := fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, st.BlockIndex)
		st.BlockIndex++
		return []string{"event: content_block_stop\ndata: " + j + "\n\n"}
	case "response.content_part.added":
		j := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, st.BlockIndex)
		return []string{"event: content_block_start\ndata: " + j + "\n\n"}
	case "response.output_text.delta":
		j := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, st.BlockIndex, root.Get("delta").String())
		return []string{"event: content_block_delta\ndata: " + j + "\n\n"}
	case "response.content_part.done":
		j := fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, st.BlockIndex)
		st.BlockIndex++
		return []string{"event: content_block_stop\ndata: " + j + "\n\n"}
	case "response.output_item.added":
		if root.Get("item.type").String() == "function_call" {
			st.HasToolCall = true
			rev := buildCodexShortToOriginalToolMap(originalRequestRawJSON)
			name := root.Get("item.name").String()
			if orig, ok := rev[name]; ok {
				name = orig
			}
			start := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, st.BlockIndex, root.Get("item.call_id").String(), name)
			delta := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, st.BlockIndex)
			return []string{
				"event: content_block_start\ndata: " + start + "\n\n",
				"event: content_block_delta\ndata: " + delta + "\n\n",
			}
		}
	case "response.function_call_arguments.delta":
		j := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, st.BlockIndex, root.Get("delta").String())
		return []string{"event: content_block_delta\ndata: " + j + "\n\n"}
	case "response.output_item.done":
		if root.Get("item.type").String() == "function_call" {
			j := fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, st.BlockIndex)
			st.BlockIndex++
			return []string{"event: content_block_stop\ndata: " + j + "\n\n"}
		}
	case "response.completed":
		stopReason := "end_turn"
		if st.HasToolCall {
			stopReason = "tool_use"
		}
		j := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
			stopReason,
			root.Get("response.usage.input_tokens").Int(),
			root.Get("response.usage.output_tokens").Int(),
		)
		return []string{
			"event: message_delta\ndata: " + j + "\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
	}
	return nil
}

func convertCodexResponseToClaudeNonStream(_ context.Context, originalRequestRawJSON, rawJSON []byte) string {
	root := gjson.ParseBytes(rawJSON)
	if root.Get("type").String() != "response.completed" {
		return ""
	}
	rev := buildCodexShortToOriginalToolMap(originalRequestRawJSON)
	resp := root.Get("response")
	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	out, _ = sjson.Set(out, "id", resp.Get("id").String())
	out, _ = sjson.Set(out, "model", resp.Get("model").String())
	out, _ = sjson.Set(out, "usage.input_tokens", resp.Get("usage.input_tokens").Int())
	out, _ = sjson.Set(out, "usage.output_tokens", resp.Get("usage.output_tokens").Int())
	hasToolCall := false
	resp.Get("output").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "reasoning":
			text := strings.TrimSpace(item.Get("summary.0.text").String())
			if text != "" {
				block := `{"type":"thinking","thinking":""}`
				block, _ = sjson.Set(block, "thinking", text)
				out, _ = sjson.SetRaw(out, "content.-1", block)
			}
		case "message":
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				text := part.Get("text").String()
				if text != "" {
					block := `{"type":"text","text":""}`
					block, _ = sjson.Set(block, "text", text)
					out, _ = sjson.SetRaw(out, "content.-1", block)
				}
				return true
			})
		case "function_call":
			hasToolCall = true
			name := item.Get("name").String()
			if orig, ok := rev[name]; ok {
				name = orig
			}
			block := `{"type":"tool_use","id":"","name":"","input":{}}`
			block, _ = sjson.Set(block, "id", item.Get("call_id").String())
			block, _ = sjson.Set(block, "name", name)
			if args := item.Get("arguments").String(); args != "" && gjson.Valid(args) && gjson.Parse(args).IsObject() {
				block, _ = sjson.SetRaw(block, "input", args)
			}
			out, _ = sjson.SetRaw(out, "content.-1", block)
		}
		return true
	})
	if hasToolCall {
		out, _ = sjson.Set(out, "stop_reason", "tool_use")
	} else {
		out, _ = sjson.Set(out, "stop_reason", "end_turn")
	}
	return out
}
