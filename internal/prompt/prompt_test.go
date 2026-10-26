package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ertiti11/freedeep/internal/chat"
	"github.com/ertiti11/freedeep/internal/toolcall"
)

func contains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q", sub)
		}
	}
}

func TestBuildBoundedPreservesEdgesAndDropsHistory(t *testing.T) {
	system := "SYSTEM_START\n" + strings.Repeat("s", 50000) + "\nTOOL_ADAPTER_END"
	history := "[Previous conversation]\n" + strings.Repeat("h", 10000) + "\n"
	conv := "TASK_START\n" + strings.Repeat("c", 70000) + "\nLATEST_TOOL_RESULT"
	b := BuildBounded(system, history, conv, 20000)
	if !b.Compacted || !b.HistoryDropped || Len(b.Prompt) > 20000 {
		t.Fatalf("compacted=%v historyDropped=%v len=%d", b.Compacted, b.HistoryDropped, Len(b.Prompt))
	}
	contains(t, b.Prompt, "SYSTEM_START", "TOOL_ADAPTER_END", "TASK_START", "LATEST_TOOL_RESULT")
	if strings.Contains(b.Prompt, "Previous conversation") {
		t.Error("recovery history should be dropped first")
	}
}

func TestBudgetCountsCharactersNotBytes(t *testing.T) {
	// 10k Cyrillic characters are 20k bytes but fit a 12k-character budget.
	conv := strings.Repeat("ж", 10000)
	if b := BuildBounded("", "", conv, 12000); b.Compacted {
		t.Error("multi-byte text was compacted by byte length")
	}
	b := BuildBounded("", "", strings.Repeat("ж", 30000), 12000)
	if !b.Compacted || Len(b.Prompt) > 12000 {
		t.Errorf("len=%d", Len(b.Prompt))
	}
}

func TestExplicitHistoryDetection(t *testing.T) {
	if HasExplicitHistory([]chat.Message{{Role: "system", Content: "rules"}, {Role: "user", Content: "hello"}}) {
		t.Error("single user turn is not history")
	}
	if !HasExplicitHistory([]chat.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}, {Role: "tool", Content: "r"}}) {
		t.Error("multi-turn history not detected")
	}
}

func TestContextTooLongDetector(t *testing.T) {
	for _, s := range []string{"Содержание слишком длинное. Сократите его и попробуйте снова.", "Maximum context length exceeded", "内容过长"} {
		if !IsContextTooLong(s) {
			t.Errorf("not detected: %s", s)
		}
	}
	if IsContextTooLong("Temporary backend overload") {
		t.Error("false positive")
	}
}

func TestRetryKeepsHistoryUnlessSmallerCapRequiresCompaction(t *testing.T) {
	history := "[Previous conversation]\nUser: old\nAssistant: answer\n\n[Continue from here]\n\n"
	initial := BuildBounded("SYSTEM", history, "User: follow up", 5000)
	retry := BuildRetry("SYSTEM", history, "User: follow up", initial.Prompt, 4000)
	if retry.Compacted {
		t.Error("small prompt should not be compacted")
	}
	contains(t, retry.Prompt, "Previous conversation", "Assistant: answer")

	large := "TASK_START\n" + strings.Repeat("x", 9000) + "\nLATEST_RESULT"
	largeInitial := BuildBounded("SYSTEM", history, large, 8000)
	smaller := BuildRetry("SYSTEM", history, large, largeInitial.Prompt, 4000)
	if !smaller.Compacted || Len(smaller.Prompt) > 4000 {
		t.Errorf("compacted=%v len=%d", smaller.Compacted, Len(smaller.Prompt))
	}
	contains(t, smaller.Prompt, "TASK_START", "LATEST_RESULT")
}

func TestFreshRetryRestoresOmittedHistory(t *testing.T) {
	history := RecoveryHistoryPrefix([]Exchange{{User: "original task", Assistant: "original answer"}})
	established := BuildBounded("SYSTEM", "", "User: follow up", 5000).Prompt
	fresh := BuildRetry("SYSTEM", history, "User: follow up", established, 5000)
	if Len(fresh.Prompt) <= Len(established) {
		t.Error("fresh prompt should be longer")
	}
	contains(t, fresh.Prompt, "Previous conversation", "original task", "original answer", "follow up")
}

func TestToolResultsUseGlobalCap(t *testing.T) {
	result := "RESULT_START\n" + strings.Repeat("z", 12000) + "\nRESULT_END"
	conv, system := Format([]chat.Message{{Role: "user", Content: "inspect this"}, {Role: "tool", Content: result}}, nil, toolcall.FormatOptions{})
	contains(t, conv, "RESULT_START", "RESULT_END")
	b := BuildBounded(system, "", conv, 5000)
	if !b.Compacted || Len(b.Prompt) > 5000 {
		t.Fatal("expected compaction")
	}
	contains(t, b.Prompt, "RESULT_END")
}

func TestFormatTranscript(t *testing.T) {
	conv, system := Format([]chat.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: []any{map[string]any{"type": "text", "text": "hi"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "u"}}}},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{Name: "ls", Arguments: `{"p":"."}`}}},
		{Role: "tool", Content: "a.txt"},
	}, nil, toolcall.FormatOptions{})
	if system != "be brief" {
		t.Errorf("system = %q", system)
	}
	want := "User: hi\n[Image: u]\n\nAssistant: TOOL_CALL: ls\narguments: {\"p\":\".\"}\n\n[Tool Result]\na.txt"
	if conv != want {
		t.Errorf("conv = %q", conv)
	}
}

func TestTruncateAndAppend(t *testing.T) {
	if got := TruncateMiddle("abcdef", 3, 0.5); got != "def" {
		t.Errorf("tiny budget keeps the tail, got %q", got)
	}
	out := AppendInstruction(strings.Repeat("p", 1000), "DO IT", 200)
	if Len(out) > 200 || !strings.HasSuffix(out, "\n\nDO IT") {
		t.Errorf("len=%d", Len(out))
	}
}

func TestExtractMediaPathsOnlyExistingFiles(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(img, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolJSON := `{"screenshot_path": "` + filepath.ToSlash(img) + `"}`
	got := ExtractMediaPaths([]chat.Message{
		{Role: "tool", Content: toolJSON},
		{Role: "tool", Content: "MEDIA:" + img + " MEDIA:/definitely/missing.png"},
	})
	if len(got) == 0 || len(got) > 2 {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if strings.Contains(g, "missing") {
			t.Errorf("hallucinated path included: %s", g)
		}
	}
}
