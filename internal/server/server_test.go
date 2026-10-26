package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ertiti11/freedeep/internal/config"
	"github.com/ertiti11/freedeep/internal/deepseek"
	"github.com/ertiti11/freedeep/internal/pow"
	"github.com/ertiti11/freedeep/internal/state"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Fake DeepSeek Web upstream

type fakeDeepSeek struct {
	t        *testing.T
	mu       sync.Mutex
	sessions int
	calls    []deepseek.CompletionRequest
	replies  []func(w http.ResponseWriter)
	msgID    int
	powUsed  map[string]bool
}

const powAnswer = 5

func (f *fakeDeepSeek) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v0/chat/create_pow_challenge", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		salt := fmt.Sprintf("salt%d", len(f.powUsed))
		f.mu.Unlock()
		h := pow.Hash([]byte(fmt.Sprintf("%s_%d_%d", salt, 1700000000, powAnswer)))
		writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"biz_data": map[string]any{"challenge": map[string]any{
			"algorithm": "DeepSeekHashV1", "challenge": hex.EncodeToString(h[:]), "salt": salt, "signature": "sig-" + salt,
			"difficulty": 100, "expire_at": 1700000000, "expire_after": 300000, "target_path": "/api/v0/chat/completion"}}}})
	})
	mux.HandleFunc("POST /api/v0/chat_session/create", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sessions++
		id := fmt.Sprintf("chat-%d", f.sessions)
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"data": map[string]any{"biz_data": map[string]any{"chat_session": map[string]any{"id": id}}}})
	})
	mux.HandleFunc("POST /api/v0/chat/completion", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-DS-PoW-Response"))
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		f.mu.Lock()
		sig, _ := p["signature"].(string)
		reused := f.powUsed[sig]
		f.powUsed[sig] = true
		f.mu.Unlock()
		if p["answer"] != float64(powAnswer) || reused {
			f.t.Errorf("bad or reused PoW response: %v (reused=%v)", p, reused)
			w.WriteHeader(422)
			return
		}
		var body deepseek.CompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, body)
		var reply func(http.ResponseWriter)
		if len(f.replies) > 0 {
			reply, f.replies = f.replies[0], f.replies[1:]
		}
		f.msgID++
		id := f.msgID
		f.mu.Unlock()
		if reply == nil {
			f.t.Errorf("unexpected completion call #%d: %q", len(f.calls), body.Prompt)
			reply = text("unexpected")
		}
		w.Header().Set("X-Msg-Id", fmt.Sprint(id))
		reply(w)
	})
	return mux
}

func sseHead(w http.ResponseWriter) int {
	id := 0
	fmt.Sscan(w.Header().Get("X-Msg-Id"), &id)
	w.Header().Del("X-Msg-Id")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fmt.Fprintf(w, "data: {\"request_message_id\":%d,\"response_message_id\":%d}\n\n", id*10, id*10+1)
	return id
}

