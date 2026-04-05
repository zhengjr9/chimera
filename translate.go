package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ---------------------------------------------------------------------------
// Request translation: Claude -> OpenAI
// ---------------------------------------------------------------------------

func claudeRequestToOpenAI(raw []byte, model string, stream bool) []byte {
	root := gjson.ParseBytes(raw)
	out, _ := sjson.Set("{}", "model", model)
	out, _ = sjson.Set(out, "stream", stream)

	// max_tokens
	if v := root.Get("max_tokens"); v.Exists() {
		out, _ = sjson.Set(out, "max_tokens", v.Int())
	}
	// temperature / top_p
	if v := root.Get("temperature"); v.Exists() {
		out, _ = sjson.Set(out, "temperature", v.Float())
	} else if v := root.Get("top_p"); v.Exists() {
		out, _ = sjson.Set(out, "top_p", v.Float())
	}
	// stop_sequences -> stop
	if ss := root.Get("stop_sequences"); ss.Exists() && ss.IsArray() {
		var stops []string
		ss.ForEach(func(_, v gjson.Result) bool { stops = append(stops, v.String()); return true })
		if len(stops) > 0 {
			if len(stops) == 1 {
				out, _ = sjson.Set(out, "stop", stops[0])
			} else {
				out, _ = sjson.Set(out, "stop", stops)
			}
		}
	}

	// thinking -> reasoning_effort (best-effort mapping)
	if tc := root.Get("thinking"); tc.Exists() && tc.IsObject() {
		if effort := mapClaudeThinkingToReasoningEffort(tc); effort != "" {
			out, _ = sjson.Set(out, "reasoning_effort", effort)
		}
	}

	// system message
	msgsStr := "[]"
	if sys := root.Get("system"); sys.Exists() {
		sysMsg := `{"role":"system","content":[]}`
		var systemParts []string
		if sys.Type == gjson.String && sys.String() != "" {
			systemParts = append(systemParts, fmt.Sprintf(`{"type":"text","text":%q}`, sys.String()))
		} else if sys.IsArray() {
			sys.ForEach(func(_, v gjson.Result) bool {
				if item, ok := convertContentPart(v); ok {
					systemParts = append(systemParts, item)
				}
				return true
			})
		}
		if applyMessageContent(&sysMsg, systemParts) {
			msgsStr, _ = sjson.SetRaw(msgsStr, "-1", sysMsg)
		}
	}

	// messages
	if ms := root.Get("messages"); ms.Exists() && ms.IsArray() {
		ms.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			content := msg.Get("content")

			if content.Exists() && content.IsArray() {
				var textParts, thinkingParts []string
				var toolCalls []interface{}
				var toolResults []string

				content.ForEach(func(_, part gjson.Result) bool {
					switch part.Get("type").String() {
					case "thinking", "redacted_thinking":
						if role == "assistant" {
							t := extractClaudeThinkingText(part)
							if strings.TrimSpace(t) != "" {
								thinkingParts = append(thinkingParts, t)
							}
						}
					case "text", "image":
						if item, ok := convertContentPart(part); ok {
							textParts = append(textParts, item)
						}
					case "tool_use":
						if role == "assistant" {
							name := strings.TrimSpace(part.Get("name").String())
							if name == "" {
								return true
							}
							tc := `{"id":"","type":"function","function":{"name":"","arguments":""}}`
							tc, _ = sjson.Set(tc, "id", part.Get("id").String())
							tc, _ = sjson.Set(tc, "function.name", name)
							if input := part.Get("input"); input.Exists() {
								tc, _ = sjson.Set(tc, "function.arguments", input.Raw)
							}
							toolCalls = append(toolCalls, gjson.Parse(tc).Value())
						}
					case "tool_result":
						if strings.TrimSpace(part.Get("tool_use_id").String()) == "" {
							return true
						}
						toolContent := toolResultContentToString(part.Get("content"))
						if part.Get("is_error").Bool() {
							toolContent = "ERROR: " + toolContent
						}
						tr := fmt.Sprintf(`{"role":"tool","tool_call_id":%q,"content":%q}`,
							part.Get("tool_use_id").String(),
							toolContent)
						toolResults = append(toolResults, tr)
					}
					return true
				})

				// emit tool_result messages first
				for _, tr := range toolResults {
					msgsStr, _ = sjson.SetRaw(msgsStr, "-1", tr)
				}

				reasoning := strings.Join(thinkingParts, "\n\n")
				hasText := len(textParts) > 0
				hasReason := reasoning != ""
				hasTC := len(toolCalls) > 0

				if role == "assistant" && (hasText || hasReason || hasTC) {
					m := `{"role":"assistant"}`
					if !applyMessageContent(&m, textParts) {
						m, _ = sjson.Set(m, "content", "")
					}
					if hasReason {
						m, _ = sjson.Set(m, "reasoning_content", reasoning)
					}
					if hasTC {
						m, _ = sjson.Set(m, "tool_calls", toolCalls)
					}
					msgsStr, _ = sjson.SetRaw(msgsStr, "-1", m)
				} else if hasText {
					m := fmt.Sprintf(`{"role":%q,"content":[]}`, role)
					applyMessageContent(&m, textParts)
					msgsStr, _ = sjson.SetRaw(msgsStr, "-1", m)
				}
			} else if content.Exists() && content.Type == gjson.String {
				m := fmt.Sprintf(`{"role":%q,"content":%q}`, role, content.String())
				msgsStr, _ = sjson.SetRaw(msgsStr, "-1", m)
			}
			return true
		})
	}

	if gjson.Parse(msgsStr).IsArray() && len(gjson.Parse(msgsStr).Array()) > 0 {
		out, _ = sjson.SetRaw(out, "messages", msgsStr)
	}

	// tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		arr := "[]"
		tools.ForEach(func(_, t gjson.Result) bool {
			name := strings.TrimSpace(t.Get("name").String())
			if name == "" {
				return true
			}
			ot := `{"type":"function","function":{"name":"","description":""}}`
			ot, _ = sjson.Set(ot, "function.name", name)
			ot, _ = sjson.Set(ot, "function.description", t.Get("description").String())
			ot, _ = sjson.SetRaw(ot, "function.parameters", normalizeToolParameters(t.Get("input_schema").Raw))
			ot, _ = sjson.Set(ot, "function.strict", true)
			arr, _ = sjson.SetRaw(arr, "-1", ot)
			return true
		})
		if gjson.Parse(arr).IsArray() && len(gjson.Parse(arr).Array()) > 0 {
			out, _ = sjson.SetRaw(out, "tools", arr)
		}
	}

	// tool_choice
	if tc := root.Get("tool_choice"); tc.Exists() {
		switch tc.Get("type").String() {
		case "auto":
			out, _ = sjson.Set(out, "tool_choice", "auto")
		case "any":
			out, _ = sjson.Set(out, "tool_choice", "required")
		case "tool":
			tcj := `{"type":"function","function":{"name":""}}`
			tcj, _ = sjson.Set(tcj, "function.name", tc.Get("name").String())
			out, _ = sjson.SetRaw(out, "tool_choice", tcj)
		}
	}

	return []byte(out)
}

