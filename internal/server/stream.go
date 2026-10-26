package server

import (
	"net/http"
	"strings"

	"github.com/ertiti11/freedeep/internal/ojson"
)

// streamWriter emits one response as server-sent events in a specific API
// dialect. Nothing is written until the first event, so a request can still
// fail with a normal JSON error (and a real status code) before that point.
type streamWriter interface {
	reasoning(delta string)
	content(delta string)
	toolCall(tc ToolCall)
	finish(reason string, usage Usage)
	fail(status int, errType, message string)
	started() bool
}

type sse struct {
	w       http.ResponseWriter
	f       http.Flusher
	begun   bool
	onBegin func()
}

func newSSE(w http.ResponseWriter) *sse {
	f, _ := w.(http.Flusher)
	return &sse{w: w, f: f}
}

func (s *sse) begin() {
	if s.begun {
		return
	}
	s.begun = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	if s.onBegin != nil {
		s.onBegin()
	}
}

func (s *sse) event(name string, data any) {
	s.begin()
	var b strings.Builder
	if name != "" {
		b.WriteString("event: " + name + "\n")
	}
	b.WriteString("data: " + ojson.MarshalString(data) + "\n\n")
	_, _ = s.w.Write([]byte(b.String()))
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sse) raw(line string) {
	s.begin()
	_, _ = s.w.Write([]byte(line))
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sse) started() bool { return s.begun }

// replay streams an already complete response in chunks.
func replay(sw streamWriter, c *Completion) {
	msg := c.message()
	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			sw.toolCall(tc)
		}
		sw.finish("tool_calls", c.Usage)
		return
	}
	for _, chunk := range chunkRunes(msg.ReasoningContent, 80) {
		sw.reasoning(chunk)
	}
	for _, chunk := range chunkRunes(c.text(), 80) {
		sw.content(chunk)
	}
	sw.finish(c.Choices[0].FinishReason, c.Usage)
}

func chunkRunes(s string, n int) []string {
	if s == "" {
		return nil
	}
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += n {
		out = append(out, string(r[i:min(len(r), i+n)]))
	}
	return out
}

// ---------------------------------------------------------------------------
// OpenAI Chat Completions

type openAIStream struct {
	*sse
	c            *Completion
	includeUsage bool
	sentRole     bool
}

func (s *openAIStream) chunk(delta map[string]any, finish any) {
	if !s.sentRole {
		delta["role"] = "assistant"
		s.sentRole = true
	}
	s.event("", map[string]any{"id": s.c.ID, "object": "chat.completion.chunk", "created": s.c.Created, "model": s.c.Model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
}

func (s *openAIStream) reasoning(d string) { s.chunk(map[string]any{"reasoning_content": d}, nil) }
func (s *openAIStream) content(d string)   { s.chunk(map[string]any{"content": d}, nil) }

func (s *openAIStream) toolCall(tc ToolCall) {
	idx := 0
	tc.Index = &idx
	s.chunk(map[string]any{"content": nil, "tool_calls": []ToolCall{tc}}, nil)
}

func (s *openAIStream) finish(reason string, u Usage) {
	s.chunk(map[string]any{}, reason)
	if s.includeUsage {
		s.event("", map[string]any{"id": s.c.ID, "object": "chat.completion.chunk", "created": s.c.Created, "model": s.c.Model, "choices": []any{}, "usage": u})
	}
	s.raw("data: [DONE]\n\n")
}

func (s *openAIStream) fail(status int, errType, message string) {
	s.event("", map[string]any{"error": map[string]any{"message": message, "type": errType, "code": status}})
	s.raw("data: [DONE]\n\n")
}

// ---------------------------------------------------------------------------
// Anthropic Messages

type anthropicStream struct {
	*sse
	c      *Completion
	index  int
	open   string // "", "reasoning", "text"
	opened bool   // any block was opened
}

func newAnthropicStream(w http.ResponseWriter, c *Completion) *anthropicStream {
	s := &anthropicStream{sse: newSSE(w), c: c}
	s.onBegin = func() {
		s.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_" + c.ID, "type": "message", "role": "assistant", "model": c.Model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage":     map[string]int{"input_tokens": c.Usage.PromptTokens, "output_tokens": 0},
			"watermark": Watermark,
		}})
	}
	return s
}

func (s *anthropicStream) startBlock(block map[string]any) {
	s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.index, "content_block": block})
	s.opened = true
}

func (s *anthropicStream) textDelta(text string) {
	s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index, "delta": map[string]any{"type": "text_delta", "text": text}})
}

func (s *anthropicStream) closeBlock() {
	if s.open == "" {
		return
	}
	if s.open == "reasoning" {
		s.textDelta("\n[/reasoning]\n")
	}
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
	s.open = ""
	s.index++
}

// Reasoning is rendered as a delimited text block: Anthropic "thinking"
// blocks require signatures that this proxy cannot produce.
func (s *anthropicStream) reasoning(d string) {
	if s.open != "reasoning" {
		s.closeBlock()
		s.startBlock(map[string]any{"type": "text", "text": ""})
		s.open = "reasoning"
		s.textDelta("[reasoning]\n")
	}
	s.textDelta(d)
}

