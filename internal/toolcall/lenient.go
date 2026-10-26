package toolcall

import (
	"log/slog"
	"strings"

	"github.com/ertiti11/freedeep/internal/ojson"
)

// parseLenientJSON parses JSON, falling back to repairJSON when the model
// produced the two mistakes it makes most often inside string literals:
// unescaped backslashes (Windows paths such as C:\pentiti) and raw control
// characters (multi-line shell commands, here-strings, tabs).
func parseLenientJSON(raw string) (any, error) {
	v, err := ojson.ParseString(raw)
	if err == nil {
		return v, nil
	}
	fixed, changed := repairJSON(raw)
	if !changed {
		return nil, err
	}
	v, rerr := ojson.ParseString(fixed)
	if rerr != nil {
		return nil, err
	}
	slog.Debug("parseToolCall: repaired invalid JSON string escapes", "err", truncate(err.Error(), 100))
	return v, nil
}

// repairJSON escapes raw control characters and invalid backslash escapes
// inside JSON string literals. Valid escapes are kept as they are, so valid
// JSON is never changed. It reports whether anything was rewritten.
func repairJSON(s string) (string, bool) {
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 16)
	inString, changed := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inString {
			if ch == '"' {
				inString = true
			}
			b.WriteByte(ch)
			continue
		}
		switch {
		case ch == '"':
			inString = false
			b.WriteByte(ch)
		case ch == '\\':
			if validEscape(s, i+1) {
				b.WriteString(s[i : i+2])
				i++
			} else {
				b.WriteString(`\\`)
				changed = true
			}
		case ch < 0x20:
			changed = true
			switch ch {
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				b.WriteString(`\u00`)
				b.WriteByte(hex[ch>>4])
				b.WriteByte(hex[ch&0xf])
			}
		default:
			b.WriteByte(ch)
		}
	}
	return b.String(), changed
}

// validEscape reports whether s[i] starts a valid JSON escape (after '\').
func validEscape(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	switch s[i] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return true
	case 'u':
		if i+4 >= len(s) {
			return false
		}
		for j := i + 1; j <= i+4; j++ {
			if !strings.ContainsRune("0123456789abcdefABCDEF", rune(s[j])) {
				return false
			}
		}
		return true
	}
	return false
}