// ---------------------------------------------------------------------------
// Response translation: OpenAI -> Claude (non-streaming)
// ---------------------------------------------------------------------------

func openaiResponseToClaude(raw []byte) []byte {
	root := gjson.ParseBytes(raw)
	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	out, _ = sjson.Set(out, "id", root.Get("id").String())
	out, _ = sjson.Set(out, "model", root.Get("model").String())

	hasTool := false
	stopSet := false

	if choices := root.Get("choices"); choices.Exists() && len(choices.Array()) > 0 {
		ch := choices.Array()[0]

		if fr := ch.Get("finish_reason"); fr.Exists() {
			out, _ = sjson.Set(out, "stop_reason", mapFinishReason(fr.String(), false))
			stopSet = true
		}

		msg := ch.Get("message")

		// reasoning (support both fields for compatibility)
		reasoning := msg.Get("reasoning_content")
		if !reasoning.Exists() {
			reasoning = msg.Get("reasoning")
		}
		for _, t := range collectTexts(reasoning) {
			if t == "" {
				continue
			}
			block, _ := sjson.Set(`{"type":"thinking","thinking":""}`, "thinking", t)
			out, _ = sjson.SetRaw(out, "content.-1", block)
		}

		// text content
		content := msg.Get("content")
		if content.Exists() {
			if content.IsArray() {
				for _, item := range content.Array() {
					switch item.Get("type").String() {
					case "text":
						block, _ := sjson.Set(`{"type":"text","text":""}`, "text", item.Get("text").String())
						out, _ = sjson.SetRaw(out, "content.-1", block)
					case "tool_calls":
						for _, tc := range item.Get("tool_calls").Array() {
							hasTool = true
							out, _ = sjson.SetRaw(out, "content.-1", toolCallToClaudeBlock(tc))
						}
					}
				}
			} else if content.Type == gjson.String && content.String() != "" {
				block, _ := sjson.Set(`{"type":"text","text":""}`, "text", content.String())
				out, _ = sjson.SetRaw(out, "content.-1", block)
			}
		}

		// tool_calls at message level
		if tcs := msg.Get("tool_calls"); tcs.Exists() && tcs.IsArray() {
			tcs.ForEach(func(_, tc gjson.Result) bool {
				hasTool = true
				out, _ = sjson.SetRaw(out, "content.-1", toolCallToClaudeBlock(tc))
				return true
			})
		}
	}

	if hasTool && stopSet {
		out, _ = sjson.Set(out, "stop_reason", "tool_use")
	}

	if usage := root.Get("usage"); usage.Exists() {
		in := usage.Get("prompt_tokens").Int()
		out2 := usage.Get("completion_tokens").Int()
		out, _ = sjson.Set(out, "usage.input_tokens", in)
		out, _ = sjson.Set(out, "usage.output_tokens", out2)
	}

	if !stopSet {
		if hasTool {
			out, _ = sjson.Set(out, "stop_reason", "tool_use")
		} else {
			out, _ = sjson.Set(out, "stop_reason", "end_turn")
		}
	}

	return []byte(out)
}