// text replies with fragment-protocol output, optionally with reasoning.
func reply(content, reasoning, finish string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		sseHead(w)
		if reasoning != "" {
			b, _ := json.Marshal(map[string]any{"v": map[string]any{"response": map[string]any{"fragments": []any{map[string]any{"type": "THINK", "content": reasoning}}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		for i, chunk := range chunks(content, 7) {
			var b []byte
			if i == 0 {
				b, _ = json.Marshal(map[string]any{"p": "response/fragments", "o": "APPEND", "v": []any{map[string]any{"type": "RESPONSE", "content": chunk}}})
			} else {
				b, _ = json.Marshal(map[string]any{"p": "response/fragments/-1/content", "o": "APPEND", "v": chunk})
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		if finish != "" {
			fmt.Fprintf(w, "data: {\"p\":\"response/finish_reason\",\"v\":%q}\n\n", finish)
		}
		fmt.Fprint(w, "data: {\"p\":\"response/status\",\"o\":\"SET\",\"v\":\"FINISHED\"}\n\n")
	}
}

func text(content string) func(http.ResponseWriter) { return reply(content, "", "") }

func modelError(msg string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		sseHead(w)
		b, _ := json.Marshal(map[string]any{"type": "error", "content": msg})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
}

func status(code int, retryAfter string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Del("X-Msg-Id")
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
		fmt.Fprint(w, `{"code":40003,"msg":"nope"}`)
	}
}

func chunks(s string, n int) []string {
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += n {
		out = append(out, string(r[i:min(len(r), i+n)]))
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// ---------------------------------------------------------------------------
// Harness

type harness struct {
	t     *testing.T
	fake  *fakeDeepSeek
	srv   *httptest.Server
	store *state.Store
	cfg   *config.Config
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	fake := &fakeDeepSeek{t: t, powUsed: map[string]bool{}}
	up := httptest.NewServer(fake.handler())
	t.Cleanup(up.Close)
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["DEEPSEEK_MEDIA_INJECTION"]; !ok {
		env["DEEPSEEK_MEDIA_INJECTION"] = "0"
	}
	cfg, err := config.Load(config.MapEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(cfg.AccountCooldown)
	store.SetAccounts([]*state.Account{{ID: "account_1", Creds: deepseek.Credentials{Token: "tok", Cookie: "c=1"}}})
	ds := deepseek.NewClient()
	ds.BaseURL = up.URL
	ds.Timeout = 5 * time.Second
	ds.IdleTimeout = 5 * time.Second
	s := New(cfg, store, ds)
	s.sleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, fake: fake, srv: srv, store: store, cfg: cfg}
}

func (h *harness) queue(replies ...func(http.ResponseWriter)) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	h.fake.replies = append(h.fake.replies, replies...)
}

func (h *harness) calls() []deepseek.CompletionRequest {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return append([]deepseek.CompletionRequest(nil), h.fake.calls...)
}

func (h *harness) post(path string, body any, headers ...string) (*http.Response, string) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func get(v any, path ...any) any {
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

type sseEvent struct {
	Name string
	Data string
}

func parseSSE(body string) []sseEvent {
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Name = line[7:]
		case strings.HasPrefix(line, "data: "):
			cur.Data = line[6:]
		case line == "":
			if cur.Data != "" {
				out = append(out, cur)
			}
			cur = sseEvent{}
		}
	}
	return out
}

var readTool = map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "description": "Read a file",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}

// ---------------------------------------------------------------------------
// Tests

func TestChatCompletionReusesRemoteSession(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(text("Hello there"), text("Second"))
	resp, body := h.post("/v1/chat/completions", map[string]any{"model": "deepseek-chat", "user": "agent-1", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	m := decode(t, body)
	if get(m, "choices", 0, "message", "content") != "Hello there" || get(m, "choices", 0, "finish_reason") != "stop" || get(m, "object") != "chat.completion" {
		t.Errorf("unexpected: %s", body)
	}
	if get(m, "usage", "prompt_tokens") != float64(1) {
		t.Errorf("usage: %v", get(m, "usage"))
	}
	h.post("/v1/chat/completions", map[string]any{"user": "agent-1", "messages": []any{map[string]any{"role": "user", "content": "again"}}})
	calls := h.calls()
	if len(calls) != 2 || calls[0].ChatSessionID != calls[1].ChatSessionID {
		t.Fatalf("session not reused: %+v", calls)
	}
	if calls[0].ParentMessageID != nil || fmt.Sprint(calls[1].ParentMessageID) != "11" {
		t.Errorf("parent ids: %v %v", calls[0].ParentMessageID, calls[1].ParentMessageID)
	}
	if calls[1].Prompt != "User: again" {
		t.Errorf("healthy session should get the incremental prompt, got %q", calls[1].Prompt)
	}
	_, sessions := httpGet(t, h.srv.URL+"/v1/sessions")
	if get(decode(t, sessions), "agents", 0, "agent") != "agent-1" {
		t.Errorf("sessions: %s", sessions)
	}
}

func httpGet(t *testing.T, url string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestReasonerModelSettingsAndReasoning(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply("42", "thinking hard", ""))
	_, body := h.post("/v1/chat/completions", map[string]any{"model": "DeepSeek-Reasoner", "messages": []any{map[string]any{"role": "user", "content": "q"}}})
	m := decode(t, body)
	if get(m, "choices", 0, "message", "reasoning_content") != "thinking hard" || get(m, "model") != "deepseek-reasoner" {
		t.Errorf("%s", body)
	}
	if c := h.calls()[0]; !c.ThinkingEnabled || c.SearchEnabled || c.ModelType != "default" {
		t.Errorf("upstream flags: %+v", c)
	}
}

func TestOpenAILiveStreaming(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply("Streaming works fine", "plan", ""))
	resp, body := h.post("/v1/chat/completions", map[string]any{"model": "deepseek-r1", "stream": true, "stream_options": map[string]any{"include_usage": true},
		"messages": []any{map[string]any{"role": "user", "content": "q"}}})
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q: %s", ct, body)
	}
	events := parseSSE(body)
	var content, reasoning strings.Builder
	var finish any
	var sawUsage bool
	for _, e := range events {
		if e.Data == "[DONE]" {
			continue
		}
		m := decode(t, e.Data)
		if c, ok := get(m, "choices", 0, "delta", "content").(string); ok {
			content.WriteString(c)
		}
		if r, ok := get(m, "choices", 0, "delta", "reasoning_content").(string); ok {
			reasoning.WriteString(r)
		}
		if f := get(m, "choices", 0, "finish_reason"); f != nil {
			finish = f
		}
		if get(m, "usage") != nil {
			sawUsage = true
		}
	}
	if content.String() != "Streaming works fine" || reasoning.String() != "plan" || finish != "stop" || !sawUsage {
		t.Errorf("content=%q reasoning=%q finish=%v usage=%v", content.String(), reasoning.String(), finish, sawUsage)
	}
	if events[len(events)-1].Data != "[DONE]" {
		t.Error("missing [DONE]")
	}
	// Live streaming delivers several content deltas, not one buffered blob.
	n := strings.Count(body, `"content":`)
	if n < 3 {
		t.Errorf("expected incremental deltas, got %d", n)
	}
}

func TestOpenAIToolCall(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply(`{"tool_call":{"name":"read_file","arguments":{"path":"/etc/hosts"}}}`, "I should read", ""))
	_, body := h.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "read hosts"}}, "tools": []any{readTool}})
	m := decode(t, body)
	if get(m, "choices", 0, "finish_reason") != "tool_calls" || get(m, "choices", 0, "message", "tool_calls", 0, "function", "name") != "read_file" {
		t.Fatalf("%s", body)
	}
	if get(m, "choices", 0, "message", "content") != nil || get(m, "choices", 0, "message", "reasoning_content") != nil {
		t.Errorf("tool turn must have null content and no reasoning: %s", body)
	}
	if args := get(m, "choices", 0, "message", "tool_calls", 0, "function", "arguments"); args != `{"path":"/etc/hosts"}` {
		t.Errorf("args %v", args)
	}
	if p := h.calls()[0].Prompt; !strings.Contains(p, "--- TOOL REQUEST SYSTEM ---") || !strings.Contains(p, "## read_file") {
		t.Errorf("tool adapter not injected: %q", p)
	}

	// Streaming tool calls carry an index and finish with tool_calls.
	h.queue(text(`TOOL_CALL: read_file` + "\n" + `arguments: {"path":"/a"}`))
	_, body = h.post("/v1/chat/completions", map[string]any{"stream": true, "messages": []any{map[string]any{"role": "user", "content": "x"}}, "tools": []any{readTool}})
	if !strings.Contains(body, `"index":0`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("stream tool call: %s", body)
	}
}

func TestUnknownToolIsRepairedOrRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(text(`<|DSML|tool_calls><|DSML|invoke name="read_file"><|DSML|parameter name="path">/tmp`), // truncated
		text(`<|DSML|tool_calls><|DSML|invoke name="read_file">`)) // still broken
	resp, body := h.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "x"}}, "tools": []any{readTool}})
	if resp.StatusCode != 502 || get(decode(t, body), "error", "type") != "malformed_tool_call" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	calls := h.calls()
	if len(calls) != 2 || !strings.Contains(calls[1].Prompt, "[STRICT INSTRUCTION]") || calls[1].ChatSessionID == calls[0].ChatSessionID {
		t.Errorf("strict retry must use a fresh session: %+v", calls)
	}

	h2 := newHarness(t, nil)
	h2.queue(text(`<tool_calls><invoke name="read_file"><parameter name="path">/x`), text(`{"tool_call":{"name":"read_file","arguments":{"path":"/x"}}}`))
	_, body = h2.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "x"}}, "tools": []any{readTool}})
	if get(decode(t, body), "choices", 0, "finish_reason") != "tool_calls" {
		t.Errorf("repair failed: %s", body)
	}
}

func TestEmptyResponseRetriesWithFreshSession(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(text(""), text("recovered"))
	_, body := h.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "q"}}})
	if get(decode(t, body), "choices", 0, "message", "content") != "recovered" {
		t.Fatalf("%s", body)
	}
	calls := h.calls()
	if len(calls) != 2 || calls[0].ChatSessionID == calls[1].ChatSessionID || calls[1].ParentMessageID != nil {
		t.Errorf("retry must use a new remote chat: %+v", calls)
	}
}

