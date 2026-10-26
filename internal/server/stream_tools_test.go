package server

import (
	"strings"
	"testing"
)

func TestLiveContentDecision(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", liveWait},
		{"T", liveWait},
		{"TOOL_", liveWait},
		{"Hello there", liveCommit},
		{"The answer is 42", liveCommit},
		{"{\"tool_call\":", liveHold},
		{"<｜tool▁calls▁begin｜>", liveHold},
		{"<tool_call>", liveHold},
		{"|DSML|tool_calls", liveHold},
		{"```json", liveHold},
		{"TOOL_CALL: read_file", liveHold},
		{"tool_call is a term I will explain", liveCommit},
	}
	for _, c := range cases {
		if got := liveContentDecision(c.in); got != c.want {
			t.Errorf("liveContentDecision(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// With tools present, a plain-text answer must still stream live, in several
// deltas, and must not be mistaken for a tool call.
func TestStreamingWithToolsStreamsProseLive(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply("Here is a normal answer with no tool markup at all", "thinking", ""))
	resp, body := h.post("/v1/chat/completions", map[string]any{"stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "q"}}, "tools": []any{readTool}})
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q: %s", ct, body)
	}
	var content strings.Builder
	var finish any
	for _, e := range parseSSE(body) {
		if e.Data == "[DONE]" {
			continue
		}
		m := decode(t, e.Data)
		if c, ok := get(m, "choices", 0, "delta", "content").(string); ok {
			content.WriteString(c)
		}
		if f := get(m, "choices", 0, "finish_reason"); f != nil {
			finish = f
		}
	}
	if content.String() != "Here is a normal answer with no tool markup at all" || finish != "stop" {
		t.Fatalf("content=%q finish=%v", content.String(), finish)
	}
	// Reasoning is never streamed while guarding a tool turn.
	if strings.Contains(body, "reasoning") {
		t.Errorf("reasoning must not stream during a guarded turn: %s", body)
	}
	if n := strings.Count(body, `"content":"`); n < 3 {
		t.Errorf("expected incremental content deltas, got %d: %s", n, body)
	}
}

// A JSON answer that is not a tool-call envelope must be held back, then
// flushed as plain content rather than leaking partially or failing.
func TestStreamingWithToolsFlushesHeldJSONAnswer(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply(`{"result": 42, "ok": true}`, "", ""))
	_, body := h.post("/v1/chat/completions", map[string]any{"stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "give json"}}, "tools": []any{readTool}})
	var content strings.Builder
	var finish any
	for _, e := range parseSSE(body) {
		if e.Data == "[DONE]" {
			continue
		}
		m := decode(t, e.Data)
		if c, ok := get(m, "choices", 0, "delta", "content").(string); ok {
			content.WriteString(c)
		}
		if f := get(m, "choices", 0, "finish_reason"); f != nil {
			finish = f
		}
	}
	if content.String() != `{"result": 42, "ok": true}` || finish != "stop" {
		t.Errorf("held JSON answer not flushed: content=%q finish=%v\n%s", content.String(), finish, body)
	}
	if strings.Contains(body, "tool_calls") {
		t.Errorf("plain JSON answer must not become a tool call: %s", body)
	}
}

// Disabling tool streaming falls back to buffer-then-replay (one content blob).
func TestStreamWithToolsDisabledBuffers(t *testing.T) {
	h := newHarness(t, map[string]string{"DEEPSEEK_STREAM_WITH_TOOLS": "0"})
	h.queue(reply("A short buffered answer here", "", ""))
	_, body := h.post("/v1/chat/completions", map[string]any{"stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "q"}}, "tools": []any{readTool}})
	var content strings.Builder
	for _, e := range parseSSE(body) {
		if e.Data == "[DONE]" {
			continue
		}
		if c, ok := get(decode(t, e.Data), "choices", 0, "delta", "content").(string); ok {
			content.WriteString(c)
		}
	}
	if content.String() != "A short buffered answer here" {
		t.Errorf("buffered content=%q", content.String())
	}
}

func TestHideReasoningNonStream(t *testing.T) {
	h := newHarness(t, map[string]string{"DEEPSEEK_SHOW_REASONING": "0"})
	h.queue(reply("42", "secret chain of thought", ""))
	_, body := h.post("/v1/chat/completions", map[string]any{"model": "deepseek-reasoner",
		"messages": []any{map[string]any{"role": "user", "content": "q"}}})
	m := decode(t, body)
	if get(m, "choices", 0, "message", "content") != "42" {
		t.Fatalf("content: %s", body)
	}
	if rc := get(m, "choices", 0, "message", "reasoning_content"); rc != nil {
		t.Errorf("reasoning must be hidden, got %v", rc)
	}
	// Hidden reasoning is omitted entirely, including from the token count.
	if rt := get(m, "usage", "completion_tokens_details", "reasoning_tokens"); rt != float64(0) {
		t.Errorf("hidden reasoning tokens should be 0, got %v", rt)
	}
}

func TestHideReasoningLiveStreamWithoutTools(t *testing.T) {
	h := newHarness(t, map[string]string{"DEEPSEEK_SHOW_REASONING": "0"})
	h.queue(reply("live answer", "hidden reasoning", ""))
	_, body := h.post("/v1/chat/completions", map[string]any{"stream": true, "model": "deepseek-r1",
		"messages": []any{map[string]any{"role": "user", "content": "q"}}})
	if strings.Contains(body, "reasoning") {
		t.Errorf("reasoning must not appear in stream: %s", body)
	}
	if !strings.Contains(body, `"content":"live answer"`) && !strings.Contains(body, `"content":"live`) {
		t.Errorf("content missing: %s", body)
	}
}

func TestHideReasoningAnthropicStreamHasNoReasoningBlock(t *testing.T) {
	h := newHarness(t, map[string]string{"DEEPSEEK_SHOW_REASONING": "0"})
	h.queue(reply("Answer text", "why", ""))
	_, body := h.post("/v1/messages", map[string]any{"stream": true, "model": "deepseek-r1",
		"messages": []any{map[string]any{"role": "user", "content": "q"}}})
	if strings.Contains(body, "[reasoning]") {
		t.Errorf("Anthropic stream must not contain a [reasoning] block: %s", body)
	}
	var text strings.Builder
	for _, e := range parseSSE(body) {
		if e.Name == "content_block_delta" {
			if d, ok := get(decode(t, e.Data), "delta", "text").(string); ok {
				text.WriteString(d)
			}
		}
	}
	if text.String() != "Answer text" {
		t.Errorf("text %q", text.String())
	}
}
