// Package state holds the account pool and the per-agent session store.
//
// Unlike the single-threaded Node original, handlers run concurrently, so all
// mutable fields are guarded by one mutex. Critical sections are short and
// never span network I/O.
package state

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ertiti11/freedeep/internal/deepseek"
	"github.com/ertiti11/freedeep/internal/prompt"
)

// Limits for session reuse and local recovery history.
const (
	MaxHistoryLength = 15
	MaxHistoryChars  = 10000
	MaxMessageDepth  = 100
	SessionTTL       = 2 * time.Hour
)

// Account is one DeepSeek login in the pool.
type Account struct {
	ID            string
	File          string
	Creds         deepseek.Credentials
	CooldownUntil time.Time
	Failures      int
	LastUsedAt    time.Time
}

func (a *Account) ready(now time.Time) bool { return a.Creds.Ready() && !a.CooldownUntil.After(now) }

// Session is one agent's sticky DeepSeek chat plus local recovery history.
type Session struct {
	ID              string
	ParentMessageID any
	CreatedAt       time.Time
	MessageCount    int
	AccountID       string
	History         []prompt.Exchange
	LastActivityAt  time.Time
}

// Failure describes a remote session that was discarded.
type Failure struct {
	FailedSessionID    string `json:"failed_session_id"`
	FailedMessageCount int    `json:"message_count"`
	AccountID          string `json:"account"`
}

// Rollover is a Failure caused by TTL or depth limits.
type Rollover struct {
	Reason string
	Failure
}

// Store is the shared mutable state.
type Store struct {
	mu       sync.Mutex
	accounts []*Account
	sessions map[string]*Session
	rr       int
	Cooldown time.Duration
	now      func() time.Time
}

// NewStore returns an empty store.
func NewStore(cooldown time.Duration) *Store {
	return &Store{sessions: map[string]*Session{}, Cooldown: cooldown, now: time.Now}
}

// ---------------------------------------------------------------------------
// Accounts

// LoadAccounts replaces the pool with credentials read from files.
func (s *Store) LoadAccounts(paths []string) (loaded int, errs []error) {
	var accts []*Account
	for _, file := range paths {
		raw, err := os.ReadFile(file)
		if err != nil {
			errs = append(errs, fmt.Errorf("could not load auth config %s: %w", file, err))
			continue
		}
		var c deepseek.Credentials
		if err := json.Unmarshal(raw, &c); err != nil {
			errs = append(errs, fmt.Errorf("could not parse auth config %s: %w", file, err))
			continue
		}
		if c.WasmURL == "" {
			c.WasmURL = deepseek.DefaultWasmURL
		}
		accts = append(accts, &Account{ID: fmt.Sprintf("account_%d", len(accts)+1), File: file, Creds: c})
	}
	s.mu.Lock()
	s.accounts = accts
	s.mu.Unlock()
	return len(accts), errs
}

// SetAccounts replaces the pool (tests).
func (s *Store) SetAccounts(accts []*Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = accts
}

// HasReadyCredentials reports whether any account has a token and cookie.
func (s *Store) HasReadyCredentials() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.Creds.Ready() {
			return true
		}
	}
	return false
}

// AccountStatus is the public view of an account.
type AccountStatus struct {
	ID                   string `json:"id"`
	Ready                bool   `json:"ready"`
	Cooldown             bool   `json:"cooldown"`
	CooldownRemainingSec int    `json:"cooldown_remaining_sec"`
	Failures             int    `json:"failures"`
	LastUsedAt           *int64 `json:"last_used_at"`
}

// Accounts returns a snapshot of the pool.
func (s *Store) Accounts() []AccountStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]AccountStatus, 0, len(s.accounts))
	for _, a := range s.accounts {
		st := AccountStatus{
			ID:                   a.ID,
			Ready:                a.Creds.Ready(),
			Cooldown:             a.CooldownUntil.After(now),
			CooldownRemainingSec: max(0, int(math.Ceil(a.CooldownUntil.Sub(now).Seconds()))),
			Failures:             a.Failures,
		}
		if !a.LastUsedAt.IsZero() {
			ms := a.LastUsedAt.UnixMilli()
			st.LastUsedAt = &ms
		}
		out = append(out, st)
	}
	return out
}

// ReadyCount returns (ready accounts, total accounts).
func (s *Store) ReadyCount() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for _, a := range s.accounts {
		if a.ready(now) {
			n++
		}
	}
	return n, len(s.accounts)
}

// PoolError is returned when no account can serve a request.
type PoolError struct {
	Status     int
	Type       string
	Message    string
	RetryAfter int
}

func (e *PoolError) Error() string { return e.Message }

