// Package prompt flattens chat messages into the single text prompt that
// DeepSeek Web accepts and keeps it within a character budget.
//
// All lengths here are in runes (characters), not bytes, so Cyrillic or CJK
// conversations are not compacted twice as aggressively as English ones.
package prompt

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ertiti11/freedeep/internal/chat"
	"github.com/ertiti11/freedeep/internal/toolcall"
)

// CompactionMarker is inserted where the middle of a prompt was removed.
const CompactionMarker = "\n\n[Earlier context compacted by freedeep]\n\n"

// Len returns the length of s in characters.
func Len(s string) int { return utf8.RuneCountInString(s) }

// TruncateMiddle keeps the head and tail of text so it fits maxChars,
// joining them with CompactionMarker.
func TruncateMiddle(text string, maxChars int, headRatio float64) string {
	n := Len(text)
	if n <= maxChars {
		return text
	}
	if maxChars <= 0 {
		return ""
	}
	r := []rune(text)
	markerLen := Len(CompactionMarker)
	if maxChars <= markerLen {
		return string(r[n-maxChars:])
	}
	payload := maxChars - markerLen
	head := int(float64(payload) * headRatio)
	head = max(0, min(payload, head))
	tail := payload - head
	return string(r[:head]) + CompactionMarker + string(r[n-tail:])
}

// HasExplicitHistory reports whether the client sent its own multi-turn
// history (in which case server-side recovery history is redundant).
func HasExplicitHistory(messages []chat.Message) bool {
	turns := 0
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		turns++
		if m.Role == "assistant" || m.Role == "tool" {
			return true
		}
	}
	return turns > 1
}

// Exchange is one remembered user/assistant pair.
type Exchange struct {
	User      string
	Assistant string
}

// RecoveryHistoryPrefix renders local history for a fresh remote session.
func RecoveryHistoryPrefix(history []Exchange) string {
	if len(history) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Previous conversation]\n")
	for _, e := range history {
		b.WriteString("User: " + e.User + "\nAssistant: " + e.Assistant + "\n\n")
	}
	b.WriteString("[Continue from here]\n\n")
	return b.String()
}

// Bounded is the result of BuildBounded.
type Bounded struct {
	Prompt              string
	Compacted           bool
	HistoryDropped      bool
	OriginalChars       int
	PromptChars         int
	PreviousPromptChars int
}

// BuildBounded joins system prompt, recovery history and conversation, and
// compacts the result to maxChars when necessary. Recovery history is dropped
// first; then the system prompt and conversation are each middle-truncated,
// preserving the task start, the tool adapter (end of the system prompt) and
// the most recent turns.
func BuildBounded(systemPrompt, historyPrefix, conversation string, maxChars int) Bounded {
	system := strings.TrimSpace(systemPrompt)
	conv := strings.TrimSpace(conversation)
	original := historyPrefix + conv
	if system != "" {
		original = system + "\n\n" + historyPrefix + conv
	}
	safeMax := max(1, maxChars)
	origLen := Len(original)
	if origLen <= safeMax {
		return Bounded{Prompt: original, OriginalChars: origLen, PromptChars: origLen}
	}

	sep := 0
	if system != "" && conv != "" {
		sep = 2
	}
	systemBudget := 0
	if system != "" {
		systemBudget = (safeMax - sep) / 2
	}
	convBudget := max(0, safeMax-sep-systemBudget)
	// Give unused capacity from a short side to the other side.
	sysLen, convLen := Len(system), Len(conv)
	if sysLen < systemBudget {
		systemBudget = sysLen
		convBudget = max(0, safeMax-sep-systemBudget)
	} else if convLen < convBudget {
		convBudget = convLen
		systemBudget = max(0, safeMax-sep-convBudget)
	}

	bs := TruncateMiddle(system, systemBudget, 0.35)
	bc := TruncateMiddle(conv, convBudget, 0.25)
	bounded := bs + bc
	if bs != "" && bc != "" {
		bounded = bs + "\n\n" + bc
	}
	if Len(bounded) > safeMax {
		bounded = string([]rune(bounded)[:safeMax])
	}
	return Bounded{
		Prompt:         bounded,
		Compacted:      true,
		HistoryDropped: historyPrefix != "",
		OriginalChars:  origLen,
		PromptChars:    Len(bounded),
	}
}

// BuildRetry rebuilds a prompt for a fresh-session retry with a smaller cap.
func BuildRetry(systemPrompt, historyPrefix, conversation, current string, maxChars int) Bounded {
	b := BuildBounded(systemPrompt, historyPrefix, conversation, maxChars)
	cur := Len(current)
	b.Compacted = b.Compacted || Len(b.Prompt) < cur
	b.PromptChars = Len(b.Prompt)
	b.PreviousPromptChars = cur
	return b
}

