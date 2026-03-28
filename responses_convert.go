package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertResponsesRequestToChatCompletions(modelName string, inputRawJSON []byte, stream bool) []byte {
	root := gjson.ParseBytes(inputRawJSON)
	out := `{"model":"","messages":[],"stream":false}`

	out, _ = sjson.Set(out, "model", modelName)
	out, _ = sjson.Set(out, "stream", stream)

	if v := root.Get("max_output_tokens"); v.Exists() {
		out, _ = sjson.Set(out, "max_tokens", v.Int())
	}
	if v := root.Get("parallel_tool_calls"); v.Exists() {
		out, _ = sjson.Set(out, "parallel_tool_calls", v.Bool())
	}
	if v := root.Get("instructions"); v.Exists() {
		msg := `{"role":"system","content":""}`
		msg, _ = sjson.Set(msg, "content", v.String())
		out, _ = sjson.SetRaw(out, "messages.-1", msg)
	}
	if v := root.Get("reasoning.effort"); v.Exists() {
		effort := strings.ToLower(strings.TrimSpace(v.String()))
		if effort != "" {
			out, _ = sjson.Set(out, "reasoning_effort", effort)
		}
	}

	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}

			switch itemType {
			case "message", "":
				role := item.Get("role").String()
				if role == "developer" {
					role = "user"
				}
				message := `{"role":"","content":[]}`
				message, _ = sjson.Set(message, "role", role)

				content := item.Get("content")
				if content.Exists() && content.IsArray() {
					content.ForEach(func(_, contentItem gjson.Result) bool {
						contentType := contentItem.Get("type").String()
						if contentType == "" {
							contentType = "input_text"
						}
						switch contentType {
						case "input_text", "output_text":
							part := `{"type":"text","text":""}`
							part, _ = sjson.Set(part, "text", contentItem.Get("text").String())
							message, _ = sjson.SetRaw(message, "content.-1", part)
						case "input_image":
							part := `{"type":"image_url","image_url":{"url":""}}`
							part, _ = sjson.Set(part, "image_url.url", contentItem.Get("image_url").String())
							message, _ = sjson.SetRaw(message, "content.-1", part)
						}
						return true
					})
				} else if content.Type == gjson.String {
					message, _ = sjson.Set(message, "content", content.String())
				}

				out, _ = sjson.SetRaw(out, "messages.-1", message)

			case "function_call":
				message := `{"role":"assistant","tool_calls":[]}`
				toolCall := `{"id":"","type":"function","function":{"name":"","arguments":""}}`
				if v := item.Get("call_id"); v.Exists() {
					toolCall, _ = sjson.Set(toolCall, "id", v.String())
				}
				if v := item.Get("name"); v.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.name", v.String())
				}
				if v := item.Get("arguments"); v.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.arguments", sanitizeFunctionArguments(v.String()))
				}
				message, _ = sjson.SetRaw(message, "tool_calls.0", toolCall)
				out, _ = sjson.SetRaw(out, "messages.-1", message)

			case "function_call_output":
				message := `{"role":"tool","tool_call_id":"","content":""}`
				if v := item.Get("call_id"); v.Exists() {
					message, _ = sjson.Set(message, "tool_call_id", v.String())
				}
				if v := item.Get("output"); v.Exists() {
					message, _ = sjson.Set(message, "content", v.String())
				}
				out, _ = sjson.SetRaw(out, "messages.-1", message)
			}
			return true
		})
	} else if input.Type == gjson.String {
		msg := `{"role":"user","content":""}`
		msg, _ = sjson.Set(msg, "content", input.String())
		out, _ = sjson.SetRaw(out, "messages.-1", msg)
	}

	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		var chatTools []interface{}
		tools.ForEach(func(_, tool gjson.Result) bool {
			toolType := tool.Get("type").String()
			if toolType != "" && toolType != "function" {
				return true
			}

			name := strings.TrimSpace(firstNonEmpty(tool.Get("name").String(), tool.Get("function.name").String()))
			if name == "" {
				return true
			}

			chatTool := `{"type":"function","function":{"name":"","description":"","parameters":{}}}`
			chatTool, _ = sjson.Set(chatTool, "function.name", name)
			if v := firstNonEmpty(tool.Get("description").String(), tool.Get("function.description").String()); v != "" {
				chatTool, _ = sjson.Set(chatTool, "function.description", v)
			}
			parameters := tool.Get("parameters")
			if !parameters.Exists() {
				parameters = tool.Get("input_schema")
			}
			if parameters.Exists() {
				chatTool, _ = sjson.SetRaw(chatTool, "function.parameters", parameters.Raw)
			}
			chatTools = append(chatTools, gjson.Parse(chatTool).Value())
			return true
		})
		if len(chatTools) > 0 {
			out, _ = sjson.Set(out, "tools", chatTools)
		}
	}

	if v := root.Get("tool_choice"); v.Exists() {
		if v.Type == gjson.String {
			out, _ = sjson.Set(out, "tool_choice", v.String())
		} else {
			out, _ = sjson.SetRaw(out, "tool_choice", v.Raw)
		}
	}

	return []byte(out)
}