// SelectAccount picks the account for a session. A remote DeepSeek chat
// belongs to the account that created it, so when the sticky account is
// gone, unauthenticated or cooling down, the remote session is discarded
// (local history and recovery are preserved) and another account is chosen.
func (s *Store) SelectAccount(sess *Session) (AccountRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if sess.AccountID != "" {
		for _, a := range s.accounts {
			if a.ID == sess.AccountID && a.ready(now) {
				a.LastUsedAt = now
				return AccountRef{ID: a.ID, Creds: a.Creds}, nil
			}
		}
		s.resetRemoteLocked(sess)
		sess.AccountID = ""
	}
	var ready, withCreds []*Account
	for _, a := range s.accounts {
		if a.Creds.Ready() {
			withCreds = append(withCreds, a)
			if a.ready(now) {
				ready = append(ready, a)
			}
		}
	}
	if len(ready) == 0 {
		if len(withCreds) > 0 {
			sort.Slice(withCreds, func(i, j int) bool { return withCreds[i].CooldownUntil.Before(withCreds[j].CooldownUntil) })
			wait := max(1, int(math.Ceil(withCreds[0].CooldownUntil.Sub(now).Seconds())))
			return AccountRef{}, &PoolError{Status: http.StatusTooManyRequests, Type: "rate_limit", RetryAfter: wait,
				Message: fmt.Sprintf("All DeepSeek auth accounts are cooling down. Retry in ~%ds or import a fresh account with `freedeep import`.", wait)}
		}
		return AccountRef{}, &PoolError{Status: http.StatusServiceUnavailable, Type: "no_auth",
			Message: "No valid DeepSeek auth accounts. Run `freedeep login` or `freedeep import`."}
	}
	a := ready[s.rr%len(ready)]
	s.rr++
	a.LastUsedAt = now
	sess.AccountID = a.ID
	return AccountRef{ID: a.ID, Creds: a.Creds}, nil
}

// AccountRef is an immutable view of an account handed to request code.
type AccountRef struct {
	ID    string
	Creds deepseek.Credentials
}

// ParseRetryAfter parses delta-seconds or an HTTP-date (clamped to >= 1s).
func ParseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
		return max(time.Second, time.Duration(n)*time.Second), true
	}
	if t, err := http.ParseTime(raw); err == nil {
		return max(time.Second, t.Sub(now)), true
	}
	return 0, false
}

// MarkFailure records an upstream failure; 401/403/429 start a cooldown.
// A 429 honors Retry-After when present.
func (s *Store) MarkFailure(accountID string, status int, reason, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != accountID {
			continue
		}
		a.Failures++
		if status == 401 || status == 403 || status == 429 {
			cooldown := s.Cooldown
			fromHeader := false
			if status == 429 {
				if d, ok := ParseRetryAfter(retryAfter, s.now()); ok {
					cooldown, fromHeader = d, true
				}
			}
			a.CooldownUntil = s.now().Add(cooldown)
			slog.Warn("account cooldown", "account", a.ID, "seconds", int(cooldown.Seconds()), "status", status, "reason", reason, "retry_after", fromHeader)
		}
		return
	}
}

// ---------------------------------------------------------------------------
// Sessions

// Session returns the agent's session, creating it if needed.
func (s *Store) Session(agentID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[agentID]
	if !ok {
		sess = &Session{}
		s.sessions[agentID] = sess
	}
	sess.LastActivityAt = s.now()
	return sess
}

// ReplaceSession installs a brand-new empty session (the /new command) and
// returns how many history entries were discarded.
func (s *Store) ReplaceSession(agentID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	if old, ok := s.sessions[agentID]; ok {
		n = len(old.History)
	}
	s.sessions[agentID] = &Session{LastActivityAt: s.now()}
	return n
}

func (s *Store) resetRemoteLocked(sess *Session) Failure {
	f := Failure{FailedSessionID: sess.ID, FailedMessageCount: sess.MessageCount, AccountID: sess.AccountID}
	sess.ID = ""
	sess.ParentMessageID = nil
	sess.CreatedAt = time.Time{}
	sess.MessageCount = 0
	// Local recovery history and the sticky account are kept: a remote chat
	// can be unhealthy without invalidating either.
	return f
}

// ResetRemote discards the remote chat but keeps local history.
func (s *Store) ResetRemote(sess *Session) Failure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetRemoteLocked(sess)
}

