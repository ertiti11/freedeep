// Package deepseek talks to the private DeepSeek Web chat API.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ertiti11/freedeep/internal/pow"
)

const (
	DefaultBaseURL = "https://chat.deepseek.com"
	DefaultWasmURL = "https://fe-static.deepseek.com/chat/static/sha3_wasm_bg.7b9ca65ddd.wasm"
	completionPath = "/api/v0/chat/completion"
	userAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
)

// Credentials is the content of a deepseek-auth.json file.
type Credentials struct {
	Token   string `json:"token"`
	HifDliq string `json:"hif_dliq"`
	HifLeim string `json:"hif_leim"`
	Cookie  string `json:"cookie"`
	WasmURL string `json:"wasmUrl"`
}

// Ready reports whether the credentials can authenticate at all.
func (c Credentials) Ready() bool { return c.Token != "" && c.Cookie != "" }

// Client is a DeepSeek Web API client. It is safe for concurrent use.
type Client struct {
	HTTP           *http.Client
	BaseURL        string
	Locale         string
	TimezoneOffset string
	// Timeout bounds small JSON calls and the wait for completion headers.
	Timeout time.Duration
	// IdleTimeout aborts a completion stream that sends nothing for this long.
	IdleTimeout time.Duration
}

// NewClient returns a client with sane defaults.
func NewClient() *Client {
	return &Client{
		HTTP:           &http.Client{Transport: http.DefaultTransport},
		BaseURL:        DefaultBaseURL,
		Locale:         "ru",
		TimezoneOffset: "14400",
		Timeout:        60 * time.Second,
		IdleTimeout:    90 * time.Second,
	}
}

func (c *Client) headers(cred Credentials) http.Header {
	h := http.Header{}
	h.Set("User-Agent", userAgent)
	h.Set("x-client-platform", "web")
	h.Set("x-client-version", "2.0.0")
	h.Set("x-client-locale", c.Locale)
	h.Set("x-client-timezone-offset", c.TimezoneOffset)
	h.Set("x-app-version", "2.0.0")
	h.Set("Authorization", "Bearer "+cred.Token)
	h.Set("x-hif-dliq", cred.HifDliq)
	h.Set("x-hif-leim", cred.HifLeim)
	h.Set("Origin", "https://chat.deepseek.com")
	h.Set("Referer", "https://chat.deepseek.com/")
	h.Set("Cookie", cred.Cookie)
	h.Set("Content-Type", "application/json")
	return h
}

// UpstreamError is a non-success answer from DeepSeek.
type UpstreamError struct {
	Status     int
	Label      string
	Body       string
	RetryAfter string
	Hint       string
}

func (e *UpstreamError) Error() string {
	detail := strings.Join(strings.Fields(e.Body), " ")
	if len(detail) > 300 {
		detail = detail[:300]
	}
	msg := fmt.Sprintf("DeepSeek upstream HTTP %d", e.Status)
	if e.Label != "" {
		msg = fmt.Sprintf("DeepSeek %s failed: HTTP %d", e.Label, e.Status)
	}
	if detail != "" {
		msg += ": " + detail
	}
	if e.Hint != "" {
		msg += ". " + e.Hint
	}
	return msg
}

// Type is the OpenAI-style error type for this status.
func (e *UpstreamError) Type() string {
	switch e.Status {
	case 429:
		return "rate_limit_error"
	case 401, 403:
		return "authentication_error"
	}
	return "upstream_http_error"
}

const authHint = "Run `freedeep doctor`; if auth expired, run `freedeep login` or `freedeep import`"

func (c *Client) postJSON(ctx context.Context, cred Credentials, path, label string, body any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(cred)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DeepSeek %s: %w", label, err)
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("DeepSeek %s: %w", label, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &UpstreamError{Status: resp.StatusCode, Label: label, Body: string(text), RetryAfter: resp.Header.Get("Retry-After"), Hint: authHint}
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		// A 200 HTML page usually means a captcha or login wall.
		return nil, &UpstreamError{Status: resp.StatusCode, Label: label + " (non-JSON response)", Body: string(text), Hint: authHint}
	}
	return out, nil
}

