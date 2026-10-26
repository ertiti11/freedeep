package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ertiti11/freedeep/internal/ojson"
	"github.com/ertiti11/freedeep/internal/toolcall"
)

// Watermark is attached to responses, as in the original project.
const Watermark = "t.me/forgetmeai"

// Usage is OpenAI-style token accounting (estimated: DeepSeek Web does not
// report token counts, so this is ~4 characters per token).
type Usage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func estimateTokens(s string) int { return (utf8.RuneCountInString(s) + 3) / 4 }

func buildUsage(promptText, content, reasoning string) Usage {
	var u Usage
	u.PromptTokens = estimateTokens(promptText)
	u.CompletionTokensDetails.ReasoningTokens = estimateTokens(reasoning)
	u.CompletionTokens = estimateTokens(content) + u.CompletionTokensDetails.ReasoningTokens
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}

// FunctionCall is the function part of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is an OpenAI tool call in a response.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// Message is an OpenAI assistant message.
type Message struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Choice is one OpenAI completion choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Completion is an OpenAI chat.completion object.
type Completion struct {
	ID        string   `json:"id"`
	Object    string   `json:"object"`
	Created   int64    `json:"created"`
	Model     string   `json:"model"`
	Choices   []Choice `json:"choices"`
	Usage     Usage    `json:"usage"`
	Watermark string   `json:"watermark"`
}

func randomSuffix(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func newCompletionID() string {
	return fmt.Sprintf("ds-%d%s", time.Now().UnixMilli(), randomSuffix(6))
}

func newCompletion(model string) *Completion {
	return &Completion{ID: newCompletionID(), Object: "chat.completion", Created: time.Now().Unix(), Model: model, Watermark: Watermark}
}

// buildToolCallResponse never attaches reasoning: some agent clients treat
// any reasoning/text payload as a final answer and stop their tool loop.
func buildToolCallResponse(tc *toolcall.Call, model, promptText, reasoning string) *Completion {
	c := newCompletion(model)
	c.Choices = []Choice{{
		Message: Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID:       fmt.Sprintf("call_%d_%s", time.Now().UnixMilli(), randomSuffix(6)),
			Type:     "function",
			Function: FunctionCall{Name: tc.Name, Arguments: tc.Arguments},
		}}},
		FinishReason: "tool_calls",
	}}
	c.Usage = buildUsage(promptText, "", reasoning)
	return c
}

// liveToolCall builds the streaming tool_call payload from a parsed call.
func liveToolCall(tc *toolcall.Call) ToolCall {
	return ToolCall{
		ID:       fmt.Sprintf("call_%d_%s", time.Now().UnixMilli(), randomSuffix(6)),
		Type:     "function",
		Function: FunctionCall{Name: tc.Name, Arguments: tc.Arguments},
	}
}

func textFinishReason(upstream string) string {
	// Surface truncation so length-aware clients can re-request.
	if upstream == "length" {
		return "length"
	}
	return "stop"
}

func buildTextResponse(content, promptText, model, reasoning, finish string) *Completion {
	c := newCompletion(model)
	text := content
	c.Choices = []Choice{{
		Message:      Message{Role: "assistant", Content: &text, ReasoningContent: reasoning},
		FinishReason: textFinishReason(finish),
	}}
	c.Usage = buildUsage(promptText, content, reasoning)
	return c
}

func (c *Completion) message() Message { return c.Choices[0].Message }

func (c *Completion) text() string {
	if m := c.message(); m.Content != nil {
		return *m.Content
	}
	return ""
}

// ---------------------------------------------------------------------------
// Anthropic Messages

func anthropicStopReason(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	}
	return "end_turn"
}

func argumentsObject(args string) json.RawMessage {
	if obj, ok := ojson.RawObject([]byte(args)); ok {
		if b, err := ojson.Marshal(obj); err == nil {
			return b
		}
	}
	return json.RawMessage(`{}`)
}

func toAnthropic(c *Completion) map[string]any {
	msg := c.message()
	var content []any
	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": argumentsObject(tc.Function.Arguments)})
		}
	} else {
		content = append(content, map[string]any{"type": "text", "text": c.text()})
	}
	resp := map[string]any{
		"id":            "msg_" + c.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         c.Model,
		"content":       content,
		"stop_reason":   anthropicStopReason(c.Choices[0].FinishReason),
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": c.Usage.PromptTokens, "output_tokens": c.Usage.CompletionTokens},
		"watermark":     Watermark,
	}
	if len(msg.ToolCalls) == 0 && msg.ReasoningContent != "" {
		resp["reasoning_content"] = msg.ReasoningContent
	}
	return resp
}

// ---------------------------------------------------------------------------
// OpenAI Responses

func responsesID(c *Completion) string { return "resp_" + strings.TrimPrefix(c.ID, "ds-") }

func responsesUsage(u Usage) map[string]any {
	return map[string]any{
		"input_tokens":          u.PromptTokens,
		"output_tokens":         u.CompletionTokens,
		"total_tokens":          u.TotalTokens,
		"output_tokens_details": map[string]int{"reasoning_tokens": u.CompletionTokensDetails.ReasoningTokens},
	}
}

func reasoningItem(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
}

func functionCallItem(tc ToolCall) map[string]any {
	return map[string]any{"type": "function_call", "id": "fc_" + tc.ID, "call_id": tc.ID, "name": tc.Function.Name, "arguments": orEmptyObject(tc.Function.Arguments), "status": "completed"}
}

func outputTextPart(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
}

func messageItem(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": "completed", "content": []any{outputTextPart(text)}}
}

func orEmptyObject(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

func newItemID(prefix string) string { return prefix + randomSuffix(16) }

func toResponses(c *Completion) map[string]any {
	msg := c.message()
	var output []any
	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			output = append(output, functionCallItem(tc))
		}
	} else {
		if msg.ReasoningContent != "" {
			output = append(output, reasoningItem(newItemID("rs_"), msg.ReasoningContent))
		}
		output = append(output, messageItem(newItemID("msg_"), c.text()))
	}
	return responsesObject(c, "completed", output, c.text(), c.Usage)
}

func responsesObject(c *Completion, status string, output []any, text string, u Usage) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id":          responsesID(c),
		"object":      "response",
		"created_at":  c.Created,
		"status":      status,
		"model":       c.Model,
		"output":      output,
		"output_text": text,
		"usage":       responsesUsage(u),
		"watermark":   Watermark,
	}
}
