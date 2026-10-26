package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ertiti11/freedeep/internal/chat"
	"github.com/ertiti11/freedeep/internal/config"
	"github.com/ertiti11/freedeep/internal/deepseek"
	"github.com/ertiti11/freedeep/internal/pow"
	"github.com/ertiti11/freedeep/internal/prompt"
	"github.com/ertiti11/freedeep/internal/state"
	"github.com/ertiti11/freedeep/internal/toolcall"
)

const maxContinuations = 2

func sanitize(s string) string { return strings.ToValidUTF8(s, "") }

// exchange is the state of one inbound completion request.
type exchange struct {
	s         *Server
	ctx       context.Context
	w         http.ResponseWriter
	mode      apiMode
	req       *chat.Request
	model     *deepseek.Model
	agent     string
	log       *slog.Logger
	sess      *state.Session
	started   time.Time
	compacted bool
	live      *liveTracker
}

func (x *exchange) deadlineHit() bool { return time.Since(x.started) > x.s.cfg.RequestDeadline }
func (x *exchange) clientGone() bool  { return x.ctx.Err() != nil }

func (x *exchange) markCompacted() {
	x.compacted = true
	x.w.Header().Set(ContextCompactedHeader, "true")
}

func (x *exchange) maxChars() int { return x.s.cfg.MaxPromptChars }

func (s *Server) completionHandler(mode apiMode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Backpressure: reject rather than fan out unbounded upstream work.
		if n := s.inFlight.Add(1); n > int64(s.cfg.MaxConcurrent) {
			s.inFlight.Add(-1)
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusServiceUnavailable, errorBody(
				fmt.Sprintf("Server busy (%d/%d requests in flight). Retry shortly.", n-1, s.cfg.MaxConcurrent), "overloaded", nil))
			return
		}
		defer s.inFlight.Add(-1)

		req, err := parseRequest(mode, http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeJSON(w, http.StatusRequestEntityTooLarge, errorBody("Request body too large", "payload_too_large", nil))
				return
			}
			writeJSON(w, http.StatusBadRequest, errorBody("Invalid JSON request body: "+err.Error(), "invalid_request_error", nil))
			return
		}
		model, known := deepseek.LookupModel(req.Model)
		if !known {
			writeJSON(w, http.StatusBadRequest, errorBody("Unknown model: "+req.Model, "invalid_model", map[string]any{
				"supported_models": deepseek.SupportedModelIDs(), "model_capabilities_url": "/v1/model-capabilities"}))
			return
		}
		if !model.Supported {
			writeJSON(w, http.StatusBadRequest, errorBody(req.Model+" is not currently supported through this DeepSeek Web API path", "unsupported_model", map[string]any{
				"model": req.Model, "real_model": model.RealModel, "reason": model.UnavailableReason,
				"capabilities": model.Capabilities, "supported_models": deepseek.SupportedModelIDs()}))
			return
		}
		agent := agentID(r, req.Session, req.User)
		x := &exchange{s: s, ctx: r.Context(), w: w, mode: mode, req: req, model: model, agent: agent,
			log: slog.With("agent", agent), started: time.Now()}
		x.run()
	}
}

// respond writes a complete response in the request's dialect.
func (x *exchange) respond(c *Completion) {
	if x.req.Stream {
		replay(newStreamWriter(x.mode, x.w, c, x.req.IncludeUsage), c)
		return
	}
	switch x.mode {
	case modeAnthropic:
		writeJSON(x.w, http.StatusOK, toAnthropic(c))
	case modeResponses:
		writeJSON(x.w, http.StatusOK, toResponses(c))
	default:
		writeJSON(x.w, http.StatusOK, c)
	}
}

// fail reports an error, as JSON or (once streaming began) as an SSE event.
func (x *exchange) fail(status int, errType, message string, extra map[string]any) {
	if x.live != nil && x.live.sw.started() {
		x.live.sw.fail(status, errType, message)
		return
	}
	writeJSON(x.w, status, errorBody(message, errType, extra))
}