func (s *anthropicStream) content(d string) {
	if s.open != "text" {
		s.closeBlock()
		s.startBlock(map[string]any{"type": "text", "text": ""})
		s.open = "text"
	}
	s.textDelta(d)
}

// Tool turns contain tool_use blocks only: if reasoning were emitted as a
// text block first, some agents would treat the turn as a plain answer.
func (s *anthropicStream) toolCall(tc ToolCall) {
	s.closeBlock()
	s.startBlock(map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}})
	s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": orEmptyObject(tc.Function.Arguments)}})
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
	s.index++
}

func (s *anthropicStream) finish(reason string, u Usage) {
	s.closeBlock()
	if !s.opened && reason != "tool_calls" {
		s.startBlock(map[string]any{"type": "text", "text": ""})
		s.open = "text"
		s.closeBlock()
	}
	s.event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(reason), "stop_sequence": nil},
		"usage": map[string]int{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens}})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

func (s *anthropicStream) fail(status int, errType, message string) {
	s.event("error", map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}})
}

// ---------------------------------------------------------------------------
// OpenAI Responses

type responsesStream struct {
	*sse
	c        *Completion
	output   []any
	text     strings.Builder
	open     string // "", "reasoning", "message"
	itemID   string
	itemText strings.Builder
	hadMsg   bool
}

func newResponsesStream(w http.ResponseWriter, c *Completion) *responsesStream {
	s := &responsesStream{sse: newSSE(w), c: c}
	s.onBegin = func() {
		r := responsesObject(c, "in_progress", nil, "", Usage{})
		s.event("response.created", map[string]any{"type": "response.created", "response": r})
		s.event("response.in_progress", map[string]any{"type": "response.in_progress", "response": r})
	}
	return s
}

func (s *responsesStream) closeItem() {
	idx := len(s.output)
	switch s.open {
	case "reasoning":
		item := reasoningItem(s.itemID, s.itemText.String())
		s.event("response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "output_index": idx, "item_id": s.itemID, "summary_index": 0, "text": s.itemText.String()})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": idx, "item": item})
		s.output = append(s.output, item)
	case "message":
		text := s.itemText.String()
		item := messageItem(s.itemID, text)
		s.event("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": idx, "content_index": 0, "item_id": s.itemID, "text": text})
		s.event("response.content_part.done", map[string]any{"type": "response.content_part.done", "output_index": idx, "content_index": 0, "item_id": s.itemID, "part": outputTextPart(text)})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": idx, "item": item})
		s.output = append(s.output, item)
	default:
		return
	}
	s.open = ""
	s.itemText.Reset()
}

func (s *responsesStream) reasoning(d string) {
	if s.open != "reasoning" {
		s.closeItem()
		s.open, s.itemID = "reasoning", newItemID("rs_")
		s.event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": len(s.output),
			"item": map[string]any{"id": s.itemID, "type": "reasoning", "summary": []any{}, "status": "in_progress"}})
	}
	s.itemText.WriteString(d)
	s.event("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": len(s.output), "item_id": s.itemID, "summary_index": 0, "delta": d})
}

func (s *responsesStream) openMessage() {
	s.closeItem()
	s.open, s.itemID, s.hadMsg = "message", newItemID("msg_"), true
	idx := len(s.output)
	s.event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": idx,
		"item": map[string]any{"id": s.itemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	s.event("response.content_part.added", map[string]any{"type": "response.content_part.added", "output_index": idx, "content_index": 0, "item_id": s.itemID, "part": outputTextPart("")})
}

func (s *responsesStream) content(d string) {
	if s.open != "message" {
		s.openMessage()
	}
	s.itemText.WriteString(d)
	s.text.WriteString(d)
	s.event("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": len(s.output), "content_index": 0, "item_id": s.itemID, "delta": d})
}

func (s *responsesStream) toolCall(tc ToolCall) {
	s.closeItem()
	idx := len(s.output)
	item := functionCallItem(tc)
	added := map[string]any{}
	for k, v := range item {
		added[k] = v
	}
	added["arguments"], added["status"] = "", "in_progress"
	s.event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": idx, "item": added})
	s.event("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": idx, "item_id": item["id"], "delta": item["arguments"]})
	s.event("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": idx, "item_id": item["id"], "arguments": item["arguments"]})
	s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": idx, "item": item})
	s.output = append(s.output, item)
}

func (s *responsesStream) finish(reason string, u Usage) {
	if !s.hadMsg && reason != "tool_calls" {
		s.openMessage()
	}
	s.closeItem()
	r := responsesObject(s.c, "completed", s.output, s.text.String(), u)
	s.event("response.completed", map[string]any{"type": "response.completed", "response": r})
	s.raw("data: [DONE]\n\n")
}

func (s *responsesStream) fail(status int, errType, message string) {
	s.event("error", map[string]any{"type": "error", "code": errType, "message": message})
}

func newStreamWriter(mode apiMode, w http.ResponseWriter, c *Completion, includeUsage bool) streamWriter {
	switch mode {
	case modeAnthropic:
		return newAnthropicStream(w, c)
	case modeResponses:
		return newResponsesStream(w, c)
	}
	return &openAIStream{sse: newSSE(w), c: c, includeUsage: includeUsage}
}
