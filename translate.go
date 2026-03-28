package main

import (
	"bytes"
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
		tp := tc.Get("type").String()
		switch tp {
		case "enabled":
			out, _ = sjson.Set(out, "reasoning_effort", "high")
		case "disabled":
			out, _ = sjson.Set(out, "reasoning_effort", "low")
		}
	}

	// system message
	msgsStr := "[]"
	if sys := root.Get("system"); sys.Exists() {
		sysMsg := `{"role":"system","content":[]}`
		has := false
		if sys.Type == gjson.String && sys.String() != "" {
			sysMsg, _ = sjson.SetRaw(sysMsg, "content.-1", fmt.Sprintf(`{"type":"text","text":%q}`, sys.String()))
			has = true
		} else if sys.IsArray() {
			sys.ForEach(func(_, v gjson.Result) bool {
				if item, ok := convertContentPart(v); ok {
					sysMsg, _ = sjson.SetRaw(sysMsg, "content.-1", item)
					has = true
				}
				return true
			})
		}
		if has {
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
					case "thinking":
						if role == "assistant" {
							t := part.Get("thinking").String()
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
							tc := `{"id":"","type":"function","function":{"name":"","arguments":""}}`
							tc, _ = sjson.Set(tc, "id", part.Get("id").String())
							tc, _ = sjson.Set(tc, "function.name", part.Get("name").String())
							if input := part.Get("input"); input.Exists() {
								tc, _ = sjson.Set(tc, "function.arguments", input.Raw)
							}
							toolCalls = append(toolCalls, gjson.Parse(tc).Value())
						}
					case "tool_result":
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
					if hasText {
						arr := "[]"
						for _, pp := range textParts {
							arr, _ = sjson.SetRaw(arr, "-1", pp)
						}
						m, _ = sjson.SetRaw(m, "content", arr)
					} else {
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
					arr := "[]"
					for _, pp := range textParts {
						arr, _ = sjson.SetRaw(arr, "-1", pp)
					}
					m, _ = sjson.SetRaw(m, "content", arr)
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
			ot := `{"type":"function","function":{"name":"","description":""}}`
			ot, _ = sjson.Set(ot, "function.name", t.Get("name").String())
			ot, _ = sjson.Set(ot, "function.description", t.Get("description").String())
			if schema := t.Get("input_schema"); schema.Exists() {
				ot, _ = sjson.Set(ot, "function.parameters", schema.Value())
			}
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
			c.hasTool = true

			if id := tc.Get("id"); id.Exists() {
				acc.id = id.String()
			}
			if fn := tc.Get("function"); fn.Exists() {
				if name := fn.Get("name"); name.Exists() {
					acc.name = name.String()
					c.closeThinkBlock(&buf)
					c.closeTextBlock(&buf)
					acc.blockIdx = c.nextIdx
					c.nextIdx++
					buf = append(buf, c.toolStart(acc.blockIdx, acc.id, acc.name)...)
				}
				if args := fn.Get("arguments"); args.Exists() && args.String() != "" {
					acc.args.WriteString(args.String())
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
			if acc.args.Len() > 0 {
				buf = append(buf, c.toolArgsDelta(acc.blockIdx, fixJSON(acc.args.String()))...)
			}
			buf = append(buf, c.blockStop(acc.blockIdx)...)
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
		var parts []string
		content.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				parts = append(parts, item.String())
			} else if item.IsObject() {
				if item.Get("text").Exists() {
					parts = append(parts, item.Get("text").String())
				} else {
					parts = append(parts, item.Raw)
				}
			} else {
				parts = append(parts, item.Raw)
			}
			return true
		})
		return strings.Join(parts, "\n\n")
	}
	if content.IsObject() {
		if content.Get("text").Exists() {
			return content.Get("text").String()
		}
		return content.Raw
	}
	return content.Raw
}

func toolCallToClaudeBlock(tc gjson.Result) string {
	id := tc.Get("id").String()
	name := tc.Get("function.name").String()
	argsRaw := tc.Get("function.arguments").String()
	block := fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, id, name)
	parsed := fixJSON(argsRaw)
	if parsed != "" && gjson.Valid(parsed) && gjson.Parse(parsed).IsObject() {
		block, _ = sjson.SetRaw(block, "input", gjson.Parse(parsed).Raw)
	}
	return block
}

// fixJSON is a best-effort fix for truncated JSON (e.g., from streaming).
func fixJSON(s string) string {
	if parsed := parseTaggedToolArguments(s); parsed != "" {
		return parsed
	}
	if gjson.Valid(s) {
		return s
	}
	// Try to close open braces/brackets
	openBraces := strings.Count(s, "{") - strings.Count(s, "}")
	openBrackets := strings.Count(s, "[") - strings.Count(s, "]")
	for i := 0; i < openBrackets; i++ {
		s += "]"
	}
	for i := 0; i < openBraces; i++ {
		s += "}"
	}
	if gjson.Valid(s) {
		return s
	}
	return "{}"
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