func TestEmptyResponseGivesUpWithDiagnostics(t *testing.T) {
	h := newHarness(t, map[string]string{"DEEPSEEK_MAX_RETRIES": "1"})
	h.queue(text(""), text(""))
	resp, body := h.post("/v1/chat/completions", map[string]any{"user": "u", "messages": []any{map[string]any{"role": "user", "content": "q"}}})
	e := decode(t, body)
	if resp.StatusCode != 502 || get(e, "error", "type") != "empty_response" || get(e, "error", "retry_attempts") != float64(1) || get(e, "error", "agent") != "u" {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

func TestContextTooLongRetriesSmallerThenFails(t *testing.T) {
	h := newHarness(t, map[string]string{"DEEPSEEK_MAX_RETRIES": "2", "DEEPSEEK_MAX_PROMPT_CHARS": "20000"})
	tooLong := modelError("Содержание слишком длинное. Сократите его и попробуйте снова.")
	h.queue(tooLong, tooLong, tooLong)
	big := strings.Repeat("x", 30000)
	resp, body := h.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "TASK " + big + " END"}}})
	if resp.StatusCode != 400 || get(decode(t, body), "error", "type") != "context_length_exceeded" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp.Header.Get(ContextCompactedHeader) != "true" {
		t.Error("compaction header missing")
	}
	calls := h.calls()
	if len(calls) != 3 {
		t.Fatalf("calls=%d", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if len([]rune(calls[i].Prompt)) > len([]rune(calls[i-1].Prompt)) {
			t.Errorf("retry %d prompt grew", i)
		}
	}
	if !strings.Contains(calls[2].Prompt, "END") || !strings.Contains(calls[2].Prompt, "TASK") {
		t.Error("compaction must keep both edges")
	}
}

func TestUpstreamRateLimitCoolsAccount(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(status(429, "30"))
	resp, body := h.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "q"}}})
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "30" || get(decode(t, body), "error", "type") != "rate_limit_error" {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	resp, body = h.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "q"}}})
	if resp.StatusCode != 429 || get(decode(t, body), "error", "type") != "rate_limit" || resp.Header.Get("Retry-After") == "" {
		t.Errorf("pool exhaustion: %d %s", resp.StatusCode, body)
	}
	if r, _ := httpGet(t, h.srv.URL+"/readyz"); r.StatusCode != 503 {
		t.Errorf("readyz = %d", r.StatusCode)
	}
}

func TestExpiredRemoteSessionIsRecreatedWithRecoveryHistory(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(text("first answer"), status(404, ""), text("after recreate"))
	h.post("/v1/chat/completions", map[string]any{"user": "a", "messages": []any{map[string]any{"role": "user", "content": "first question"}}})
	_, body := h.post("/v1/chat/completions", map[string]any{"user": "a", "messages": []any{map[string]any{"role": "user", "content": "second question"}}})
	if get(decode(t, body), "choices", 0, "message", "content") != "after recreate" {
		t.Fatalf("%s", body)
	}
	calls := h.calls()
	last := calls[len(calls)-1]
	if last.ChatSessionID == calls[0].ChatSessionID || last.ParentMessageID != nil {
		t.Errorf("not recreated: %+v", last)
	}
	for _, want := range []string{"[Previous conversation]", "first question", "first answer", "second question"} {
		if !strings.Contains(last.Prompt, want) {
			t.Errorf("recovery prompt missing %q: %q", want, last.Prompt)
		}
	}
}

func TestLengthFinishTriggersContinuation(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply("part one", "", "length"), text("part two"))
	_, body := h.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "long please"}}})
	m := decode(t, body)
	if get(m, "choices", 0, "message", "content") != "part one\npart two" || get(m, "choices", 0, "finish_reason") != "stop" {
		t.Errorf("%s", body)
	}
	if calls := h.calls(); calls[1].Prompt != "continue" {
		t.Errorf("continuation prompt %q", calls[1].Prompt)
	}
}