func sanitizeFunctionArguments(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}"
	}
	if gjson.Valid(raw) {
		return raw
	}
	if obj, ok := extractFirstJSONObject(raw); ok && gjson.Valid(obj) {
		return obj
	}
	if obj, ok := repairDuplicatedJSONObject(raw); ok && gjson.Valid(obj) {
		return obj
	}
	return "{}"
}

func extractFirstJSONObject(raw string) (string, bool) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", false
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(raw); i++ {
		ch := raw[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : i+1], true
			}
		}
	}
	return "", false
}

func repairDuplicatedJSONObject(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "{") {
		return "", false
	}
	second := strings.Index(raw[1:], `{"`)
	if second < 0 {
		return "", false
	}
	candidate := strings.TrimSpace(raw[:second+1])
	if strings.HasSuffix(candidate, "}") {
		return candidate, true
	}
	return candidate + "}", true
}

type responsesStateReasoning struct {
	ID   string
	Text string
}

type responsesToolCallState struct {
	OutputIndex int
	Name        string
	CallID      string
	ArgsBuf     strings.Builder
	ItemAdded   bool
	ItemDone    bool
}

type responsesStreamState struct {
	Seq              int
	ResponseID       string
	Created          int64
	Started          bool
	ReasoningID      string
	ReasoningIndex   int
	MsgTextBuf       map[int]*strings.Builder
	ReasoningBuf     strings.Builder
	Reasonings       []responsesStateReasoning
	ToolCalls        map[string]*responsesToolCallState
	MsgItemAdded     map[int]bool
	MsgContentAdded  map[int]bool
	MsgItemDone      map[int]bool
	PromptTokens     int64
	CachedTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	ReasoningTokens  int64
	UsageSeen        bool
}

var responseIDCounter uint64

func emitResponsesEvent(event, payload string) string {
	return fmt.Sprintf("event: %s\ndata: %s", event, payload)
}

