package server

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/ertiti11/freedeep/internal/chat"
	"github.com/ertiti11/freedeep/internal/deepseek"
	"github.com/ertiti11/freedeep/internal/ojson"
)

type apiMode string

const (
	modeOpenAI    apiMode = "openai"
	modeAnthropic apiMode = "anthropic"
	modeResponses apiMode = "responses"
)

type rawFunction struct {
	Name        string          `json:"name"`
	Description any             `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Arguments   any             `json:"arguments"`
}

type rawTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description any             `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Parameters  json.RawMessage `json:"parameters"`
	Function    *rawFunction    `json:"function"`
}

type rawToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function rawFunction `json:"function"`
}

type rawMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	ToolCalls  []rawToolCall `json:"tool_calls"`
	ToolCallID any           `json:"tool_call_id"`
}

type rawRequest struct {
	Model         any            `json:"model"`
	Messages      []rawMessage   `json:"messages"`
	Tools         []rawTool      `json:"tools"`
	Stream        any            `json:"stream"`
	StreamOptions map[string]any `json:"stream_options"`
	User          any            `json:"user"`
	Session       any            `json:"session"`
	System        any            `json:"system"`
	Metadata      map[string]any `json:"metadata"`
	Input         any            `json:"input"`
	Instructions  any            `json:"instructions"`
}

func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

func argumentsString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return "{}"
	}
	return ojson.MarshalString(v)
}

var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// parseRequest decodes a request body in the given dialect.
func parseRequest(mode apiMode, body io.Reader) (*chat.Request, error) {
	var raw rawRequest
	dec := json.NewDecoder(body)
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		if err == io.EOF {
			raw = rawRequest{}
		} else {
			return nil, err
		}
	}
	req := &chat.Request{
		Model:   strings.ToLower(chat.String(raw.Model)),
		Stream:  raw.Stream == true,
		User:    chat.String(raw.User),
		Session: chat.String(raw.Session),
	}
	if req.Model == "" {
		req.Model = deepseek.DefaultModel
	}
	if v, ok := raw.StreamOptions["include_usage"].(bool); ok {
		req.IncludeUsage = v
	}

	switch mode {
	case modeAnthropic:
		req.Messages = anthropicMessages(raw)
		for _, t := range raw.Tools {
			if t.Name == "" {
				continue
			}
			params := t.InputSchema
			if !present(params) {
				params = t.Parameters
			}
			if !present(params) {
				params = emptySchema
			}
			req.Tools = append(req.Tools, chat.Tool{Type: "function", HasFunction: true, Name: t.Name, Description: chat.String(t.Description), Parameters: params})
		}
		if uid := chat.String(raw.Metadata["user_id"]); uid != "" {
			req.User = uid
		}
	case modeResponses:
		req.Messages = responsesInput(raw.Input)
		if chat.Truthy(raw.Instructions) {
			req.Messages = append([]chat.Message{{Role: "system", Content: chat.String(raw.Instructions)}}, req.Messages...)
		}
		for _, t := range raw.Tools {
			switch {
			case t.Type == "function" && t.Function != nil:
				req.Tools = append(req.Tools, openAITool(t))
			case t.Type == "function" && t.Name != "":
				params := t.Parameters
				if !present(params) {
					params = emptySchema
				}
				req.Tools = append(req.Tools, chat.Tool{Type: "function", HasFunction: true, Name: t.Name, Description: chat.String(t.Description), Parameters: params})
			}
		}
	default:
		for _, m := range raw.Messages {
			msg := chat.Message{Role: m.Role, Content: m.Content, ToolCallID: chat.String(m.ToolCallID)}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, chat.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: argumentsString(tc.Function.Arguments)})
			}
			req.Messages = append(req.Messages, msg)
		}
		for _, t := range raw.Tools {
			req.Tools = append(req.Tools, openAITool(t))
		}
	}
	return req, nil
}

func openAITool(t rawTool) chat.Tool {
	tool := chat.Tool{Type: t.Type}
	if t.Function != nil {
		tool.HasFunction = true
		tool.Name = t.Function.Name
		tool.Description = chat.String(t.Function.Description)
		if present(t.Function.Parameters) {
			tool.Parameters = t.Function.Parameters
		}
	}
	return tool
}

func partType(p any) string {
	m, ok := p.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["type"].(string)
	return s
}

func anthropicMessages(raw rawRequest) []chat.Message {
	var out []chat.Message
	if chat.Truthy(raw.System) {
		out = append(out, chat.Message{Role: "system", Content: chat.NormalizeContent(raw.System)})
	}
	for _, m := range raw.Messages {
		parts, isArr := m.Content.([]any)
		role := m.Role
		if role == "" {
			role = "user"
		}
		switch {
		case role == "assistant" && isArr:
			var text []any
			var uses []map[string]any
			for _, p := range parts {
				if partType(p) == "tool_use" {
					uses = append(uses, p.(map[string]any))
				} else {
					text = append(text, p)
				}
			}
			if t := chat.NormalizeContent(text); t != "" {
				out = append(out, chat.Message{Role: "assistant", Content: t})
			}
			for _, tu := range uses {
				input := tu["input"]
				if !chat.Truthy(input) {
					input = map[string]any{}
				}
				out = append(out, chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{
					ID: chat.String(tu["id"]), Name: chat.String(tu["name"]), Arguments: ojson.MarshalString(input),
				}}})
			}
		case role == "user" && isArr && hasToolResult(parts):
			for _, p := range parts {
				if partType(p) == "tool_result" {
					pm := p.(map[string]any)
					out = append(out, chat.Message{Role: "tool", ToolCallID: chat.String(pm["tool_use_id"]), Content: chat.NormalizeContent(pm["content"])})
				} else {
					out = append(out, chat.Message{Role: "user", Content: chat.NormalizeContent(p)})
				}
			}
		default:
			out = append(out, chat.Message{Role: role, Content: chat.NormalizeContent(m.Content)})
		}
	}
	return out
}

func hasToolResult(parts []any) bool {
	for _, p := range parts {
		if partType(p) == "tool_result" {
			return true
		}
	}
	return false
}

func responsesInput(input any) []chat.Message {
	if s, ok := input.(string); ok {
		return []chat.Message{{Role: "user", Content: s}}
	}
	items, ok := input.([]any)
	if !ok {
		return nil
	}
	var out []chat.Message
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		role := chat.String(item["role"])
		switch {
		case typ == "message":
			if role == "" {
				role = "user"
			}
			out = append(out, chat.Message{Role: role, Content: chat.NormalizeContent(item["content"])})
		case role != "":
			out = append(out, chat.Message{Role: role, Content: chat.NormalizeContent(item["content"])})
		case typ == "function_call_output":
			out = append(out, chat.Message{Role: "tool", ToolCallID: chat.String(item["call_id"]), Content: chat.NormalizeContent(item["output"])})
		case typ == "function_call":
			// Echoed tool calls keep the transcript coherent for the model.
			out = append(out, chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{
				ID: chat.String(item["call_id"]), Name: chat.String(item["name"]), Arguments: argumentsString(item["arguments"]),
			}}})
		case typ == "input_text":
			out = append(out, chat.Message{Role: "user", Content: chat.String(item["text"])})
		}
	}
	return out
}
