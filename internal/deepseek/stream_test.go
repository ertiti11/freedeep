package deepseek

import (
	"fmt"
	"strings"
	"testing"
)

func TestSearchFragmentsAreAssistantOutput(t *testing.T) {
	resp, think := RebuildFragmentText([]map[string]any{
		{"type": "SEARCH", "content": "The official Reuters website is "},
		{"type": "SEARCH", "content": "https://www.reuters.com/."},
	})
	if resp != "The official Reuters website is https://www.reuters.com/." || think != "" {
		t.Errorf("resp=%q think=%q", resp, think)
	}
}

func TestResponseLevelAppendPatches(t *testing.T) {
	var frags []map[string]any
	applied := ApplyResponsePatchOperations([]any{
		map[string]any{"p": "fragments", "o": "APPEND", "v": []any{map[string]any{"type": "RESPONSE", "content": "The"}}},
		map[string]any{"p": "has_pending_fragment", "o": "SET", "v": false},
	}, func(v any) {
		for _, f := range v.([]any) {
			frags = append(frags, f.(map[string]any))
		}
	})
	if !applied || len(frags) != 1 {
		t.Fatalf("applied=%v frags=%v", applied, frags)
	}
	if resp, _ := RebuildFragmentText(frags); resp != "The" {
		t.Errorf("resp=%q", resp)
	}
}

func TestModelErrorEventDetection(t *testing.T) {
	if IsModelErrorEvent(map[string]any{"content": "Official Reuters website URL"}) ||
		IsModelErrorEvent(map[string]any{"finish_reason": "stop"}) ||
		!IsModelErrorEvent(map[string]any{"type": "error", "content": "backend error"}) {
		t.Error("model error detection")
	}
}

// A realistic thinking-mode stream using the incremental patch protocol.
var thinkingStream = strings.Join([]string{
	`event: ready`,
	`data: {"request_message_id":1,"response_message_id":2}`,
	``,
	`data: {"v":{"response":{"message_id":2,"fragments":[{"id":1,"type":"THINK","content":"Let"}]}}}`,
	`data: {"p":"response/fragments/-1/content","o":"APPEND","v":" me think"}`,
	`data: {"v":"."}`,
	`data: {"p":"response/fragments","o":"APPEND","v":[{"id":2,"type":"RESPONSE","content":"Hel"}]}`,
	`data: {"p":"response/fragments/-1/content","v":"lo"}`,
	`data: {"v":" ✓"}`,
	`data: {"p":"response","o":"BATCH","v":[{"p":"fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"!"}]},{"p":"quasi_status","v":"FINISHED"}]}`,
	`data: {"p":"response/status","o":"SET","v":"FINISHED"}`,
	`event: close`,
	`data: {"click_behavior":"none"}`,
	``,
}, "\n")

func TestReadStreamThinkingProtocol(t *testing.T) {
	var updates []string
	res, err := ReadStream(strings.NewReader(thinkingStream), func(c, r string) { updates = append(updates, c+"|"+r) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Hello ✓!" || res.Reasoning != "Let me think." {
		t.Errorf("content=%q reasoning=%q", res.Content, res.Reasoning)
	}
	if fmt.Sprint(res.MessageID) != "2" {
		t.Errorf("message id = %v", res.MessageID)
	}
	if res.FinishReason != "" {
		t.Errorf("FINISHED status must not become a finish reason, got %q", res.FinishReason)
	}
	if len(updates) < 4 || updates[len(updates)-1] != "Hello ✓!|Let me think." {
		t.Errorf("updates=%v", updates)
	}
}

func TestReadStreamLegacyContentAndErrors(t *testing.T) {
	legacy := "data: {\"p\":\"response/content\",\"v\":\"Hi\"}\ndata: {\"v\":\" there\"}\ndata: {\"p\":\"response/finish_reason\",\"v\":\"length\"}\n"
	res, _ := ReadStream(strings.NewReader(legacy), nil)
	if res.Content != "Hi there" || res.FinishReason != "length" {
		t.Errorf("%+v", res)
	}
	errStream := "data: {\"type\":\"error\",\"content\":\"Content too long\",\"finish_reason\":\"input_exceeds_limit\"}\n"
	res, _ = ReadStream(strings.NewReader(errStream), nil)
	if res.ModelError == nil || res.ModelError.Content != "Content too long" || res.FinishReason != "input_exceeds_limit" {
		t.Errorf("%+v", res)
	}
	// A final line without a trailing newline is still processed.
	res, _ = ReadStream(strings.NewReader(`data: {"p":"response/content","v":"tail"}`), nil)
	if res.Content != "tail" {
		t.Errorf("%+v", res)
	}
}

func TestUpstreamErrorTypes(t *testing.T) {
	e := &UpstreamError{Status: 429, Body: "  Rate limited\ntry later  ", RetryAfter: "12"}
	if e.Type() != "rate_limit_error" || !strings.Contains(e.Error(), "Rate limited try later") {
		t.Errorf("%s / %s", e.Type(), e.Error())
	}
	if (&UpstreamError{Status: 401}).Type() != "authentication_error" || (&UpstreamError{Status: 500}).Type() != "upstream_http_error" {
		t.Error("types")
	}
}

func TestModelTable(t *testing.T) {
	if m := ResolveModel("DEEPSEEK-REASONER"); m.ID != "deepseek-reasoner" || !m.ThinkingEnabled {
		t.Error("case-insensitive lookup")
	}
	if ResolveModel("nope").ID != DefaultModel {
		t.Error("fallback")
	}
	if m, _ := LookupModel("deepseek-vision"); m.Supported {
		t.Error("vision must be unsupported")
	}
}