func convertChatCompletionsStreamToResponses(_ context.Context, requestRawJSON, rawJSON []byte, state *any) []string {
	if *state == nil {
		*state = &responsesStreamState{
			ToolCalls:       make(map[string]*responsesToolCallState),
			MsgTextBuf:      make(map[int]*strings.Builder),
			MsgItemAdded:    make(map[int]bool),
			MsgContentAdded: make(map[int]bool),
			MsgItemDone:     make(map[int]bool),
			Reasonings:      make([]responsesStateReasoning, 0),
		}
	}
	st := (*state).(*responsesStreamState)

	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}
	rawJSON = bytes.TrimSpace(rawJSON)
	if len(rawJSON) == 0 || bytes.Equal(rawJSON, []byte("[DONE]")) {
		return nil
	}

	root := gjson.ParseBytes(rawJSON)
	if obj := root.Get("object"); obj.Exists() && obj.String() != "" && obj.String() != "chat.completion.chunk" {
		return nil
	}
	if !root.Get("choices").Exists() || !root.Get("choices").IsArray() {
		return nil
	}

	if usage := root.Get("usage"); usage.Exists() {
		if v := usage.Get("prompt_tokens"); v.Exists() {
			st.PromptTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
			st.CachedTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("completion_tokens"); v.Exists() {
			st.CompletionTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("output_tokens"); v.Exists() {
			st.CompletionTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("output_tokens_details.reasoning_tokens"); v.Exists() {
			st.ReasoningTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("completion_tokens_details.reasoning_tokens"); v.Exists() {
			st.ReasoningTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("total_tokens"); v.Exists() {
			st.TotalTokens = v.Int()
			st.UsageSeen = true
		}
	}

	nextSeq := func() int { st.Seq++; return st.Seq }
	var out []string

	if !st.Started {
		st.ResponseID = root.Get("id").String()
		st.Created = root.Get("created").Int()

		created := `{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`
		created, _ = sjson.Set(created, "sequence_number", nextSeq())
		created, _ = sjson.Set(created, "response.id", st.ResponseID)
		created, _ = sjson.Set(created, "response.created_at", st.Created)
		out = append(out, emitResponsesEvent("response.created", created))

		inProgress := `{"type":"response.in_progress","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress"}}`
		inProgress, _ = sjson.Set(inProgress, "sequence_number", nextSeq())
		inProgress, _ = sjson.Set(inProgress, "response.id", st.ResponseID)
		inProgress, _ = sjson.Set(inProgress, "response.created_at", st.Created)
		out = append(out, emitResponsesEvent("response.in_progress", inProgress))
		st.Started = true
	}

	stopReasoning := func(text string) {
		textDone := `{"type":"response.reasoning_summary_text.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"text":""}`
		textDone, _ = sjson.Set(textDone, "sequence_number", nextSeq())
		textDone, _ = sjson.Set(textDone, "item_id", st.ReasoningID)
		textDone, _ = sjson.Set(textDone, "output_index", st.ReasoningIndex)
		textDone, _ = sjson.Set(textDone, "text", text)
		out = append(out, emitResponsesEvent("response.reasoning_summary_text.done", textDone))

		partDone := `{"type":"response.reasoning_summary_part.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`
		partDone, _ = sjson.Set(partDone, "sequence_number", nextSeq())
		partDone, _ = sjson.Set(partDone, "item_id", st.ReasoningID)
		partDone, _ = sjson.Set(partDone, "output_index", st.ReasoningIndex)
		partDone, _ = sjson.Set(partDone, "part.text", text)
		out = append(out, emitResponsesEvent("response.reasoning_summary_part.done", partDone))

		itemDone := `{"type":"response.output_item.done","item":{"id":"","type":"reasoning","summary":[{"type":"summary_text","text":""}]},"output_index":0,"sequence_number":0}`
		itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
		itemDone, _ = sjson.Set(itemDone, "item.id", st.ReasoningID)
		itemDone, _ = sjson.Set(itemDone, "output_index", st.ReasoningIndex)
		itemDone, _ = sjson.Set(itemDone, "item.summary.0.text", text)
		out = append(out, emitResponsesEvent("response.output_item.done", itemDone))

		st.Reasonings = append(st.Reasonings, responsesStateReasoning{ID: st.ReasoningID, Text: text})
		st.ReasoningID = ""
	}

	getToolCallState := func(choiceIndex, toolIndex int) *responsesToolCallState {
		key := fmt.Sprintf("%d:%d", choiceIndex, toolIndex)
		if existing, ok := st.ToolCalls[key]; ok {
			return existing
		}
		tc := &responsesToolCallState{OutputIndex: choiceIndex + toolIndex + 1}
		st.ToolCalls[key] = tc
		return tc
	}

	root.Get("choices").ForEach(func(_, choice gjson.Result) bool {
		idx := int(choice.Get("index").Int())
		delta := choice.Get("delta")

		if content := delta.Get("content"); content.Exists() && content.String() != "" {
			if st.ReasoningID != "" {
				stopReasoning(st.ReasoningBuf.String())
				st.ReasoningBuf.Reset()
			}
			if !st.MsgItemAdded[idx] {
				item := `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`
				item, _ = sjson.Set(item, "sequence_number", nextSeq())
				item, _ = sjson.Set(item, "output_index", idx)
				item, _ = sjson.Set(item, "item.id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
				out = append(out, emitResponsesEvent("response.output_item.added", item))
				st.MsgItemAdded[idx] = true
			}
			if !st.MsgContentAdded[idx] {
				part := `{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`
				part, _ = sjson.Set(part, "sequence_number", nextSeq())
				part, _ = sjson.Set(part, "item_id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
				part, _ = sjson.Set(part, "output_index", idx)
				out = append(out, emitResponsesEvent("response.content_part.added", part))
				st.MsgContentAdded[idx] = true
			}
			msg := `{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`
			msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
			msg, _ = sjson.Set(msg, "item_id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
			msg, _ = sjson.Set(msg, "output_index", idx)
			msg, _ = sjson.Set(msg, "delta", content.String())
			out = append(out, emitResponsesEvent("response.output_text.delta", msg))
			if st.MsgTextBuf[idx] == nil {
				st.MsgTextBuf[idx] = &strings.Builder{}
			}
			st.MsgTextBuf[idx].WriteString(content.String())
		}

		if rc := delta.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
			if st.ReasoningID == "" {
				st.ReasoningID = fmt.Sprintf("rs_%s_%d", st.ResponseID, idx)
				st.ReasoningIndex = idx
				item := `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","summary":[]}}`
				item, _ = sjson.Set(item, "sequence_number", nextSeq())
				item, _ = sjson.Set(item, "output_index", idx)
				item, _ = sjson.Set(item, "item.id", st.ReasoningID)
				out = append(out, emitResponsesEvent("response.output_item.added", item))

				part := `{"type":"response.reasoning_summary_part.added","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`
				part, _ = sjson.Set(part, "sequence_number", nextSeq())
				part, _ = sjson.Set(part, "item_id", st.ReasoningID)
				part, _ = sjson.Set(part, "output_index", st.ReasoningIndex)
				out = append(out, emitResponsesEvent("response.reasoning_summary_part.added", part))
			}

			st.ReasoningBuf.WriteString(rc.String())
			msg := `{"type":"response.reasoning_summary_text.delta","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"delta":""}`
			msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
			msg, _ = sjson.Set(msg, "item_id", st.ReasoningID)
			msg, _ = sjson.Set(msg, "output_index", st.ReasoningIndex)
			msg, _ = sjson.Set(msg, "delta", rc.String())
			out = append(out, emitResponsesEvent("response.reasoning_summary_text.delta", msg))
		}

		if tcs := delta.Get("tool_calls"); tcs.Exists() && tcs.IsArray() {
			if st.ReasoningID != "" {
				stopReasoning(st.ReasoningBuf.String())
				st.ReasoningBuf.Reset()
			}
			if st.MsgItemAdded[idx] && !st.MsgItemDone[idx] {
				fullText := ""
				if b := st.MsgTextBuf[idx]; b != nil {
					fullText = b.String()
				}
				textDone := `{"type":"response.output_text.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"text":"","logprobs":[]}`
				textDone, _ = sjson.Set(textDone, "sequence_number", nextSeq())
				textDone, _ = sjson.Set(textDone, "item_id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
				textDone, _ = sjson.Set(textDone, "output_index", idx)
				textDone, _ = sjson.Set(textDone, "text", fullText)
				out = append(out, emitResponsesEvent("response.output_text.done", textDone))

				partDone := `{"type":"response.content_part.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`
				partDone, _ = sjson.Set(partDone, "sequence_number", nextSeq())
				partDone, _ = sjson.Set(partDone, "item_id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
				partDone, _ = sjson.Set(partDone, "output_index", idx)
				partDone, _ = sjson.Set(partDone, "part.text", fullText)
				out = append(out, emitResponsesEvent("response.content_part.done", partDone))

				itemDone := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}}`
				itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
				itemDone, _ = sjson.Set(itemDone, "output_index", idx)
				itemDone, _ = sjson.Set(itemDone, "item.id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
				itemDone, _ = sjson.Set(itemDone, "item.content.0.text", fullText)
				out = append(out, emitResponsesEvent("response.output_item.done", itemDone))
				st.MsgItemDone[idx] = true
			}

			tcs.ForEach(func(toolIdx, tc gjson.Result) bool {
				callIndex := int(tc.Get("index").Int())
				if !tc.Get("index").Exists() {
					callIndex = int(toolIdx.Int())
				}
				callState := getToolCallState(idx, callIndex)
				if name := tc.Get("function.name").String(); name != "" {
					callState.Name = name
				}
				if callState.CallID == "" {
					callState.CallID = tc.Get("id").String()
					if callState.CallID == "" {
						callState.CallID = fmt.Sprintf("call_%s_%d_%d", st.ResponseID, idx, callIndex)
					}
				}
				if !callState.ItemAdded {
					item := `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"in_progress","arguments":"","call_id":"","name":""}}`
					item, _ = sjson.Set(item, "sequence_number", nextSeq())
					item, _ = sjson.Set(item, "output_index", callState.OutputIndex)
					item, _ = sjson.Set(item, "item.id", fmt.Sprintf("fc_%s", callState.CallID))
					item, _ = sjson.Set(item, "item.call_id", callState.CallID)
					item, _ = sjson.Set(item, "item.name", callState.Name)
					out = append(out, emitResponsesEvent("response.output_item.added", item))
					callState.ItemAdded = true
				}
				if args := tc.Get("function.arguments"); args.Exists() && args.String() != "" {
					callState.ArgsBuf.WriteString(args.String())
					deltaEvent := `{"type":"response.function_call_arguments.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`
					deltaEvent, _ = sjson.Set(deltaEvent, "sequence_number", nextSeq())
					deltaEvent, _ = sjson.Set(deltaEvent, "item_id", fmt.Sprintf("fc_%s", callState.CallID))
					deltaEvent, _ = sjson.Set(deltaEvent, "output_index", callState.OutputIndex)
					deltaEvent, _ = sjson.Set(deltaEvent, "delta", args.String())
					out = append(out, emitResponsesEvent("response.function_call_arguments.delta", deltaEvent))
				}
				return true
			})
		}

		if fr := choice.Get("finish_reason"); fr.Exists() && fr.String() != "" {
			if st.ReasoningID != "" {
				stopReasoning(st.ReasoningBuf.String())
				st.ReasoningBuf.Reset()
			}
			for _, callState := range st.ToolCalls {
				if callState.CallID == "" || callState.ItemDone {
					continue
				}
				argsDone := `{"type":"response.function_call_arguments.done","sequence_number":0,"item_id":"","output_index":0,"arguments":""}`
				argsDone, _ = sjson.Set(argsDone, "sequence_number", nextSeq())
				argsDone, _ = sjson.Set(argsDone, "item_id", fmt.Sprintf("fc_%s", callState.CallID))
				argsDone, _ = sjson.Set(argsDone, "output_index", callState.OutputIndex)
				argsDone, _ = sjson.Set(argsDone, "arguments", callState.ArgsBuf.String())
				out = append(out, emitResponsesEvent("response.function_call_arguments.done", argsDone))

				itemDone := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}}`
				itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
				itemDone, _ = sjson.Set(itemDone, "output_index", callState.OutputIndex)
				itemDone, _ = sjson.Set(itemDone, "item.id", fmt.Sprintf("fc_%s", callState.CallID))
				itemDone, _ = sjson.Set(itemDone, "item.arguments", callState.ArgsBuf.String())
				itemDone, _ = sjson.Set(itemDone, "item.call_id", callState.CallID)
				itemDone, _ = sjson.Set(itemDone, "item.name", callState.Name)
				out = append(out, emitResponsesEvent("response.output_item.done", itemDone))
				callState.ItemDone = true
			}

			completed := `{"type":"response.completed","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null}}`
			completed, _ = sjson.Set(completed, "sequence_number", nextSeq())
			completed, _ = sjson.Set(completed, "response.id", st.ResponseID)
			completed, _ = sjson.Set(completed, "response.created_at", st.Created)
			if len(requestRawJSON) > 0 {
				req := gjson.ParseBytes(requestRawJSON)
				for _, key := range []string{"instructions", "model", "previous_response_id", "prompt_cache_key", "safety_identifier", "service_tier", "truncation"} {
					if v := req.Get(key); v.Exists() {
						completed, _ = sjson.Set(completed, "response."+key, v.Value())
					}
				}
				for _, key := range []string{"parallel_tool_calls", "store"} {
					if v := req.Get(key); v.Exists() {
						completed, _ = sjson.Set(completed, "response."+key, v.Bool())
					}
				}
				for _, key := range []string{"max_output_tokens", "max_tool_calls", "top_logprobs"} {
					if v := req.Get(key); v.Exists() {
						completed, _ = sjson.Set(completed, "response."+key, v.Int())
					}
				}
				for _, key := range []string{"temperature", "top_p"} {
					if v := req.Get(key); v.Exists() {
						completed, _ = sjson.Set(completed, "response."+key, v.Float())
					}
				}
				for _, key := range []string{"reasoning", "text", "tool_choice", "tools", "user", "metadata"} {
					if v := req.Get(key); v.Exists() {
						completed, _ = sjson.Set(completed, "response."+key, v.Value())
					}
				}
			}
			outputs := `{"arr":[]}`
			for _, reasoning := range st.Reasonings {
				item := `{"id":"","type":"reasoning","summary":[{"type":"summary_text","text":""}]}`
				item, _ = sjson.Set(item, "id", reasoning.ID)
				item, _ = sjson.Set(item, "summary.0.text", reasoning.Text)
				outputs, _ = sjson.SetRaw(outputs, "arr.-1", item)
			}
			for idx, added := range st.MsgItemAdded {
				if !added {
					continue
				}
				text := ""
				if b := st.MsgTextBuf[idx]; b != nil {
					text = b.String()
				}
				item := `{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`
				item, _ = sjson.Set(item, "id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
				item, _ = sjson.Set(item, "content.0.text", text)
				outputs, _ = sjson.SetRaw(outputs, "arr.-1", item)
			}
			for _, callState := range st.ToolCalls {
				item := `{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`
				item, _ = sjson.Set(item, "id", fmt.Sprintf("fc_%s", callState.CallID))
				item, _ = sjson.Set(item, "arguments", callState.ArgsBuf.String())
				item, _ = sjson.Set(item, "call_id", callState.CallID)
				item, _ = sjson.Set(item, "name", callState.Name)
				outputs, _ = sjson.SetRaw(outputs, "arr.-1", item)
			}
			if gjson.Get(outputs, "arr.#").Int() > 0 {
				completed, _ = sjson.SetRaw(completed, "response.output", gjson.Get(outputs, "arr").Raw)
			}
			if st.UsageSeen {
				completed, _ = sjson.Set(completed, "response.usage.input_tokens", st.PromptTokens)
				completed, _ = sjson.Set(completed, "response.usage.input_tokens_details.cached_tokens", st.CachedTokens)
				completed, _ = sjson.Set(completed, "response.usage.output_tokens", st.CompletionTokens)
				if st.ReasoningTokens > 0 {
					completed, _ = sjson.Set(completed, "response.usage.output_tokens_details.reasoning_tokens", st.ReasoningTokens)
				}
				total := st.TotalTokens
				if total == 0 {
					total = st.PromptTokens + st.CompletionTokens
				}
				completed, _ = sjson.Set(completed, "response.usage.total_tokens", total)
			}
			out = append(out, emitResponsesEvent("response.completed", completed))
		}

		return true
	})

	return out
}

func convertChatCompletionsResponseToResponses(_ context.Context, requestRawJSON, rawJSON []byte) string {
	root := gjson.ParseBytes(rawJSON)
	resp := `{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"incomplete_details":null}`

	id := root.Get("id").String()
	if id == "" {
		id = fmt.Sprintf("resp_%x_%d", time.Now().UnixNano(), atomic.AddUint64(&responseIDCounter, 1))
	}
	resp, _ = sjson.Set(resp, "id", id)

	created := root.Get("created").Int()
	if created == 0 {
		created = time.Now().Unix()
	}
	resp, _ = sjson.Set(resp, "created_at", created)

	if len(requestRawJSON) > 0 {
		req := gjson.ParseBytes(requestRawJSON)
		for _, key := range []string{"instructions", "model", "previous_response_id", "prompt_cache_key", "safety_identifier", "service_tier", "truncation"} {
			if v := req.Get(key); v.Exists() {
				resp, _ = sjson.Set(resp, key, v.Value())
			}
		}
		for _, key := range []string{"parallel_tool_calls", "store"} {
			if v := req.Get(key); v.Exists() {
				resp, _ = sjson.Set(resp, key, v.Bool())
			}
		}
		for _, key := range []string{"max_output_tokens", "max_tool_calls", "top_logprobs"} {
			if v := req.Get(key); v.Exists() {
				resp, _ = sjson.Set(resp, key, v.Int())
			}
		}
		for _, key := range []string{"temperature", "top_p"} {
			if v := req.Get(key); v.Exists() {
				resp, _ = sjson.Set(resp, key, v.Float())
			}
		}
		for _, key := range []string{"reasoning", "text", "tool_choice", "tools", "user", "metadata"} {
			if v := req.Get(key); v.Exists() {
				resp, _ = sjson.Set(resp, key, v.Value())
			}
		}
	}

	outputs := `{"arr":[]}`
	rcText := root.Get("choices.0.message.reasoning_content").String()
	includeReasoning := rcText != ""
	if !includeReasoning && len(requestRawJSON) > 0 {
		includeReasoning = gjson.GetBytes(requestRawJSON, "reasoning").Exists()
	}
	if includeReasoning {
		reasoningItem := `{"id":"","type":"reasoning","encrypted_content":"","summary":[]}`
		reasoningItem, _ = sjson.Set(reasoningItem, "id", fmt.Sprintf("rs_%s", strings.TrimPrefix(id, "resp_")))
		if rcText != "" {
			reasoningItem, _ = sjson.Set(reasoningItem, "summary.0.type", "summary_text")
			reasoningItem, _ = sjson.Set(reasoningItem, "summary.0.text", rcText)
		}
		outputs, _ = sjson.SetRaw(outputs, "arr.-1", reasoningItem)
	}
	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() {
		choices.ForEach(func(_, choice gjson.Result) bool {
			msg := choice.Get("message")
			if !msg.Exists() {
				return true
			}
			if v := msg.Get("content"); v.Exists() && v.String() != "" {
				item := `{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`
				item, _ = sjson.Set(item, "id", fmt.Sprintf("msg_%s_%d", id, int(choice.Get("index").Int())))
				item, _ = sjson.Set(item, "content.0.text", v.String())
				outputs, _ = sjson.SetRaw(outputs, "arr.-1", item)
			}
			if tcs := msg.Get("tool_calls"); tcs.Exists() && tcs.IsArray() {
				tcs.ForEach(func(_, tc gjson.Result) bool {
					item := `{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`
					callID := tc.Get("id").String()
					item, _ = sjson.Set(item, "id", fmt.Sprintf("fc_%s", callID))
					item, _ = sjson.Set(item, "call_id", callID)
					item, _ = sjson.Set(item, "name", tc.Get("function.name").String())
					item, _ = sjson.Set(item, "arguments", tc.Get("function.arguments").String())
					outputs, _ = sjson.SetRaw(outputs, "arr.-1", item)
					return true
				})
			}
			return true
		})
	}
	if gjson.Get(outputs, "arr.#").Int() > 0 {
		resp, _ = sjson.SetRaw(resp, "output", gjson.Get(outputs, "arr").Raw)
	}

	if usage := root.Get("usage"); usage.Exists() {
		resp, _ = sjson.Set(resp, "usage.input_tokens", usage.Get("prompt_tokens").Int())
		if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
			resp, _ = sjson.Set(resp, "usage.input_tokens_details.cached_tokens", v.Int())
		}
		outputTokens := usage.Get("completion_tokens").Int()
		if v := usage.Get("output_tokens"); v.Exists() {
			outputTokens = v.Int()
		}
		resp, _ = sjson.Set(resp, "usage.output_tokens", outputTokens)
		if v := usage.Get("output_tokens_details.reasoning_tokens"); v.Exists() {
			resp, _ = sjson.Set(resp, "usage.output_tokens_details.reasoning_tokens", v.Int())
		} else if v := usage.Get("completion_tokens_details.reasoning_tokens"); v.Exists() {
			resp, _ = sjson.Set(resp, "usage.output_tokens_details.reasoning_tokens", v.Int())
		}
		total := usage.Get("total_tokens").Int()
		if total == 0 {
			total = usage.Get("prompt_tokens").Int() + outputTokens
		}
		resp, _ = sjson.Set(resp, "usage.total_tokens", total)
	}

	return resp
}
