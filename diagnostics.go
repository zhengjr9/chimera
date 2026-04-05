package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

type toolDiagnosticLogger struct {
	mu         sync.Mutex
	file       *os.File
	streamSeen map[string]struct{}
}

type toolDiagnosticEntry struct {
	Timestamp    string `json:"timestamp"`
	RequestID    string `json:"request_id,omitempty"`
	Phase        string `json:"phase"`
	Model        string `json:"model,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
	ToolID       string `json:"tool_id,omitempty"`
	ToolUseID    string `json:"tool_use_id,omitempty"`
	Path         string `json:"path,omitempty"`
	Direction    string `json:"direction,omitempty"`
	IsError      bool   `json:"is_error,omitempty"`
	Schema       string `json:"schema,omitempty"`
	RawArguments string `json:"raw_arguments,omitempty"`
	FixedArgs    string `json:"fixed_arguments,omitempty"`
	Content      string `json:"content,omitempty"`
	Error        string `json:"error,omitempty"`
}

func newToolDiagnosticLogger(path string) (*toolDiagnosticLogger, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &toolDiagnosticLogger{
		file:       f,
		streamSeen: make(map[string]struct{}),
	}, nil
}

func (l *toolDiagnosticLogger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

func (l *toolDiagnosticLogger) log(entry toolDiagnosticEntry) {
	if l == nil || l.file == nil {
		return
	}
	entry.Timestamp = time.Now().Format(time.RFC3339Nano)
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.file.Write(append(line, '\n'))
}

func (l *toolDiagnosticLogger) shouldLogStreamChunk(reqID, phase, toolID, toolName string) bool {
	if l == nil || l.file == nil {
		return false
	}
	key := strings.Join([]string{reqID, phase, toolID, toolName}, "|")
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.streamSeen[key]; ok {
		return false
	}
	l.streamSeen[key] = struct{}{}
	return true
}

func (p *Proxy) logToolDiagnosticsFromClaudeRequest(reqID, path string, body []byte) {
	if p.toolDiag == nil {
		return
	}
	root := gjson.ParseBytes(body)
	model := root.Get("model").String()

	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			p.toolDiag.log(toolDiagnosticEntry{
				RequestID: reqID,
				Phase:     "claude_request_tool_schema",
				Model:     model,
				Path:      path,
				Direction: "request",
				ToolName:  tool.Get("name").String(),
				Schema:    tool.Get("input_schema").Raw,
			})
			return true
		})
	}

	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			msg.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() != "tool_result" {
					return true
				}
				p.toolDiag.log(toolDiagnosticEntry{
					RequestID: reqID,
					Phase:     "claude_request_tool_result",
					Model:     model,
					Path:      path,
					Direction: "request",
					ToolUseID: part.Get("tool_use_id").String(),
					IsError:   part.Get("is_error").Bool(),
					Content:   trimForLog(toolResultContentToString(part.Get("content"))),
				})
				return true
			})
			return true
		})
	}
}

func (p *Proxy) logToolDiagnosticsFromOpenAIRequest(reqID, phase, path string, body []byte) {
	if p.toolDiag == nil {
		return
	}
	root := gjson.ParseBytes(body)
	model := root.Get("model").String()

	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			p.toolDiag.log(toolDiagnosticEntry{
				RequestID: reqID,
				Phase:     phase + "_tool_schema",
				Model:     model,
				Path:      path,
				Direction: "request",
				ToolName:  firstNonEmpty(tool.Get("function.name").String(), tool.Get("name").String()),
				Schema:    firstNonEmpty(tool.Get("function.parameters").Raw, tool.Get("parameters").Raw),
				Content:   firstNonEmpty(tool.Get("function.strict").Raw, tool.Get("strict").Raw),
			})
			return true
		})
	}
}

func (p *Proxy) logToolDiagnosticsFromOpenAIResponse(reqID, phase, path string, body []byte) {
	if p.toolDiag == nil {
		return
	}
	root := gjson.ParseBytes(body)
	model := root.Get("model").String()
	if tcs := root.Get("choices.0.message.tool_calls"); tcs.Exists() && tcs.IsArray() {
		tcs.ForEach(func(_, tc gjson.Result) bool {
			rawArgs := tc.Get("function.arguments").String()
			fixed, _ := parseToolArgumentsObject(rawArgs)
			p.toolDiag.log(toolDiagnosticEntry{
				RequestID:    reqID,
				Phase:        phase + "_tool_call",
				Model:        model,
				Path:         path,
				Direction:    "response",
				ToolName:     tc.Get("function.name").String(),
				ToolID:       tc.Get("id").String(),
				RawArguments: trimForLog(rawArgs),
				FixedArgs:    trimForLog(fixed),
			})
			return true
		})
	}
}

func (p *Proxy) logToolDiagnosticsFromOpenAIStreamChunk(reqID, phase, path string, data []byte) {
	if p.toolDiag == nil {
		return
	}
	root := gjson.ParseBytes(data)
	model := root.Get("model").String()
	if tcs := root.Get("choices.0.delta.tool_calls"); tcs.Exists() && tcs.IsArray() {
		tcs.ForEach(func(_, tc gjson.Result) bool {
			rawArgs := tc.Get("function.arguments").String()
			toolName := tc.Get("function.name").String()
			toolID := tc.Get("id").String()
			if !p.toolDiag.shouldLogStreamChunk(reqID, phase, toolID, toolName) {
				return true
			}
			fixed := ""
			if rawArgs != "" {
				fixed, _ = parseToolArgumentsObject(rawArgs)
			}
			p.toolDiag.log(toolDiagnosticEntry{
				RequestID:    reqID,
				Phase:        phase + "_tool_call_chunk",
				Model:        model,
				Path:         path,
				Direction:    "response",
				ToolName:     toolName,
				ToolID:       toolID,
				RawArguments: trimForLog(rawArgs),
				FixedArgs:    trimForLog(fixed),
			})
			return true
		})
	}
}
