package state

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ertiti11/freedeep/internal/deepseek"
	"github.com/ertiti11/freedeep/internal/prompt"
)

var creds = deepseek.Credentials{Token: "t", Cookie: "c"}

func TestSweepEvictsOnlyIdle(t *testing.T) {
	s := NewStore(time.Minute)
	s.InjectSession("stale", &Session{LastActivityAt: time.Unix(1, 0)})
	s.Session("fresh")
	s.SweepIdle(time.Minute)
	if s.HasSession("stale") || !s.HasSession("fresh") {
		t.Error("sweep")
	}
}

func TestResetRemoteKeepsHistoryAndStickyAccount(t *testing.T) {
	s := NewStore(time.Minute)
	sess := &Session{ID: "failed", ParentMessageID: "p", CreatedAt: time.Unix(123, 0), MessageCount: 17, AccountID: "account_2",
		History: []prompt.Exchange{{User: "old", Assistant: "answer"}}}
	f := s.ResetRemote(sess)
	if f != (Failure{FailedSessionID: "failed", FailedMessageCount: 17, AccountID: "account_2"}) {
		t.Errorf("failure=%+v", f)
	}
	if sess.ID != "" || sess.ParentMessageID != nil || !sess.CreatedAt.IsZero() || sess.MessageCount != 0 || sess.AccountID != "account_2" || len(sess.History) != 1 {
		t.Errorf("session=%+v", sess)
	}
}

func TestRotationClearsForeignRemoteSession(t *testing.T) {
	s := NewStore(time.Minute)
	s.SetAccounts([]*Account{
		{ID: "cooling", Creds: creds, CooldownUntil: time.Now().Add(time.Minute)},
		{ID: "ready", Creds: creds},
	})
	sess := &Session{ID: "foreign", ParentMessageID: "fp", AccountID: "cooling", MessageCount: 7, History: []prompt.Exchange{{User: "u", Assistant: "a"}}}
	acct, err := s.SelectAccount(sess)
	if err != nil || acct.ID != "ready" || sess.AccountID != "ready" {
		t.Fatalf("acct=%v err=%v", acct, err)
	}
	if sess.ID != "" || sess.ParentMessageID != nil || sess.MessageCount != 0 || len(sess.History) != 1 {
		t.Errorf("session=%+v", sess)
	}
}

func TestPoolExhaustionErrors(t *testing.T) {
	s := NewStore(time.Minute)
	_, err := s.SelectAccount(&Session{})
	var pe *PoolError
	if !errors.As(err, &pe) || pe.Status != 503 || pe.Type != "no_auth" {
		t.Errorf("no accounts: %v", err)
	}
	s.SetAccounts([]*Account{{ID: "a", Creds: creds, CooldownUntil: time.Now().Add(30 * time.Second)}})
	_, err = s.SelectAccount(&Session{})
	if !errors.As(err, &pe) || pe.Status != 429 || pe.RetryAfter < 29 || pe.RetryAfter > 30 {
		t.Errorf("cooling: %+v", pe)
	}
}

func TestRoundRobinAndCooldowns(t *testing.T) {
	s := NewStore(10 * time.Minute)
	s.SetAccounts([]*Account{{ID: "a", Creds: creds}, {ID: "b", Creds: creds}})
	a1, _ := s.SelectAccount(&Session{})
	a2, _ := s.SelectAccount(&Session{})
	if a1.ID == a2.ID {
		t.Error("round robin")
	}
	s.MarkFailure("a", 500, "x", "")
	if ready, _ := s.ReadyCount(); ready != 2 {
		t.Error("500 must not cool down")
	}
	s.MarkFailure("a", 429, "x", "5")
	st := s.Accounts()[0]
	if !st.Cooldown || st.CooldownRemainingSec > 5 || st.Failures != 2 {
		t.Errorf("Retry-After not honored: %+v", st)
	}
	s.MarkFailure("b", 401, "x", "")
	if st := s.Accounts()[1]; st.CooldownRemainingSec < 590 {
		t.Errorf("default cooldown: %+v", st)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2025, 10, 21, 7, 27, 0, 0, time.UTC)
	if d, ok := ParseRetryAfter("120", now); !ok || d != 120*time.Second {
		t.Error("seconds")
	}
	if d, ok := ParseRetryAfter("Tue, 21 Oct 2025 07:28:00 GMT", now); !ok || d != time.Minute {
		t.Errorf("http-date: %v", d)
	}
	if d, _ := ParseRetryAfter("0", now); d != time.Second {
		t.Error("clamp")
	}
	if _, ok := ParseRetryAfter("soon", now); ok {
		t.Error("garbage")
	}
}

func TestRolloverBeforePrompt(t *testing.T) {
	s := NewStore(time.Minute)
	deep := &Session{ID: "deep", MessageCount: 100, AccountID: "account_1", History: []prompt.Exchange{{User: "u", Assistant: "a"}}}
	r := s.PrepareForPrompt(deep)
	if r == nil || r.Reason != "max_message_depth" || deep.ID != "" || len(deep.History) != 1 || deep.AccountID != "account_1" {
		t.Errorf("depth: %+v %+v", r, deep)
	}
	old := &Session{ID: "old", CreatedAt: time.Now().Add(-SessionTTL - time.Millisecond)}
	if r := s.PrepareForPrompt(old); r == nil || r.Reason != "session_ttl" || r.FailedSessionID != "old" {
		t.Errorf("ttl: %+v", r)
	}
	if s.PrepareForPrompt(&Session{ID: "ok", CreatedAt: time.Now()}) != nil {
		t.Error("healthy session rolled over")
	}
}

func TestStoreHistoryBounds(t *testing.T) {
	s := NewStore(time.Minute)
	sess := s.Session("a")
	for i := 0; i < 20; i++ {
		s.StoreHistory(sess, "q", "a")
	}
	if len(sess.History) != MaxHistoryLength {
		t.Errorf("len=%d", len(sess.History))
	}
	long := strings.Repeat("x", 3000)
	for i := 0; i < 5; i++ {
		s.StoreHistory(sess, "q", long)
	}
	total := 0
	for _, e := range sess.History {
		total += prompt.Len(e.User) + prompt.Len(e.Assistant)
	}
	if total > MaxHistoryChars {
		t.Errorf("total=%d", total)
	}
	s.StoreHistory(sess, long, "a")
	if u := sess.History[len(sess.History)-1].User; prompt.Len(u) != 503 {
		t.Errorf("prompt not shortened: %d", prompt.Len(u))
	}
}