// AppendInstruction appends an instruction, truncating the prompt to fit.
func AppendInstruction(text, instruction string, maxChars int) string {
	suffix := "\n\n" + strings.TrimSpace(instruction)
	budget := max(0, maxChars-Len(suffix))
	return TruncateMiddle(text, budget, 0.35) + suffix
}

// Format splits messages into the system prompt (system messages plus the
// injected tool adapter) and a flattened conversation transcript.
func Format(messages []chat.Message, tools []chat.Tool, opts toolcall.FormatOptions) (conversation, systemPrompt string) {
	var sys strings.Builder
	for _, m := range messages {
		if m.Role == "system" && chat.Truthy(m.Content) {
			sys.WriteString(chat.NormalizeContent(m.Content) + "\n")
		}
	}
	sys.WriteString(toolcall.FormatDefinitions(tools, opts))

	var conv strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "system":
		case "user":
			if chat.Truthy(m.Content) {
				conv.WriteString("User: " + chat.NormalizeContent(m.Content) + "\n\n")
			}
		case "assistant":
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					conv.WriteString("Assistant: TOOL_CALL: " + tc.Name + "\narguments: " + tc.Arguments + "\n\n")
				}
			} else if chat.Truthy(m.Content) {
				conv.WriteString("Assistant: " + chat.NormalizeContent(m.Content) + "\n\n")
			}
		case "tool":
			// No per-result cap: one large tool result may be the essential
			// input. BuildBounded applies the single global request cap while
			// preserving the latest conversation tail.
			if chat.Truthy(m.Content) {
				conv.WriteString("[Tool Result]\n" + chat.NormalizeContent(m.Content) + "\n\n")
			}
		}
	}
	return strings.TrimSpace(conv.String()), strings.TrimSpace(sys.String())
}

var (
	reContextTooLong = regexp.MustCompile(`(?i)(?:content|prompt|context).{0,40}(?:too\s+long|too\s+large|length|limit|maximum)|maximum.{0,30}(?:context|token)|too\s+many\s+tokens|содержани[ея]\s+слишком\s+длин|контекст.{0,30}(?:длин|лимит)|内容.{0,12}(?:过长|太长)|上下文.{0,12}(?:过长|超出)`)
	reTimeout        = regexp.MustCompile(`(?i)(?:timed?\s*out|timeout)`)
)

// IsContextTooLong recognizes (localized) "prompt too long" upstream errors.
func IsContextTooLong(message string) bool { return reContextTooLong.MatchString(message) }

// LooksLikeTimeout matches timeout wording in an error message.
func LooksLikeTimeout(message string) bool { return reTimeout.MatchString(message) }

var (
	reScreenshotField = regexp.MustCompile(`(?i)["'](screenshot_path|path)["']\s*:\s*["']([^"']+\.(?:png|jpg|jpeg|webp|gif))["']`)
	reMediaTag        = regexp.MustCompile(`MEDIA:(\S+)`)
	reImagePath       = regexp.MustCompile(`(?i)(/[^\s<>"']+\.(?:png|jpg|jpeg|webp|gif))`)
)

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ExtractMediaPaths finds image paths referenced in the conversation that
// exist on this machine and returns them as MEDIA:<path> tags. Gateways such
// as Telegram bots use these tags to attach screenshots to replies.
func ExtractMediaPaths(messages []chat.Message) []string {
	var paths []string
	seen := map[string]bool{}
	add := func(tag string) {
		if !seen[tag] {
			seen[tag] = true
			paths = append(paths, tag)
		}
	}
	for _, m := range messages {
		content := chat.NormalizeContent(m.Content)
		if content == "" {
			continue
		}
		switch m.Role {
		case "tool":
			if sm := reScreenshotField.FindStringSubmatch(content); sm != nil {
				if p := sm[2]; filepath.IsAbs(p) && fileExists(p) {
					add("MEDIA:" + p)
				}
			}
			for _, tag := range reMediaTag.FindAllString(content, -1) {
				if fileExists(strings.TrimPrefix(tag, "MEDIA:")) {
					add(tag)
				}
			}
		case "user", "assistant":
			// Only include paths that actually exist: the model hallucinates.
			for _, sm := range reImagePath.FindAllStringSubmatch(content, -1) {
				if fileExists(sm[1]) {
					add("MEDIA:" + sm[1])
				}
			}
		}
	}
	return paths
}