func TestAnthropicMessages(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply("Hi from Claude shim", "hmm", ""))
	_, body := h.post("/v1/messages", map[string]any{"model": "deepseek-reasoner", "max_tokens": 64, "system": []any{map[string]any{"type": "text", "text": "SYS"}},
		"metadata": map[string]any{"user_id": "claude"}, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	m := decode(t, body)
	if get(m, "type") != "message" || get(m, "content", 0, "text") != "Hi from Claude shim" || get(m, "stop_reason") != "end_turn" || get(m, "reasoning_content") != "hmm" {
		t.Errorf("%s", body)
	}
	if p := h.calls()[0].Prompt; !strings.HasPrefix(p, "SYS\n\nUser: hello") {
		t.Errorf("prompt %q", p)
	}

	// Tool round trip: tool_use history + tool_result, streaming tool-only turn.
	h.queue(reply(`{"tool_call":{"name":"write_file","arguments":{"path":"a.py","content":"print(1)"}}}`, "reasoning must not leak", ""))
	_, body = h.post("/v1/messages", map[string]any{"stream": true, "metadata": map[string]any{"user_id": "claude"},
		"tools": []any{map[string]any{"name": "write_file", "description": "Write", "input_schema": map[string]any{"type": "object"}}},
		"messages": []any{
			map[string]any{"role": "user", "content": "make file"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "ok"}, map[string]any{"type": "tool_use", "id": "tu1", "name": "write_file", "input": map[string]any{"path": "x"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "done"}, map[string]any{"type": "text", "text": "now a.py"}}},
		}})
	if !strings.Contains(body, `"type":"tool_use"`) || strings.Contains(body, "text_delta") || strings.Contains(body, "reasoning") {
		t.Errorf("tool stream must be tool_use only: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) || !strings.Contains(body, "event: message_stop") {
		t.Errorf("stream end: %s", body)
	}
	p := h.calls()[1].Prompt
	for _, want := range []string{"Assistant: ok", "TOOL_CALL: write_file", "[Tool Result]\ndone", "User: now a.py"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestAnthropicLiveTextStream(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply("Answer text", "why", ""))
	_, body := h.post("/v1/messages", map[string]any{"stream": true, "model": "deepseek-r1", "messages": []any{map[string]any{"role": "user", "content": "q"}}})
	events := parseSSE(body)
	if events[0].Name != "message_start" || events[len(events)-1].Name != "message_stop" {
		t.Fatalf("framing: %s", body)
	}
	var text strings.Builder
	for _, e := range events {
		if e.Name == "content_block_delta" {
			text.WriteString(get(decode(t, e.Data), "delta", "text").(string))
		}
	}
	if text.String() != "[reasoning]\nwhy\n[/reasoning]\nAnswer text" {
		t.Errorf("text %q", text.String())
	}
	if strings.Count(body, "event: content_block_start") != 2 || strings.Count(body, "event: content_block_stop") != 2 {
		t.Errorf("expected two blocks: %s", body)
	}
}

func TestResponsesAPI(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(reply("Resp text", "r", ""))
	_, body := h.post("/v1/responses", map[string]any{"model": "deepseek-reasoner", "instructions": "INSTR", "input": "hello"})
	m := decode(t, body)
	if get(m, "object") != "response" || get(m, "output_text") != "Resp text" || get(m, "output", 0, "type") != "reasoning" || get(m, "output", 1, "content", 0, "text") != "Resp text" {
		t.Errorf("%s", body)
	}
	if !strings.HasPrefix(get(m, "id").(string), "resp_") {
		t.Errorf("id %v", get(m, "id"))
	}

	h.queue(text(`{"tool_call":{"name":"get_time","arguments":{"tz":"UTC"}}}`))
	_, body = h.post("/v1/responses", map[string]any{"stream": true, "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "time?"}}},
	}, "tools": []any{map[string]any{"type": "function", "name": "get_time", "parameters": map[string]any{"type": "object"}}}})
	if !strings.Contains(body, "response.function_call_arguments.done") || !strings.Contains(body, `"call_id":"call_`) || strings.Contains(body, `"type":"reasoning"`) {
		t.Errorf("responses tool stream: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Error("missing [DONE]")
	}

	h.queue(text("live text"))
	_, body = h.post("/v1/responses", map[string]any{"stream": true, "input": "hi"})
	var completed map[string]any
	for _, e := range parseSSE(body) {
		if e.Name == "response.completed" {
			completed = decode(t, e.Data)
		}
	}
	if get(completed, "response", "output_text") != "live text" || get(completed, "response", "output", 0, "content", 0, "text") != "live text" {
		t.Errorf("completed: %v", completed)
	}
}

func TestResponsesFunctionCallHistoryReachesModel(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(text("It is noon"))
	h.post("/v1/responses", map[string]any{"input": []any{
		map[string]any{"role": "user", "content": "time?"},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "get_time", "arguments": `{"tz":"UTC"}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "12:00"},
	}})
	if p := h.calls()[0].Prompt; !strings.Contains(p, "TOOL_CALL: get_time") || !strings.Contains(p, "[Tool Result]\n12:00") {
		t.Errorf("prompt %q", p)
	}
}

func TestNewCommandResetsSession(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(text("a"))
	h.post("/v1/chat/completions", map[string]any{"user": "n", "messages": []any{map[string]any{"role": "user", "content": "q"}}})
	_, body := h.post("/v1/chat/completions", map[string]any{"user": "n", "messages": []any{map[string]any{"role": "user", "content": " /new "}}})
	if !strings.Contains(get(decode(t, body), "choices", 0, "message", "content").(string), "Started a new chat") {
		t.Errorf("%s", body)
	}
	if len(h.calls()) != 1 {
		t.Error("/new must not reach DeepSeek")
	}
	_, sessions := httpGet(t, h.srv.URL+"/v1/sessions")
	if get(decode(t, sessions), "agents", 0, "history_size") != float64(0) {
		t.Errorf("history not cleared: %s", sessions)
	}
}

func TestModelValidation(t *testing.T) {
	h := newHarness(t, nil)
	resp, body := h.post("/v1/chat/completions", map[string]any{"model": "gpt-4", "messages": []any{}})
	if resp.StatusCode != 400 || get(decode(t, body), "error", "type") != "invalid_model" {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	resp, body = h.post("/v1/chat/completions", map[string]any{"model": "deepseek-vision", "messages": []any{}})
	if resp.StatusCode != 400 || get(decode(t, body), "error", "type") != "unsupported_model" {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/chat/completions", strings.NewReader("{not json"))
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("bad JSON status %d", r.StatusCode)
	}
}

func TestProxyKeyAndCORS(t *testing.T) {
	h := newHarness(t, map[string]string{"PROXY_API_KEY": "secret", "PROXY_CORS_ORIGINS": "https://ui.example.com"})
	resp, body := h.post("/v1/chat/completions", map[string]any{})
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("no key: %d %s", resp.StatusCode, body)
	}
	resp, _ = httpGet(t, h.srv.URL+"/v1/models", "X-Api-Key", "secret")
	if resp.StatusCode != 200 {
		t.Errorf("x-api-key: %d", resp.StatusCode)
	}
	resp, body = httpGet(t, h.srv.URL+"/health")
	if resp.StatusCode != 200 || strings.Contains(body, "accounts") {
		t.Errorf("public health must hide private status: %s", body)
	}
	_, body = httpGet(t, h.srv.URL+"/health", "Authorization", "Bearer secret")
	if !strings.Contains(body, "account_1") {
		t.Errorf("authorized health: %s", body)
	}
	resp, _ = httpGet(t, h.srv.URL+"/health", "Origin", "https://evil.example")
	if resp.StatusCode != 403 {
		t.Errorf("evil origin: %d", resp.StatusCode)
	}
	resp, _ = httpGet(t, h.srv.URL+"/health", "Origin", "https://ui.example.com")
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://ui.example.com" || resp.Header.Get("Access-Control-Expose-Headers") != ContextCompactedHeader {
		t.Errorf("cors headers: %v", resp.Header)
	}
	req, _ := http.NewRequest(http.MethodOptions, h.srv.URL+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 204 {
		t.Errorf("preflight %d", r.StatusCode)
	}
}

func TestResetSessionEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	h.queue(text("a"))
	h.post("/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "q"}}}, "X-Agent-Session", "s1")
	resp, body := h.post("/reset-session?agent=s1", nil)
	if resp.StatusCode != 200 || get(decode(t, body), "history_preserved") != float64(1) {
		t.Errorf("%s", body)
	}
	resp, _ = h.post("/reset-session?agent=nobody", nil)
	if resp.StatusCode != 404 {
		t.Errorf("unknown agent %d", resp.StatusCode)
	}
	_, body = h.post("/reset-session?agent=all", nil)
	if get(decode(t, body), "count") != float64(1) {
		t.Errorf("%s", body)
	}
}

func TestModelsEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	_, body := httpGet(t, h.srv.URL+"/v1/models")
	m := decode(t, body)
	data := get(m, "data").([]any)
	if len(data) != len(deepseek.SupportedModelIDs()) {
		t.Errorf("models: %d", len(data))
	}
	_, body = httpGet(t, h.srv.URL+"/v1/model-capabilities")
	if get(decode(t, body), "data", "deepseek-vision", "supported") != false {
		t.Errorf("%s", body)
	}
}
