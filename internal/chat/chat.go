// Package chat holds the provider-neutral request model that the OpenAI,
// Anthropic and Responses adapters normalize into.
package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ertiti11/freedeep/internal/ojson"
)

// Message is one OpenAI-style chat message.
type Message struct {
	Role       string
	Content    any // string, []any of parts, nil, or other decoded JSON
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolCall is an OpenAI-style assistant tool call.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON-encoded object
}

// Tool is an OpenAI-style function tool definition.
type Tool struct {
	Type        string
	Name        string
	Description string
	Parameters  json.RawMessage // nil when absent
	HasFunction bool            // OpenAI shape: {type, function:{...}}
}

// IsFunction reports whether the tool is callable through the adapter.
func (t Tool) IsFunction() bool { return t.Type == "function" && t.HasFunction && t.Name != "" }

// Request is the normalized chat request.
type Request struct {
	Model        string
	Messages     []Message
	Tools        []Tool
	Stream       bool
	IncludeUsage bool
	User         string
	Session      string
}

// Truthy mirrors JavaScript truthiness for decoded JSON values.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	}
	return true
}

// String mirrors JavaScript String(v) for scalar JSON values.
func String(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprint(t)
	}
	return ojson.MarshalString(v)
}

func field(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

// NormalizeContent flattens OpenAI/Anthropic/Responses content into text.
func NormalizeContent(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, raw := range c {
			var s string
			switch part := raw.(type) {
			case string:
				s = part
			case map[string]any:
				typ, _ := part["type"].(string)
				switch typ {
				case "text", "input_text", "output_text":
					s = String(part["text"])
				case "tool_result":
					s = "[Tool Result " + String(part["tool_use_id"]) + "]\n" + NormalizeContent(part["content"])
				case "image_url":
					url := ""
					if iu, ok := part["image_url"].(map[string]any); ok {
						url = String(field(iu, "url"))
					} else if s, ok := part["image_url"].(string); ok {
						url = s
					}
					s = "[Image: " + url + "]"
				default:
					switch {
					case Truthy(part["text"]):
						s = String(part["text"])
					case Truthy(part["content"]):
						s = NormalizeContent(part["content"])
					default:
						s = ojson.MarshalString(part)
					}
				}
			}
			if s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if Truthy(c["text"]) {
			return String(c["text"])
		}
		return ojson.MarshalString(c)
	}
	return String(content)
}