// PrepareForPrompt rolls over a remote chat that hit the depth or TTL limit.
func (s *Store) PrepareForPrompt(sess *Session) *Rollover {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ID == "" {
		return nil
	}
	now := s.now()
	reason := ""
	switch {
	case sess.MessageCount >= MaxMessageDepth:
		reason = "max_message_depth"
	case !sess.CreatedAt.IsZero() && now.Sub(sess.CreatedAt) > SessionTTL:
		reason = "session_ttl"
	default:
		return nil
	}
	return &Rollover{Reason: reason, Failure: s.resetRemoteLocked(sess)}
}

// RemoteState is a consistent snapshot of a session's remote chat.
type RemoteState struct {
	ID              string
	ParentMessageID any
	MessageCount    int
	AccountID       string
	History         []prompt.Exchange
}

// Remote returns a snapshot of the session.
func (s *Store) Remote(sess *Session) RemoteState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RemoteState{ID: sess.ID, ParentMessageID: sess.ParentMessageID, MessageCount: sess.MessageCount,
		AccountID: sess.AccountID, History: append([]prompt.Exchange(nil), sess.History...)}
}

// AttachRemote records a newly created remote chat.
func (s *Store) AttachRemote(sess *Session, chatID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.ID = chatID
	sess.AccountID = accountID
	sess.ParentMessageID = nil
	sess.CreatedAt = s.now()
	sess.MessageCount = 0
}

// Advance records the id of the last assistant message in the remote chat.
func (s *Store) Advance(sess *Session, messageID any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.ParentMessageID = messageID
	sess.MessageCount++
}

// StoreHistory remembers an exchange for recovery after a remote reset.
func (s *Store) StoreHistory(sess *Session, userPrompt, assistant string) {
	short := userPrompt
	if r := []rune(userPrompt); len(r) > 500 {
		short = "..." + string(r[len(r)-500:])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.History = append(sess.History, prompt.Exchange{User: short, Assistant: assistant})
	if len(sess.History) > MaxHistoryLength {
		sess.History = sess.History[len(sess.History)-MaxHistoryLength:]
	}
	total := 0
	for _, e := range sess.History {
		total += prompt.Len(e.User) + prompt.Len(e.Assistant)
	}
	for total > MaxHistoryChars && len(sess.History) > 1 {
		total -= prompt.Len(sess.History[0].User) + prompt.Len(sess.History[0].Assistant)
		sess.History = sess.History[1:]
	}
}

// HistoryLen returns the number of remembered exchanges.
func (s *Store) HistoryLen(sess *Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(sess.History)
}

// SweepIdle evicts sessions untouched for longer than maxIdle.
func (s *Store) SweepIdle(maxIdle time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	removed := 0
	for id, sess := range s.sessions {
		if now.Sub(sess.LastActivityAt) > maxIdle {
			delete(s.sessions, id)
			removed++
		}
	}
	if removed > 0 {
		slog.Info("swept idle sessions", "removed", removed, "remaining", len(s.sessions))
	}
	return removed
}

// SessionCount returns the number of tracked agents.
func (s *Store) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// SessionInfo is the public view of a session.
type SessionInfo struct {
	Agent        string `json:"agent"`
	SessionID    any    `json:"session_id"`
	MessageCount int    `json:"message_count"`
	Account      any    `json:"account"`
	HistorySize  int    `json:"history_size"`
	AgeMin       int    `json:"age_min"`
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Sessions lists all sessions sorted by agent id.
func (s *Store) Sessions() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]SessionInfo, 0, len(s.sessions))
	for id, sess := range s.sessions {
		age := 0
		if !sess.CreatedAt.IsZero() {
			age = int(math.Round(now.Sub(sess.CreatedAt).Minutes()))
		}
		out = append(out, SessionInfo{Agent: id, SessionID: nullable(sess.ID), MessageCount: sess.MessageCount,
			Account: nullable(sess.AccountID), HistorySize: len(sess.History), AgeMin: age})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// ClearSessions drops every session and returns how many there were.
func (s *Store) ClearSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.sessions)
	s.sessions = map[string]*Session{}
	return n
}

// ResetAgent resets one agent's remote chat, keeping its history. It returns
// the number of history entries and a short preview, or ok=false if unknown.
func (s *Store) ResetAgent(agentID string) (historyCount int, preview string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[agentID]
	if !ok {
		return 0, "", false
	}
	parts := make([]string, 0, len(sess.History))
	for _, e := range sess.History {
		r := []rune(e.User)
		parts = append(parts, string(r[:min(40, len(r))]))
	}
	s.resetRemoteLocked(sess)
	return len(sess.History), strings.Join(parts, " | "), true
}

// SetClock overrides the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// InjectSession installs a session directly (tests).
func (s *Store) InjectSession(agentID string, sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[agentID] = sess
}

// HasSession reports whether an agent has a session (tests).
func (s *Store) HasSession(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[agentID]
	return ok
}