func lastUserText(messages []chat.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			s, _ := messages[i].Content.(string)
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func (x *exchange) run() {
	s, req := x.s, x.req
	// "/new" resets the agent's DeepSeek session and history locally.
	if lastUserText(req.Messages) == "/new" {
		n := s.store.ReplaceSession(x.agent)
		x.log.Info("/new received: session reset", "history_cleared", n)
		x.respond(buildTextResponse("Started a new chat. Session and history have been reset.", "/new", req.Model, "", ""))
		return
	}

	opts := toolcall.FormatOptions{Host: s.host, IP: s.ip, MaxPromptChars: x.maxChars()}
	conv, system := prompt.Format(req.Messages, req.Tools, opts)
	// Usage counts what the client sent, not the proxy-expanded prompt.
	var clientParts []string
	for _, m := range req.Messages {
		clientParts = append(clientParts, chat.NormalizeContent(m.Content))
	}
	clientPrompt := strings.Join(clientParts, "\n")

	allowed := map[string]bool{}
	for _, t := range req.Tools {
		if t.IsFunction() {
			allowed[t.Name] = true
		}
	}
	// Without tools the answer is streamed live. With tools it can still be
	// streamed (guarded), but the full text is kept to detect a tool call.
	if req.Stream && (len(allowed) == 0 || s.cfg.StreamTools) {
		skeleton := newCompletion(req.Model)
		skeleton.Usage.PromptTokens = estimateTokens(clientPrompt)
		lt := &liveTracker{sw: newStreamWriter(x.mode, x.w, skeleton, req.IncludeUsage), log: x.log}
		if len(allowed) == 0 {
			lt.hideReasoning = !s.cfg.ShowReasoning
		} else {
			// A tool turn interleaves reasoning before content is classified;
			// streaming it would force reasoning ahead of a possible tool_use
			// block, which some agents mistake for a final answer. Reasoning is
			// never streamed while guarding, independent of DEEPSEEK_SHOW_REASONING.
			lt.guard = true
			lt.hideReasoning = true
		}
		x.live = lt
	}

	x.sess = s.store.Session(x.agent)
	if ro := s.store.PrepareForPrompt(x.sess); ro != nil {
		x.log.Info("session rolled over before prompt build; recovery history preserved", "session", ro.FailedSessionID, "reason", ro.Reason)
	}
	remote := s.store.Remote(x.sess)
	// Keep a recovery prompt even while the remote chat is healthy: if it
	// expires mid-request, the replacement must receive local history.
	recoveryPrefix := ""
	if !prompt.HasExplicitHistory(req.Messages) {
		recoveryPrefix = prompt.RecoveryHistoryPrefix(remote.History)
	}
	historyPrefix := ""
	if remote.ID == "" {
		historyPrefix = recoveryPrefix
	}
	built := prompt.BuildBounded(system, historyPrefix, conv, x.maxChars())
	fresh := prompt.BuildBounded(system, recoveryPrefix, conv, x.maxChars())
	fullPrompt := built.Prompt
	if built.Compacted {
		x.markCompacted()
		x.log.Info("compacted upstream prompt", "from", built.OriginalChars, "to", built.PromptChars, "history_dropped", built.HistoryDropped)
	}

	call, err := x.ask(fullPrompt, fresh.Prompt)
	if err != nil {
		x.handleError(err)
		return
	}
	if call.promptUsed != fullPrompt {
		fullPrompt = call.promptUsed
		if fresh.Compacted {
			x.markCompacted()
		}
	}
	res, err := x.read(call, "")
	if err != nil {
		x.handleError(err)
		return
	}
	content, reasoning := sanitize(res.Content), sanitize(res.Reasoning)
	finish, modelErr := res.FinishReason, res.ModelError
	x.log.Info("upstream answer", "chars", prompt.Len(content), "reasoning_chars", prompt.Len(reasoning), "elapsed_ms", time.Since(x.started).Milliseconds())

	// Empty / context-overflow recovery: each retry gets a smaller prompt and
	// a fresh remote session; the attempt cap prevents retry storms.
	attempts := 0
	for strings.TrimSpace(content) == "" {
		if x.clientGone() {
			x.log.Info("client disconnected; abandoning empty-response retries")
			return
		}
		if x.deadlineHit() {
			x.log.Info("request deadline hit; stopping empty-response retries")
			break
		}
		tooLong := modelErr != nil && prompt.IsContextTooLong(modelErrorText(modelErr))
		if modelErr != nil && !tooLong {
			break
		}
		if attempts >= s.cfg.MaxEmptyRetries {
			break
		}
		attempts++
		ratio := max(0.5, 1-float64(attempts)*0.2)
		if tooLong {
			ratio = max(0.35, 0.8-float64(attempts)*0.2)
		}
		budget := max(config.MinPromptChars, int(float64(x.maxChars())*ratio))
		retry := prompt.BuildRetry(system, recoveryPrefix, conv, fullPrompt, budget)
		if retry.Compacted {
			x.markCompacted()
		}
		reason := "empty response"
		if tooLong {
			reason = "context-too-long response"
		}
		x.log.Info("retrying with a fresh session", "reason", reason, "attempt", attempts, "max", s.cfg.MaxEmptyRetries, "prompt_chars", retry.PromptChars)
		s.store.ResetRemote(x.sess)
		if !s.sleep(x.ctx, time.Duration(min(500*attempts, 1500))*time.Millisecond) {
			return
		}
		call, err := x.ask(retry.Prompt, retry.Prompt)
		if err != nil {
			x.handleError(err)
			return
		}
		r2, err := x.read(call, "")
		if err != nil {
			x.handleError(err)
			return
		}
		fullPrompt = retry.Prompt
		modelErr = r2.ModelError
		// Never leak a stale finish_reason=length into a successful retry.
		finish = r2.FinishReason
		if c := sanitize(r2.Content); strings.TrimSpace(c) != "" {
			x.log.Info("retry succeeded", "attempt", attempts)
			content, reasoning = c, sanitize(r2.Reasoning)
		}
	}

	if strings.TrimSpace(content) == "" {
		timedOut := x.deadlineHit()
		status, errType := classifyRecoveryFailure(modelErr, timedOut)
		failure := s.store.ResetRemote(x.sess)
		msg := ""
		if modelErr != nil {
			msg = modelErr.Content
		}
		if msg == "" {
			if timedOut {
				msg = "DeepSeek request deadline reached while recovering an empty response"
			} else {
				msg = fmt.Sprintf("DeepSeek returned empty content after %d %s", attempts, plural(attempts, "retry", "retries"))
			}
		}
		x.log.Warn("giving up", "type", errType, "retries", attempts)
		x.fail(status, errType, msg, map[string]any{
			"agent": x.agent, "failed_session_id": nullable(failure.FailedSessionID), "message_count": failure.FailedMessageCount,
			"history_length": s.store.HistoryLen(x.sess), "account": nullable(failure.AccountID), "retry_attempts": attempts,
			"upstream_prompt_chars": prompt.Len(fullPrompt), "prompt_compacted": x.compacted, "model": req.Model, "real_model": x.model.RealModel,
		})
		return
	}

	// Auto-continuation for truncated (or suspiciously long) answers.
	for rounds := 0; rounds < maxContinuations && (finish == "length" || (s.cfg.ContinueOverChars > 0 && prompt.Len(content) > s.cfg.ContinueOverChars)); rounds++ {
		if x.clientGone() || x.deadlineHit() {
			break
		}
		x.log.Info("auto-continuing", "chars", prompt.Len(content), "finish", finish, "round", rounds+1)
		if !s.sleep(x.ctx, 500*time.Millisecond) {
			return
		}
		before := s.store.Remote(x.sess).AccountID
		recovery := prompt.AppendInstruction(fresh.Prompt+"\n\n[Assistant response so far]\n"+content,
			"Continue the assistant response from exactly where it stopped. Do not restart or repeat completed sections.", x.maxChars())
		cc, err := x.ask("continue", recovery)
		if err != nil {
			x.handleError(err)
			return
		}
		// A cross-account continuation is only valid when the call noticed
		// the rotation and sent the full recovery prompt.
		if !continuationSafe(before, cc) {
			x.log.Warn("continuation rotated to a foreign account; skipping", "from", before, "to", cc.accountID)
			cc.body.Close()
			s.store.ResetRemote(x.sess)
			break
		}
		cr, err := x.read(cc, "\n")
		if err != nil {
			x.handleError(err)
			return
		}
		cont := sanitize(cr.Content)
		if strings.TrimSpace(cont) == "" || strings.Contains(cont, "I am an AI") {
			x.log.Info("continuation returned nothing useful; stopping")
			break
		}
		content += "\n" + cont
		if r := sanitize(cr.Reasoning); r != "" {
			if reasoning != "" {
				reasoning += "\n"
			}
			reasoning += r
		}
		finish = cr.FinishReason
	}

	var tc *toolcall.Call
	// Content already streamed live as prose cannot be re-read as a tool call;
	// the guard only commits once the start is clearly not markup.
	liveCommitted := x.live != nil && x.live.committed
	if len(allowed) > 0 && !liveCommitted {
		tc = toolcall.Parse(content)
		if tc != nil && !allowed[tc.Name] {
			x.log.Info("model requested an unknown tool; attempting format repair", "tool", tc.Name)
			tc = nil
		}
		// Retry once if tool markup was truncated or malformed; never pass raw
		// DSML through as a normal assistant turn.
		if tc == nil && toolcall.LooksLikeMarkup(content) && !x.clientGone() && !x.deadlineHit() {
			x.log.Info("tool-call markup invalid or truncated; retrying with a stricter prompt", "chars", prompt.Len(content))
			s.store.ResetRemote(x.sess)
			if !s.sleep(x.ctx, time.Second) {
				return
			}
			strict := prompt.AppendInstruction(fresh.Prompt,
				`[STRICT INSTRUCTION] Your previous response contained incomplete tool-call markup. Keep arguments short and output ONLY strict JSON: {"tool_call":{"name":"<function>","arguments":{...}}}. Inside JSON strings escape every backslash as \\ and every line break as \n.`, x.maxChars())
			call, err := x.ask(strict, strict)
			if err != nil {
				x.handleError(err)
				return
			}
			r3, err := x.read(call, "")
			if err != nil {
				x.handleError(err)
				return
			}
			if rc := sanitize(r3.Content); strings.TrimSpace(rc) != "" {
				content, reasoning = rc, sanitize(r3.Reasoning)
				// If the retry was streamed live as prose (guard committed) it is
				// the final answer, not a tool call, so leave tc nil.
				if rtc := toolcall.Parse(rc); rtc != nil && allowed[rtc.Name] && !(x.live != nil && x.live.committed) {
					x.log.Info("strict retry succeeded", "tool", rtc.Name)
					tc = rtc
				}
			}
		}
		if tc == nil && toolcall.LooksLikeMarkup(content) {
			x.log.Warn("rejected malformed tool-call markup", "chars", prompt.Len(content), "preview", prompt.TruncateMiddle(content, 600, 0.5))
			failure := s.store.ResetRemote(x.sess)
			x.fail(http.StatusBadGateway, "malformed_tool_call", "DeepSeek returned malformed tool-call markup after one repair attempt", map[string]any{
				"agent": x.agent, "failed_session_id": nullable(failure.FailedSessionID), "message_count": failure.FailedMessageCount,
				"history_length": s.store.HistoryLen(x.sess), "account": nullable(failure.AccountID), "prompt_compacted": x.compacted,
				"model": req.Model, "real_model": x.model.RealModel,
			})
			return
		}
	}

	// Gateways (e.g. Telegram bots) attach files referenced as MEDIA:<path>.
	if s.cfg.MediaInjection && tc == nil && !strings.Contains(content, "MEDIA:") {
		if paths := prompt.ExtractMediaPaths(req.Messages); len(paths) > 0 {
			suffix := "\n\n" + strings.Join(paths, "\n")
			content += suffix
			// When guarding, content may still be held back; the final flush
			// emits it whole, so appending to the buffer is enough here.
			if x.live != nil && (x.live.committed || !x.live.guard) {
				x.live.sw.content(suffix)
			}
			x.log.Info("injected MEDIA paths", "paths", paths)
		}
	}

	stored := content
	if tc != nil {
		stored = "TOOL_CALL: " + tc.Name + "\narguments: " + tc.Arguments
	}
	s.store.StoreHistory(x.sess, conv, stored)

	if x.live != nil {
		if tc != nil {
			// Held back by the guard, so no prose was streamed: emit the call.
			x.live.sw.toolCall(liveToolCall(tc))
			x.live.sw.finish("tool_calls", buildUsage(clientPrompt, "", reasoning))
		} else {
			// Held prose (e.g. a JSON answer that was not a tool call) has not
			// been streamed yet: emit it before finishing.
			if x.live.guard && !x.live.committed {
				x.live.flushHeld(content)
			}
			x.live.sw.finish(textFinishReason(finish), buildUsage(clientPrompt, content, reasoning))
		}
		x.log.Info("streamed live response", "mode", x.mode, "tool", tc != nil, "chars", prompt.Len(content), "elapsed_ms", time.Since(x.started).Milliseconds())
		return
	}
	respReasoning := reasoning
	if !s.cfg.ShowReasoning {
		respReasoning = ""
	}
	var resp *Completion
	if tc != nil {
		resp = buildToolCallResponse(tc, req.Model, clientPrompt, respReasoning)
	} else {
		resp = buildTextResponse(content, clientPrompt, req.Model, respReasoning, finish)
	}
	x.respond(resp)
	x.log.Info("response sent", "mode", x.mode, "stream", req.Stream, "tool", tc != nil, "chars", prompt.Len(content), "elapsed_ms", time.Since(x.started).Milliseconds())
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func modelErrorText(me *deepseek.ModelError) string {
	if me == nil {
		return ""
	}
	return me.Content + " " + me.FinishReason + " " + me.Type
}

// classifyRecoveryFailure maps an unrecoverable empty answer to a status.
func classifyRecoveryFailure(me *deepseek.ModelError, timedOut bool) (int, string) {
	if me != nil && prompt.IsContextTooLong(modelErrorText(me)) {
		return http.StatusBadRequest, "context_length_exceeded"
	}
	if timedOut {
		return http.StatusGatewayTimeout, "request_timeout"
	}
	if me != nil && me.Type != "" {
		return http.StatusBadGateway, me.Type
	}
	return http.StatusBadGateway, "empty_response"
}

// upstreamCall is one started DeepSeek completion.
type upstreamCall struct {
	body       io.ReadCloser
	accountID  string
	promptUsed string
	// freshReset is true when the call ran in a new remote chat and therefore
	// sent the full recovery prompt instead of the incremental one.
	freshReset bool
}

func continuationSafe(previousAccount string, c *upstreamCall) bool {
	return previousAccount == "" || c.accountID == "" || c.accountID == previousAccount || c.freshReset
}

func (x *exchange) noteFailure(accountID string, err error, label string) {
	var ue *deepseek.UpstreamError
	if errors.As(err, &ue) {
		x.s.store.MarkFailure(accountID, ue.Status, label, ue.RetryAfter)
	}
}

func (x *exchange) solve(acct state.AccountRef) (string, error) {
	hdr, err := x.s.ds.SolvePoW(x.ctx, acct.Creds)
	if err != nil {
		x.noteFailure(acct.ID, err, "pow challenge")
		if errors.Is(err, pow.ErrNoSolution) {
			return "", fmt.Errorf("could not solve DeepSeek proof-of-work: %w", err)
		}
	}
	return hdr, err
}

func (x *exchange) newRemote(acct state.AccountRef, label string) error {
	id, err := x.s.ds.CreateSession(x.ctx, acct.Creds)
	if err != nil {
		x.noteFailure(acct.ID, err, label)
		return err
	}
	x.s.store.AttachRemote(x.sess, id, acct.ID)
	x.log.Info("created remote session", "session", id, "account", acct.ID)
	return nil
}

// ask starts a completion in the agent's sticky remote chat. freshPrompt is
// used instead of p whenever the call has to start a new remote chat.
func (x *exchange) ask(p, freshPrompt string) (*upstreamCall, error) {
	st := x.s.store
	hadRemote := st.Remote(x.sess).ID != ""
	acct, err := st.SelectAccount(x.sess)
	if err != nil {
		return nil, err
	}
	// Rollover normally happens before the prompt is built; this guard covers
	// concurrent requests that advanced the same session meanwhile.
	rollover := st.PrepareForPrompt(x.sess)
	remote := st.Remote(x.sess)
	rotationReset := hadRemote && remote.ID == ""
	recovered := rotationReset || rollover != nil
	effective := p
	if recovered {
		effective = freshPrompt
	}
	if rotationReset {
		x.log.Info("account rotation reset the previous remote session; using recovery prompt")
	}
	if rollover != nil {
		x.log.Info("session reset before upstream call", "session", rollover.FailedSessionID, "reason", rollover.Reason)
	}

	powHeader, err := x.solve(acct)
	if err != nil {
		return nil, err
	}
	if remote.ID == "" {
		if err := x.newRemote(acct, "session create"); err != nil {
			return nil, err
		}
		remote = st.Remote(x.sess)
	} else {
		x.log.Debug("reusing remote session", "session", remote.ID, "msg", remote.MessageCount)
	}

	completion := func(chatID string, parent any, text string) (io.ReadCloser, error) {
		return x.s.ds.Completion(x.ctx, acct.Creds, powHeader, deepseek.CompletionRequest{
			ChatSessionID: chatID, ParentMessageID: parent, ModelType: x.model.ModelType, Prompt: text,
			ThinkingEnabled: x.model.ThinkingEnabled, SearchEnabled: x.model.SearchEnabled,
		})
	}
	body, err := completion(remote.ID, remote.ParentMessageID, effective)
	if err == nil {
		return &upstreamCall{body: body, accountID: acct.ID, promptUsed: effective, freshReset: recovered}, nil
	}
	x.noteFailure(acct.ID, err, "completion")
	var ue *deepseek.UpstreamError
	if !errors.As(err, &ue) || (ue.Status != 400 && ue.Status != 404 && ue.Status != 500) {
		return nil, err
	}
	// The remote chat expired or broke: recreate it once and resend the full
	// recovery prompt. PoW challenges are single-use, so solve a new one.
	x.log.Info("remote session rejected; recreating", "session", remote.ID, "status", ue.Status)
	st.ResetRemote(x.sess)
	if powHeader, err = x.solve(acct); err != nil {
		return nil, err
	}
	if err := x.newRemote(acct, "session recreate"); err != nil {
		return nil, err
	}
	remote = st.Remote(x.sess)
	body, err = completion(remote.ID, nil, freshPrompt)
	if err != nil {
		x.noteFailure(acct.ID, err, "completion after session recreate")
		return nil, err
	}
	return &upstreamCall{body: body, accountID: acct.ID, promptUsed: freshPrompt, freshReset: true}, nil
}

// read consumes a completion, streaming deltas live when enabled. prefix is
// written before the first live content delta (used by continuations).
func (x *exchange) read(c *upstreamCall, prefix string) (deepseek.StreamResult, error) {
	defer c.body.Close()
	var onUpdate func(string, string)
	if x.live != nil {
		x.live.reset(prefix)
		onUpdate = x.live.update
	}
	res, err := deepseek.ReadStream(c.body, onUpdate)
	if x.live != nil && err == nil {
		x.live.finalize(sanitize(res.Content))
	}
	if res.MessageID != nil {
		x.s.store.Advance(x.sess, res.MessageID)
	} else if err == nil {
		x.log.Warn("could not extract message_id from DeepSeek stream")
	}
	return res, err
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, deepseek.ErrIdleTimeout) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return prompt.LooksLikeTimeout(err.Error())
}

func (x *exchange) handleError(err error) {
	if x.clientGone() {
		x.log.Info("client disconnected", "err", err)
		return
	}
	x.log.Error("request failed", "err", err)
	status, errType := http.StatusInternalServerError, "server_error"
	var extra map[string]any
	var pe *state.PoolError
	var ue *deepseek.UpstreamError
	switch {
	case errors.As(err, &pe):
		status, errType = pe.Status, pe.Type
		if pe.Status == http.StatusTooManyRequests && pe.RetryAfter > 0 && (x.live == nil || !x.live.sw.started()) {
			x.w.Header().Set("Retry-After", strconv.Itoa(pe.RetryAfter))
		}
	case errors.As(err, &ue):
		status, errType = ue.Status, ue.Type()
		if ue.RetryAfter != "" && (x.live == nil || !x.live.sw.started()) {
			x.w.Header().Set("Retry-After", ue.RetryAfter)
		}
	case isTimeout(err):
		status, errType = http.StatusGatewayTimeout, "request_timeout"
		if x.sess != nil {
			f := x.s.store.ResetRemote(x.sess)
			extra = map[string]any{"agent": x.agent, "failed_session_id": nullable(f.FailedSessionID), "message_count": f.FailedMessageCount,
				"history_length": x.s.store.HistoryLen(x.sess), "account": nullable(f.AccountID)}
		}
	}
	x.fail(status, errType, err.Error(), extra)
}

// liveTracker turns accumulated upstream text into client deltas.
//
// When guard is set (tool requests are allowed) content cannot be streamed
// blindly: a tool call must be caught whole and never leaked as prose. Tool
// markup always begins the answer, so the tracker inspects the first visible
// characters of each read and only commits to live streaming once they are
// clearly not markup. A markup-looking start is held back, so the buffered
// path can parse it into a tool call. hideReasoning drops reasoning deltas.
type liveTracker struct {
	sw            streamWriter
	log           *slog.Logger
	hideReasoning bool
	guard         bool
	prefix        string
	content       string
	reasoning     string
	committed     bool // content is being streamed live this read
	blocked       bool // this read's content looked like tool markup; held back
}

func (t *liveTracker) reset(prefix string) {
	t.prefix, t.content, t.reasoning = prefix, "", ""
	t.committed, t.blocked = false, false
}

func (t *liveTracker) update(content, reasoning string) {
	content, reasoning = sanitize(content), sanitize(reasoning)
	if !t.hideReasoning && reasoning != t.reasoning {
		if strings.HasPrefix(reasoning, t.reasoning) {
			t.sw.reasoning(reasoning[len(t.reasoning):])
		} else {
			t.log.Warn("upstream rewrote streamed reasoning; delta skipped")
		}
	}
	t.reasoning = reasoning

	if content == t.content {
		return
	}
	if t.guard && !t.committed {
		if t.blocked {
			return
		}
		switch liveContentDecision(strings.TrimSpace(content)) {
		case liveHold:
			t.blocked = true
			return
		case liveWait:
			return
		}
		// liveCommit: flush everything buffered so far as the first delta.
		t.committed = true
		t.sw.content(t.prefix + content)
		t.content = content
		return
	}
	if strings.HasPrefix(content, t.content) {
		d := content[len(t.content):]
		if t.content == "" {
			d = t.prefix + d
		}
		t.sw.content(d)
	} else {
		t.log.Warn("upstream rewrote streamed content; delta skipped")
	}
	t.content = content
}

// finalize is called once a read has fully drained, so a short answer that
// never grew past the "wait" stage is still streamed. It returns whether the
// read's content was held back as suspected tool markup.
func (t *liveTracker) finalize(content string) bool {
	if !t.guard || t.committed {
		return false
	}
	if t.blocked || liveContentDecision(strings.TrimSpace(content)) == liveHold {
		t.blocked = true
		return true
	}
	if content != "" {
		t.committed = true
		t.sw.content(t.prefix + content)
		t.content = content
	}
	return false
}

// flushHeld streams content that the guard buffered but never committed.
func (t *liveTracker) flushHeld(content string) {
	if t.committed {
		return
	}
	t.committed = true
	t.content = sanitize(content)
	t.sw.content(t.prefix + t.content)
}

const (
	liveWait   = iota // too little text to decide; keep buffering
	liveHold          // looks like the start of tool markup; never stream live
	liveCommit        // clearly prose; safe to stream live
)

// liveContentDecision classifies the start of a guarded answer. DeepSeek emits
// tool markup (DSML tags, a JSON envelope, a fenced block or the legacy
// TOOL_CALL: prefix) at the very beginning, so only the leading bytes matter.
func liveContentDecision(trimmed string) int {
	if trimmed == "" {
		return liveWait
	}
	switch trimmed[0] {
	case '<', '{', '`', '|':
		return liveHold
	}
	if strings.HasPrefix(trimmed, "｜") {
		return liveHold
	}
	const legacy = "TOOL_CALL:"
	if len(trimmed) < len(legacy) {
		if strings.EqualFold(trimmed, legacy[:len(trimmed)]) {
			return liveWait
		}
	} else if strings.EqualFold(trimmed[:len(legacy)], legacy) {
		return liveHold
	}
	return liveCommit
}