func dig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// CreatePoW requests a proof-of-work challenge for the completion endpoint.
func (c *Client) CreatePoW(ctx context.Context, cred Credentials) (pow.Challenge, error) {
	var ch pow.Challenge
	out, err := c.postJSON(ctx, cred, "/api/v0/chat/create_pow_challenge", "PoW challenge", map[string]string{"target_path": completionPath})
	if err != nil {
		return ch, err
	}
	raw := dig(out, "data", "biz_data", "challenge")
	if raw == nil {
		return ch, fmt.Errorf("DeepSeek PoW response has no data.biz_data.challenge: auth may be expired, a captcha may be required, or DeepSeek changed its Web API. %s", authHint)
	}
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &ch); err != nil {
		return ch, fmt.Errorf("DeepSeek PoW challenge has an unexpected shape: %w", err)
	}
	if ch.TargetPath == "" {
		ch.TargetPath = completionPath
	}
	return ch, nil
}

// SolvePoW fetches and solves a challenge, returning the header value.
func (c *Client) SolvePoW(ctx context.Context, cred Credentials) (string, error) {
	ch, err := c.CreatePoW(ctx, cred)
	if err != nil {
		return "", err
	}
	answer, err := pow.Solve(ch)
	if err != nil {
		return "", err
	}
	return pow.Header(ch, answer), nil
}

// CreateSession creates a new remote chat session and returns its id.
func (c *Client) CreateSession(ctx context.Context, cred Credentials) (string, error) {
	out, err := c.postJSON(ctx, cred, "/api/v0/chat_session/create", "chat session create", map[string]any{})
	if err != nil {
		return "", err
	}
	id := dig(out, "data", "biz_data", "chat_session", "id")
	if id == nil {
		id = dig(out, "data", "biz_data", "id")
	}
	s, _ := id.(string)
	if s == "" {
		return "", fmt.Errorf("could not create DeepSeek chat session: response has no session id. %s", authHint)
	}
	return s, nil
}

// CompletionRequest is the body of /api/v0/chat/completion.
type CompletionRequest struct {
	ChatSessionID   string `json:"chat_session_id"`
	ParentMessageID any    `json:"parent_message_id"`
	ModelType       string `json:"model_type"`
	Prompt          string `json:"prompt"`
	RefFileIDs      []any  `json:"ref_file_ids"`
	ThinkingEnabled bool   `json:"thinking_enabled"`
	SearchEnabled   bool   `json:"search_enabled"`
	Action          any    `json:"action"`
	Preempt         bool   `json:"preempt"`
}

// Completion starts a completion and returns the SSE body. The caller must
// close it. Non-200 answers are returned as *UpstreamError (body consumed).
// The body is wrapped with an idle timeout: a stalled stream errors out
// instead of pinning the request and its account forever.
func (c *Client) Completion(ctx context.Context, cred Credentials, powHeader string, body CompletionRequest) (io.ReadCloser, error) {
	if body.RefFileIDs == nil {
		body.RefFileIDs = []any{}
	}
	payload, _ := json.Marshal(body)
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+completionPath, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header = c.headers(cred)
	req.Header.Set("X-DS-PoW-Response", powHeader)

	var headerTimeout atomic.Bool
	headerTimer := time.AfterFunc(c.Timeout, func() { headerTimeout.Store(true); cancel() })
	resp, err := c.HTTP.Do(req)
	headerTimer.Stop()
	if err != nil {
		cancel()
		if headerTimeout.Load() {
			return nil, fmt.Errorf("DeepSeek completion timed out waiting for response headers: %w", err)
		}
		return nil, fmt.Errorf("DeepSeek completion: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		cancel()
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(text), RetryAfter: resp.Header.Get("Retry-After")}
	}
	return newIdleReader(resp.Body, c.IdleTimeout, cancel), nil
}

// idleReader cancels the request when no bytes arrive within timeout.
type idleReader struct {
	rc      io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
	cancel  context.CancelFunc
	fired   chan struct{}
}

func newIdleReader(rc io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleReader {
	r := &idleReader{rc: rc, timeout: timeout, cancel: cancel, fired: make(chan struct{})}
	if timeout > 0 {
		r.timer = time.AfterFunc(timeout, func() { close(r.fired); cancel() })
	}
	return r
}

// ErrIdleTimeout is returned when the upstream stream stalls.
var ErrIdleTimeout = fmt.Errorf("DeepSeek stream timed out (no data received)")

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if r.timer != nil {
		select {
		case <-r.fired:
			return n, ErrIdleTimeout
		default:
		}
		if n > 0 {
			r.timer.Reset(r.timeout)
		}
	}
	return n, err
}

func (r *idleReader) Close() error {
	if r.timer != nil {
		r.timer.Stop()
	}
	err := r.rc.Close()
	r.cancel()
	return err
}