// ---------------------------------------------------------------------------
// Response translation: OpenAI -> Claude (streaming, stateful)
// ---------------------------------------------------------------------------

type streamConverter struct {
	msgID string
	model string

	started bool // message_start sent

	thinkOn  bool
	thinkIdx int

	textOn  bool
	textIdx int

	tools   map[int]*toolCallState
	nextIdx int

	blocksClosed bool
	finish       string
	hasTool      bool
	deltaSent    bool
	stopSent     bool
}

type toolCallState struct {
	id       string
	name     string
	args     strings.Builder
	blockIdx int
	started  bool
	closed   bool
	dropped  bool
}

func newStreamConverter() *streamConverter {
	return &streamConverter{
		tools:    make(map[int]*toolCallState),
		thinkIdx: -1,
		textIdx:  -1,
	}
}

func (c *streamConverter) convert(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return c.finalize()
	}

	root := gjson.ParseBytes(trimmed)
	var buf []byte

	if c.msgID == "" {
		c.msgID = root.Get("id").String()
	}
	if c.model == "" {
		c.model = root.Get("model").String()
	}

	delta := root.Get("choices.0.delta")

	// message_start
	if delta.Exists() && !c.started {
		buf = append(buf, c.msgStart()...)
		c.started = true
	}

	// reasoning (support both reasoning_content and reasoning fields)
	reasoning := delta.Get("reasoning_content")
	if !reasoning.Exists() {
		reasoning = delta.Get("reasoning")
	}
	if reasoning.Exists() && reasoning.String() != "" {
		c.closeTextBlock(&buf)
		if !c.thinkOn {
			c.thinkIdx = c.nextIdx
			c.nextIdx++
			c.thinkOn = true
			buf = append(buf, c.blockStart(c.thinkIdx, "thinking", "")...)
		}
		buf = append(buf, c.thinkingDelta(c.thinkIdx, reasoning.String())...)
	}

	// content
	content := delta.Get("content")
	if content.Exists() && content.Type != gjson.Null && content.String() != "" {
		c.closeThinkBlock(&buf)
		if !c.textOn {
			c.textIdx = c.nextIdx
			c.nextIdx++
			c.textOn = true
			buf = append(buf, c.blockStart(c.textIdx, "text", "")...)
		}
		buf = append(buf, c.textDelta(c.textIdx, content.String())...)
	}

	// tool_calls
	if tcs := delta.Get("tool_calls"); tcs.Exists() && tcs.IsArray() {
		tcs.ForEach(func(_, tc gjson.Result) bool {
			idx := int(tc.Get("index").Int())
			if _, ok := c.tools[idx]; !ok {
				c.tools[idx] = &toolCallState{}
			}
			acc := c.tools[idx]

			if id := tc.Get("id"); id.Exists() {
				acc.id = id.String()
			}
			if fn := tc.Get("function"); fn.Exists() {
				if name := fn.Get("name"); name.Exists() {
					acc.name = strings.TrimSpace(name.String())
				}
				if !acc.started && acc.name != "" {
					c.hasTool = true
					c.closeThinkBlock(&buf)
					c.closeTextBlock(&buf)
					acc.blockIdx = c.nextIdx
					c.nextIdx++
					acc.started = true
					buf = append(buf, c.toolStart(acc.blockIdx, acc.id, acc.name)...)
					if acc.args.Len() > 0 {
						buf = append(buf, c.toolArgsDelta(acc.blockIdx, acc.args.String())...)
					}
				}
				if args := fn.Get("arguments"); args.Exists() && args.String() != "" {
					acc.args.WriteString(args.String())
					if acc.started {
						buf = append(buf, c.toolArgsDelta(acc.blockIdx, args.String())...)
					}
				}
			}
			return true
		})
	}

	// finish_reason
	if fr := root.Get("choices.0.finish_reason"); fr.Exists() && fr.String() != "" {
		c.finish = fr.String()
		buf = append(buf, c.closeAllBlocks()...)
	}

	// usage (send message_delta once we have both finish_reason and usage)
	if c.finish != "" && !c.deltaSent {
		usage := root.Get("usage")
		if usage.Exists() && usage.Type != gjson.Null {
			in := usage.Get("prompt_tokens").Int()
			out2 := usage.Get("completion_tokens").Int()
			buf = append(buf, c.msgDelta(in, out2)...)
			c.deltaSent = true
		}
	}

	return buf
}

