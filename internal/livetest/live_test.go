// Package livetest holds smoke tests against a running proxy that is logged
// in to a real DeepSeek account. They are skipped unless FREEDEEP_LIVE_URL is set:
//
//	FREEDEEP_LIVE_URL=http://127.0.0.1:9655 go test ./internal/livetest -v
package livetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func base(t *testing.T) string {
	u := os.Getenv("FREEDEEP_LIVE_URL")
	if u == "" {
		t.Skip("set FREEDEEP_LIVE_URL to run live smoke tests against a running proxy")
	}
	return strings.TrimRight(u, "/")
}

func model() string {
	if m := os.Getenv("FREEDEEP_LIVE_MODEL"); m != "" {
		return m
	}
	return "deepseek-chat"
}

func post(t *testing.T, path string, body any) (int, string, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base(t)+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if k := os.Getenv("FREEDEEP_LIVE_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	return resp.StatusCode, string(raw), m
}

func uid(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

const askTool = "What time is it in UTC? Use the available tool. Output only the tool request."

var timeSchema = map[string]any{"type": "object", "properties": map[string]any{"timezone": map[string]any{"type": "string"}}, "required": []any{"timezone"}}

func TestLiveChatCompletions(t *testing.T) {
	_, raw, m := post(t, "/v1/chat/completions", map[string]any{"model": model(), "user": uid("smoke-chat"),
		"messages": []any{map[string]any{"role": "user", "content": "Reply exactly: OK_CHAT"}}})
	if !regexp.MustCompile(`(?i)OK_CHAT`).MatchString(fmt.Sprint(dig(m, "choices", 0, "message", "content"))) {
		t.Errorf("unexpected: %s", raw)
	}
}

func TestLiveMessages(t *testing.T) {
	_, raw, m := post(t, "/v1/messages", map[string]any{"model": model(), "max_tokens": 64, "metadata": map[string]any{"user_id": uid("smoke-msg")},
		"messages": []any{map[string]any{"role": "user", "content": "Reply exactly: OK_MESSAGES"}}})
	if !strings.Contains(strings.ToUpper(fmt.Sprint(dig(m, "content", 0, "text"))), "OK_MESSAGES") {
		t.Errorf("unexpected: %s", raw)
	}
}

func TestLiveResponses(t *testing.T) {
	_, raw, m := post(t, "/v1/responses", map[string]any{"model": model(), "user": uid("smoke-resp"), "input": "Reply exactly: OK_RESPONSES"})
	if !strings.Contains(strings.ToUpper(fmt.Sprint(m["output_text"])), "OK_RESPONSES") {
		t.Errorf("unexpected: %s", raw)
	}
}

func TestLiveOpenAIToolCall(t *testing.T) {
	_, raw, m := post(t, "/v1/chat/completions", map[string]any{"model": model(), "user": uid("smoke-tool-oai"),
		"messages": []any{map[string]any{"role": "user", "content": askTool}},
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_current_time", "description": "Return current local time for a timezone.", "parameters": timeSchema}}}})
	if dig(m, "choices", 0, "message", "tool_calls", 0, "function", "name") != "get_current_time" || dig(m, "choices", 0, "message", "reasoning_content") != nil {
		t.Errorf("no/incorrect tool call: %s", raw)
	}
}

func TestLiveAnthropicToolCall(t *testing.T) {
	_, raw, m := post(t, "/v1/messages", map[string]any{"model": model(), "max_tokens": 128, "metadata": map[string]any{"user_id": uid("smoke-tool-anth")},
		"messages": []any{map[string]any{"role": "user", "content": askTool}},
		"tools":    []any{map[string]any{"name": "get_current_time", "description": "Return current local time for a timezone.", "input_schema": timeSchema}}})
	if dig(m, "content", 0, "type") != "tool_use" || dig(m, "content", 0, "name") != "get_current_time" || m["reasoning_content"] != nil {
		t.Errorf("no/incorrect tool_use: %s", raw)
	}
}

func TestLiveResponsesToolCall(t *testing.T) {
	_, raw, m := post(t, "/v1/responses", map[string]any{"model": model(), "user": uid("smoke-tool-resp"), "input": askTool,
		"tools": []any{map[string]any{"type": "function", "name": "get_current_time", "description": "Return current local time for a timezone.", "parameters": timeSchema}}})
	if dig(m, "output", 0, "type") != "function_call" || dig(m, "output", 0, "name") != "get_current_time" {
		t.Errorf("no/incorrect function_call: %s", raw)
	}
}

func TestLiveAnthropicStreamingToolOnly(t *testing.T) {
	_, raw, _ := post(t, "/v1/messages", map[string]any{"model": model(), "max_tokens": 256, "stream": true, "metadata": map[string]any{"user_id": uid("smoke-tool-stream")},
		"messages": []any{map[string]any{"role": "user", "content": "Create a tiny script file named test-deepseek.py that prints the current date and time. Use the available tool. Output only the tool request."}},
		"tools": []any{map[string]any{"name": "write_file", "description": "Write a local file. Use this when the user asks you to create a file.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []any{"path", "content"}}}}})
	if !strings.Contains(raw, `"type":"tool_use"`) || strings.Contains(raw, `"text_delta"`) || strings.Contains(raw, "[reasoning]") {
		t.Errorf("tool stream must contain only tool_use blocks: %.400s", raw)
	}
}