func (c *streamConverter) finalize() []byte {
	var buf []byte
	buf = append(buf, c.closeAllBlocks()...)
	if c.finish != "" && !c.deltaSent {
		buf = append(buf, c.msgDelta(0, 0)...)
		c.deltaSent = true
	}
	if !c.stopSent {
		buf = append(buf, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")...)
		c.stopSent = true
	}
	return buf
}

// -- helper emitters --

func (c *streamConverter) msgStart() []byte {
	j := fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		c.msgID, c.model)
	return []byte("event: message_start\ndata: " + j + "\n\n")
}

func (c *streamConverter) blockStart(idx int, typ, text string) []byte {
	var j string
	if typ == "thinking" {
		j = fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, idx)
	} else {
		j = fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, idx)
	}
	return []byte("event: content_block_start\ndata: " + j + "\n\n")
}

func (c *streamConverter) thinkingDelta(idx int, text string) []byte {
	j := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%q}}`, idx, text)
	return []byte("event: content_block_delta\ndata: " + j + "\n\n")
}

func (c *streamConverter) textDelta(idx int, text string) []byte {
	j := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, idx, text)
	return []byte("event: content_block_delta\ndata: " + j + "\n\n")
}

func (c *streamConverter) toolStart(idx int, id, name string) []byte {
	j := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, idx, id, name)
	return []byte("event: content_block_start\ndata: " + j + "\n\n")
}

func (c *streamConverter) toolArgsDelta(idx int, partialJSON string) []byte {
	j := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, idx, partialJSON)
	return []byte("event: content_block_delta\ndata: " + j + "\n\n")
}

func (c *streamConverter) blockStop(idx int) []byte {
	j := fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx)
	return []byte("event: content_block_stop\ndata: " + j + "\n\n")
}

func (c *streamConverter) msgDelta(inputTokens, outputTokens int64) []byte {
	j := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		mapFinishReason(c.finish, c.hasTool), inputTokens, outputTokens)
	return []byte("event: message_delta\ndata: " + j + "\n\n")
}

// -- block management --

func (c *streamConverter) closeThinkBlock(buf *[]byte) {
	if !c.thinkOn {
		return
	}
	*buf = append(*buf, c.blockStop(c.thinkIdx)...)
	c.thinkOn = false
	c.thinkIdx = -1
}

func (c *streamConverter) closeTextBlock(buf *[]byte) {
	if !c.textOn {
		return
	}
	*buf = append(*buf, c.blockStop(c.textIdx)...)
	c.textOn = false
	c.textIdx = -1
}

func (c *streamConverter) closeAllBlocks() []byte {
	var buf []byte
	c.closeThinkBlock(&buf)
	c.closeTextBlock(&buf)

	if !c.blocksClosed {
		for idx, acc := range c.tools {
			if !acc.started || acc.closed {
				delete(c.tools, idx)
				continue
			}
			buf = append(buf, c.blockStop(acc.blockIdx)...)
			acc.closed = true
			delete(c.tools, idx)
		}
		c.blocksClosed = true
	}
	return buf
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func mapFinishReason(reason string, hasTool bool) string {
	if hasTool {
		switch reason {
		case "stop", "tool_calls", "function_call":
			return "tool_use"
		}
	}
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func mapClaudeThinkingToReasoningEffort(thinking gjson.Result) string {
	switch strings.TrimSpace(thinking.Get("type").String()) {
	case "disabled":
		return "low"
	case "adaptive":
		return "medium"
	case "enabled":
		budget := thinking.Get("budget_tokens").Int()
		switch {
		case budget <= 0:
			return "high"
		case budget <= 2048:
			return "low"
		case budget <= 8192:
			return "medium"
		default:
			return "high"
		}
	default:
		if thinking.Get("budget_tokens").Exists() {
			return "medium"
		}
		return ""
	}
}

func extractClaudeThinkingText(part gjson.Result) string {
	switch strings.TrimSpace(part.Get("type").String()) {
	case "thinking":
		return part.Get("thinking").String()
	case "redacted_thinking":
		data := strings.TrimSpace(part.Get("data").String())
		if data == "" {
			return "[redacted thinking]"
		}
		return fmt.Sprintf("[redacted thinking %d bytes]", len(data))
	default:
		return ""
	}
}

func applyMessageContent(msg *string, parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	if joined, ok := joinTextParts(parts); ok {
		*msg, _ = sjson.Set(*msg, "content", joined)
		return true
	}
	arr := "[]"
	for _, pp := range parts {
		arr, _ = sjson.SetRaw(arr, "-1", pp)
	}
	*msg, _ = sjson.SetRaw(*msg, "content", arr)
	return true
}

func joinTextParts(parts []string) (string, bool) {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		parsed := gjson.Parse(part)
		if parsed.Get("type").String() != "text" {
			return "", false
		}
		texts = append(texts, parsed.Get("text").String())
	}
	return strings.Join(texts, "\n\n"), true
}

func collectTexts(node gjson.Result) []string {
	var out []string
	if !node.Exists() {
		return out
	}
	if node.IsArray() {
		node.ForEach(func(_, v gjson.Result) bool {
			out = append(out, collectTexts(v)...)
			return true
		})
		return out
	}
	if node.Type == gjson.String && node.String() != "" {
		return []string{node.String()}
	}
	if node.IsObject() {
		if t := node.Get("text"); t.Exists() && t.Type == gjson.String {
			return []string{t.String()}
		}
	}
	return out
}
func convertContentPart(part gjson.Result) (string, bool) {
	switch part.Get("type").String() {
	case "text":
		t := part.Get("text").String()
		if strings.TrimSpace(t) == "" {
			return "", false
		}
		j, _ := sjson.Set(`{"type":"text","text":""}`, "text", t)
		return j, true
	case "image":
		var url string
		if src := part.Get("source"); src.Exists() {
			switch src.Get("type").String() {
			case "base64":
				mt := src.Get("media_type").String()
				if mt == "" {
					mt = "application/octet-stream"
				}
				url = "data:" + mt + ";base64," + src.Get("data").String()
			case "url":
				url = src.Get("url").String()
			}
		}
		if url == "" {
			url = part.Get("url").String()
		}
		if url == "" {
			return "", false
		}
		j, _ := sjson.Set(`{"type":"image_url","image_url":{"url":""}}`, "image_url.url", url)
		return j, true
	default:
		return "", false
	}
}

func toolResultContentToString(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		textOnly := true
		var parts []string
		content.ForEach(func(_, item gjson.Result) bool {
			switch {
			case item.Type == gjson.String:
				parts = append(parts, item.String())
			case isPlainTextToolResultItem(item):
				parts = append(parts, item.Get("text").String())
			default:
				textOnly = false
			}
			return true
		})
		if textOnly {
			return strings.Join(parts, "\n\n")
		}
		return content.Raw
	}
	if content.IsObject() {
		if isPlainTextToolResultItem(content) {
			return content.Get("text").String()
		}
		return content.Raw
	}
	return content.Raw
}

func isPlainTextToolResultItem(item gjson.Result) bool {
	if !item.IsObject() || !item.Get("text").Exists() {
		return false
	}
	typ := strings.TrimSpace(item.Get("type").String())
	return typ == "" || typ == "text"
}

func toolCallToClaudeBlock(tc gjson.Result) string {
	id := tc.Get("id").String()
	name := strings.TrimSpace(tc.Get("function.name").String())
	argsRaw := tc.Get("function.arguments").String()
	block := fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, id, name)
	if parsed, ok := parseToolArgumentsObject(argsRaw); ok {
		parsed = normalizeClaudeToolArguments(name, parsed)
		block, _ = sjson.SetRaw(block, "input", gjson.Parse(parsed).Raw)
	}
	return block
}

func normalizeClaudeToolArguments(toolName, raw string) string {
	switch strings.TrimSpace(toolName) {
	case "Edit":
		return normalizeEditToolArguments(raw)
	default:
		return raw
	}
}

func normalizeEditToolArguments(raw string) string {
	if strings.TrimSpace(raw) == "" || !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return raw
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return raw
	}

	renameArgumentAlias(data, "filePath", "file_path")
	renameArgumentAlias(data, "path", "file_path")
	renameArgumentAlias(data, "oldString", "old_string")
	renameArgumentAlias(data, "old_text", "old_string")
	renameArgumentAlias(data, "newString", "new_string")
	renameArgumentAlias(data, "new_text", "new_string")
	renameArgumentAlias(data, "replaceAll", "replace_all")

	normalizeStringArgument(data, "file_path")
	normalizeStringArgument(data, "old_string")
	normalizeStringArgument(data, "new_string")
	normalizeOptionalArgument(data, "replace_all")
	normalizeBooleanArgument(data, "replace_all")

	normalized, err := json.Marshal(data)
	if err != nil {
		return raw
	}
	return string(normalized)
}

func renameArgumentAlias(data map[string]any, alias, canonical string) {
	if _, ok := data[canonical]; ok {
		return
	}
	if value, ok := data[alias]; ok {
		data[canonical] = value
		delete(data, alias)
	}
}

func normalizeOptionalArgument(data map[string]any, key string) {
	value, ok := data[key]
	if !ok {
		return
	}
	switch v := value.(type) {
	case nil:
		delete(data, key)
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" || strings.EqualFold(trimmed, "null") || strings.EqualFold(trimmed, "undefined") {
			delete(data, key)
		}
	}
}

func normalizeStringArgument(data map[string]any, key string) {
	value, ok := data[key]
	if !ok {
		return
	}
	switch v := value.(type) {
	case string:
		return
	case nil:
		delete(data, key)
	case bool:
		if v {
			data[key] = "true"
		} else {
			data[key] = "false"
		}
	case float64:
		data[key] = fmt.Sprintf("%.0f", v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			delete(data, key)
			return
		}
		data[key] = string(encoded)
	}
}

func normalizeBooleanArgument(data map[string]any, key string) {
	value, ok := data[key]
	if !ok {
		return
	}
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		switch {
		case strings.EqualFold(trimmed, "true"):
			data[key] = true
		case strings.EqualFold(trimmed, "false"):
			data[key] = false
		}
	}
}

func normalizeToolParameters(raw string) string {
	if strings.TrimSpace(raw) == "" || !gjson.Valid(raw) {
		return `{"type":"object","properties":{},"additionalProperties":false}`
	}
	schema := raw
	parsed := gjson.Parse(raw)
	if !parsed.IsObject() {
		return `{"type":"object","properties":{},"additionalProperties":false}`
	}
	return normalizeSchemaObject(schema)
}

// fixJSON is a best-effort fix for truncated JSON (e.g., from streaming).
func fixJSON(s string) string {
	if parsed, ok := parseToolArgumentsObject(s); ok {
		return parsed
	}
	return "{}"
}

func parseToolArgumentsObject(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if parsed := parseTaggedToolArguments(raw); parsed != "" {
		return parsed, true
	}
	if gjson.Valid(raw) && gjson.Parse(raw).IsObject() {
		return raw, true
	}
	if obj, ok := extractFirstJSONObject(raw); ok && gjson.Valid(obj) && gjson.Parse(obj).IsObject() {
		return obj, true
	}
	if obj, ok := repairDuplicatedJSONObject(raw); ok && gjson.Valid(obj) && gjson.Parse(obj).IsObject() {
		return obj, true
	}

	candidates := []string{}
	if looksLikeJSONObjectBody(raw) {
		candidates = append(candidates, "{"+raw+"}")
	}
	if candidate, ok := closeJSONObject(raw); ok {
		candidates = append(candidates, candidate)
	}

	for _, candidate := range candidates {
		candidate = trimDanglingCommas(candidate)
		if gjson.Valid(candidate) && gjson.Parse(candidate).IsObject() {
			return candidate, true
		}
	}

	return "", false
}

func looksLikeJSONObjectBody(raw string) bool {
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		return false
	}
	if !strings.Contains(raw, ":") {
		return false
	}
	if strings.Contains(raw, "\n") && len(raw) > 512 {
		return false
	}
	return true
}

func closeJSONObject(s string) (string, bool) {
	const maxAutoCloseDepth = 16
	const maxAutoCloseInputLen = 4096
	if len(s) > maxAutoCloseInputLen {
		return "", false
	}
	openBraces := strings.Count(s, "{") - strings.Count(s, "}")
	openBrackets := strings.Count(s, "[") - strings.Count(s, "]")
	if openBraces < 0 || openBrackets < 0 {
		return "", false
	}
	if openBraces == 0 && openBrackets == 0 {
		return s, true
	}
	if openBraces > maxAutoCloseDepth || openBrackets > maxAutoCloseDepth {
		return "", false
	}
	for i := 0; i < openBrackets; i++ {
		s += "]"
	}
	for i := 0; i < openBraces; i++ {
		s += "}"
	}
	return s, true
}

func trimDanglingCommas(s string) string {
	replacer := strings.NewReplacer(",}", "}", ",]", "]")
	for {
		next := replacer.Replace(s)
		if next == s {
			return s
		}
		s = next
	}
}

func normalizeSchemaObject(schema string) string {
	schema, _ = sjson.Delete(schema, "$schema")

	parsed := gjson.Parse(schema)
	if !parsed.IsObject() {
		return `{"type":"object","properties":{},"additionalProperties":false}`
	}
	if !parsed.Get("type").Exists() {
		schema, _ = sjson.Set(schema, "type", "object")
	}

	switch gjson.Get(schema, "type").String() {
	case "object":
		if !gjson.Get(schema, "properties").Exists() {
			schema, _ = sjson.SetRaw(schema, "properties", `{}`)
		}
		if !gjson.Get(schema, "additionalProperties").Exists() {
			schema, _ = sjson.Set(schema, "additionalProperties", false)
		}
		props := gjson.Get(schema, "properties")
		if props.Exists() && props.IsObject() {
			props.ForEach(func(key, value gjson.Result) bool {
				if value.IsObject() {
					nested := normalizeSchemaObject(value.Raw)
					schema, _ = sjson.SetRaw(schema, "properties."+key.String(), nested)
				}
				return true
			})
		}
	case "array":
		items := gjson.Get(schema, "items")
		if items.Exists() && items.IsObject() {
			schema, _ = sjson.SetRaw(schema, "items", normalizeSchemaObject(items.Raw))
		}
	}

	return schema
}

var taggedToolArgPattern = regexp.MustCompile(`(?s)<arg_key>\s*(.*?)\s*</arg_key>\s*<arg_value>\s*(.*?)\s*</arg_value>`)

func parseTaggedToolArguments(s string) string {
	if !strings.Contains(s, "<arg_key>") || !strings.Contains(s, "<arg_value>") {
		return ""
	}

	out := "{}"
	for _, match := range taggedToolArgPattern.FindAllStringSubmatch(s, -1) {
		if len(match) != 3 {
			continue
		}
		key := strings.TrimSpace(match[1])
		val := strings.TrimSpace(match[2])
		if key == "" {
			continue
		}
		out, _ = sjson.Set(out, key, val)
	}
	if out == "{}" {
		return ""
	}
	return out
}
