// Package toolcall injects tool definitions into DeepSeek prompts and parses
// tool requests back out of the model's plain-text answers.
//
// DeepSeek Web has no native function calling. The model is asked to reply
// with a strict JSON envelope; in practice it also emits a legacy
// "TOOL_CALL:" form and DeepSeek's own DSML markup. Parsing is deliberately
// strict: only explicit envelopes are executable, and malformed or truncated
// markup is rejected instead of being guessed at.
package toolcall

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ertiti11/freedeep/internal/chat"
	"github.com/ertiti11/freedeep/internal/ojson"
)

// Call is a parsed tool request. Arguments is a JSON-encoded object.
type Call struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

const (
	MaxMarkupChars        = 256 * 1024
	MaxArgumentChars      = 128 * 1024
	maxJSONCandidates     = 32
	maxDSMLParameters     = 128
	maxDSMLStructuralTags = maxDSMLParameters*2 + 16
	maxDSMLTagChars       = 2048
	maxRawTagChars        = 1024
)

var (
	reToolName       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,127}$`)
	reDSMLBarPrefix  = regexp.MustCompile(`(?i)^\|+\s*DSML\s*\|+\s*`)
	reDSMLCollapsed  = regexp.MustCompile(`(?i)^DSML((?:tool[\s_-]*calls|function[\s_-]*calls|invoke|parameter)\b)`)
	reDirectName     = regexp.MustCompile(`(?i)^name\s*=`)
	reSemanticTag    = regexp.MustCompile(`(?is)^(?:(?:[A-Za-z_][\w.-]*):)?(tool[\s_-]*calls|function[\s_-]*calls|invoke|parameter)\b(.*)$`)
	reSepChars       = regexp.MustCompile(`[\s_-]`)
	reCDATA          = regexp.MustCompile(`(?is)^<!\[CDATA\[(.*?)\]\]>$`)
	reTagPrefix      = regexp.MustCompile(`(?i)^/?(?:tool_calls|invoke|parameter|direct)\b`)
	reTagToken       = regexp.MustCompile(`(?is)^(tool_calls|invoke|parameter|direct)\b(.*)$`)
	reAttrName       = regexp.MustCompile(`(?i)\bname\s*=\s*(?:"([^"']+)"|'([^"']+)')`)
	reAttrString     = regexp.MustCompile(`(?i)\bstring\s*=\s*(?:"([^"']+)"|'([^"']+)')`)
	reDSMLDetect     = regexp.MustCompile(`(?i)[|｜]+\s*DSML\s*[|｜]+|[<＜]\s*/?\s*(?:DSML)?(?:[\w.-]+:)?(?:tool[\s_-]*calls|function[\s_-]*calls|invoke)\b`)
	reLooksLike      = regexp.MustCompile(`(?i)TOOL_CALL:\s*[\w-]+|<\s*tool_call\b|[|｜]+\s*DSML\s*[|｜]+|[<＜]\s*/?\s*(?:DSML)?(?:[\w.-]+:)?(?:tool[\s_-]*calls|function[\s_-]*calls|invoke)\b|["'](?:tool_call|tool_calls|function_call)["']\s*:`)
	reXMLToolCall    = regexp.MustCompile(`(?is)<tool_call[^>]*>(.*?)</tool_call>`)
	reFence          = regexp.MustCompile("(?is)```(?:json)?\\s*(.*?)```")
	reLegacyToolCall = regexp.MustCompile(`(?i)TOOL_CALL:\s*([\w-]+)\s*`)
	entityReplacers  = []struct {
		re  *regexp.Regexp
		out string
	}{
		{regexp.MustCompile(`(?i)&quot;`), `"`},
		{regexp.MustCompile(`(?i)&apos;`), `'`},
		{regexp.MustCompile(`(?i)&lt;`), `<`},
		{regexp.MustCompile(`(?i)&gt;`), `>`},
		{regexp.MustCompile(`(?i)&amp;`), `&`},
	}
	quoteNormalizer = strings.NewReplacer("｜", "|", "“", `"`, "”", `"`, "＂", `"`, "‘", "'", "’", "'", "＇", "'")
)

// ---------------------------------------------------------------------------
// Prompt injection

var (
	schemaAnnotationKeys = set("description", "examples", "$comment", "title")
	schemaMapKeys        = set("properties", "patternProperties", "$defs", "definitions", "dependentSchemas")
	schemaArrayKeys      = set("allOf", "anyOf", "oneOf", "prefixItems")
	schemaSingleKeys     = set("additionalItems", "additionalProperties", "contains", "contentSchema", "else", "if",
		"items", "not", "propertyNames", "then", "unevaluatedItems", "unevaluatedProperties")
)

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// CompactSchema drops prose annotations (description, title, examples...)
// from a JSON schema while keeping its validation shape. Only keywords whose
// values are themselves schemas are traversed, so literal const/enum/default
// values stay byte-for-byte intact even if they contain "description".
func CompactSchema(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, c := range v {
			out[i] = CompactSchema(c)
		}
		return out
	case ojson.Object:
		out := ojson.Object{}
		for _, m := range v {
			key, child := m.Key, m.Value
			if schemaAnnotationKeys[key] {
				continue
			}
			childObj, isObj := child.(ojson.Object)
			childArr, isArr := child.([]any)
			switch {
			case schemaMapKeys[key] && isObj:
				mapped := make(ojson.Object, len(childObj))
				for i, cm := range childObj {
					mapped[i] = ojson.Member{Key: cm.Key, Value: CompactSchema(cm.Value)}
				}
				out = append(out, ojson.Member{Key: key, Value: mapped})
			case schemaArrayKeys[key] && isArr:
				out = append(out, ojson.Member{Key: key, Value: CompactSchema(childArr)})
			case schemaSingleKeys[key]:
				out = append(out, ojson.Member{Key: key, Value: CompactSchema(child)})
			case key == "dependencies" && isObj:
				mapped := make(ojson.Object, len(childObj))
				for i, cm := range childObj {
					if _, arr := cm.Value.([]any); arr {
						mapped[i] = cm
					} else {
						mapped[i] = ojson.Member{Key: cm.Key, Value: CompactSchema(cm.Value)}
					}
				}
				out = append(out, ojson.Member{Key: key, Value: mapped})
			default:
				out = append(out, m)
			}
		}
		return out
	}
	return value
}

// FormatOptions configures the injected tool system prompt.
type FormatOptions struct {
	Host           string // hostname of the machine executing tools
	IP             string
	MaxPromptChars int // schemas are compacted when they exceed 40% of this
}

func compactRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// FormatDefinitions renders the tool adapter appended to the system prompt.
func FormatDefinitions(tools []chat.Tool, opts FormatOptions) string {
	if len(tools) == 0 {
		return ""
	}
	rawSchemaChars := 0
	for _, t := range tools {
		rawSchemaChars += len(compactRaw(t.Parameters))
	}
	compact := rawSchemaChars > opts.MaxPromptChars*4/10

	var b strings.Builder
	b.WriteString("\n\n--- TOOL REQUEST SYSTEM ---\n")
	b.WriteString("You are an AI that ONLY REASONS and REQUESTS tool executions. You do NOT run any commands yourself.\n")
	b.WriteString("When you need data from the local server, REQUEST exactly one tool call. Prefer strict JSON:\n")
	b.WriteString(`{"tool_call":{"name":"<function_name>","arguments":{...}}}` + "\n\n")
	b.WriteString("Legacy format is also accepted: TOOL_CALL: <function_name>\narguments: <JSON arguments>\n\n")
	b.WriteString("Your response will be sent to the local gateway, which executes the command and sends the output back in the next message.\n\n")
	b.WriteString("RULES:\n")
	b.WriteString("1. You ONLY output the tool request — you never run anything yourself\n")
	b.WriteString("2. Do NOT simulate, guess, or fabricate command output — wait for the actual result\n")
	b.WriteString("3. The tool runs on " + opts.Host + " (" + opts.IP + "), the local server — NOT on DeepSeek\n")
	b.WriteString("4. After the tool executes, the result will be sent to you as a new user/tool message\n")
	b.WriteString("5. Never add explanation before or after the tool request when requesting a tool\n")
	b.WriteString("6. Keep arguments compact. Do not include large file contents unless the tool schema requires it.\n")
	b.WriteString(`7. Arguments must be valid JSON: inside strings escape every backslash as \\ (Windows paths: C:\\Users\\me) and every line break as \n.` + "\n\n")
	b.WriteString("Available functions:\n")
	for _, t := range tools {
		if t.Type != "function" || !t.HasFunction {
			continue
		}
		b.WriteString("\n## " + t.Name + "\n")
		desc := strings.Join(strings.Fields(t.Description), " ")
		if utf8.RuneCountInString(desc) > 500 {
			desc = string([]rune(desc)[:497]) + "..."
		}
		b.WriteString(desc + "\n")
		if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
			schema := compactRaw(t.Parameters)
			if compact {
				if v, err := ojson.Parse(t.Parameters); err == nil {
					schema = ojson.MarshalString(CompactSchema(v))
				}
			}
			b.WriteString("Parameters: " + schema + "\n")
		}
	}
	b.WriteString("\n--- END TOOL REQUEST SYSTEM ---\n")
	b.WriteString("\nREMEMBER: Request tools only with strict JSON or TOOL_CALL legacy format. Never simulate results.")
	return b.String()
}

// ---------------------------------------------------------------------------
// JSON helpers

func extractBalancedJSONAt(text string, start int) (string, bool) {
	if start < 0 || start >= len(text) || text[start] != '{' {
		return "", false
	}
	depth := 0
	inString, escape := false, false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if escape {
			escape = false
			continue
		}
		if ch == '\\' && inString {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if !inString {
			if ch == '{' {
				depth++
			}
			if ch == '}' {
				depth--
				if depth == 0 {
					return text[start : i+1], true
				}
			}
		}
	}
	return "", false
}

// extractBalancedJSONObjects scans top-level {...} spans in linear time.
func extractBalancedJSONObjects(text string, max int) []string {
	var objects []string
	start, depth := -1, 0
	inString, escape := false, false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if start == -1 {
			if ch == '{' {
				start, depth = i, 1
				inString, escape = false, false
			}
			continue
		}
		if escape {
			escape = false
			continue
		}
		if ch == '\\' && inString {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' {
			depth++
		}
		if ch == '}' {
			depth--
			if depth == 0 {
				objects = append(objects, text[start:i+1])
				if len(objects) >= max {
					return objects
				}
				start = -1
			}
		}
	}
	return objects
}

func buildCall(name any, args any) *Call {
	n, ok := name.(string)
	if !ok {
		return nil
	}
	n = strings.TrimSpace(n)
	if !reToolName.MatchString(n) {
		return nil
	}
	if s, isStr := args.(string); isStr {
		if len(s) > MaxArgumentChars {
			return nil
		}
		parsed, err := parseLenientJSON(s)
		if err != nil {
			return nil
		}
		args = parsed
	}
	if args == nil {
		args = ojson.Object{}
	}
	obj, ok := args.(ojson.Object)
	if !ok {
		return nil
	}
	serialized, err := ojson.Marshal(obj)
	if err != nil || len(serialized) > MaxArgumentChars {
		return nil
	}
	return &Call{Name: n, Arguments: string(serialized)}
}

func coalesce(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func getOr(o ojson.Object, key string) any { v, _ := o.Get(key); return v }

func coerceCallObject(value any, allowBare bool) *Call {
	obj, ok := value.(ojson.Object)
	if !ok {
		return nil
	}
	var candidate any
	switch {
	case obj.Has("tool_call"):
		candidate = getOr(obj, "tool_call")
	case obj.Has("function_call"):
		candidate = getOr(obj, "function_call")
	case obj.Has("tool_calls"):
		arr, ok := getOr(obj, "tool_calls").([]any)
		if !ok || len(arr) != 1 {
			return nil
		}
		candidate = arr[0]
	case allowBare:
		candidate = obj
	}
	cand, ok := candidate.(ojson.Object)
	if !ok {
		return nil
	}
	fn := cand
	if f, ok := getOr(cand, "function").(ojson.Object); ok {
		fn = f
	}
	return buildCall(
		coalesce(getOr(fn, "name"), getOr(cand, "name")),
		coalesce(getOr(fn, "arguments"), getOr(cand, "arguments"), getOr(cand, "input"), ojson.Object{}),
	)
}

func parseJSONCandidate(raw, label string, allowBare bool) *Call {
	if raw == "" {
		return nil
	}
	v, err := parseLenientJSON(raw)
	if err != nil {
		slog.Debug("parseToolCall: JSON parse failed", "label", label, "err", truncate(err.Error(), 100))
		return nil
	}
	if tc := coerceCallObject(v, allowBare); tc != nil {
		slog.Debug("parseToolCall: success", "label", label, "tool", tc.Name, "args_chars", len(tc.Arguments))
		return tc
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------------------
// DSML (DeepSeek markup) parsing

func canonicalizeTag(rawTag string) (string, bool) {
	token := quoteNormalizer.Replace(strings.TrimSpace(rawTag))
	closing := false
	if strings.HasPrefix(token, "/") {
		closing = true
		token = strings.TrimSpace(token[1:])
	}
	token = reDSMLBarPrefix.ReplaceAllString(token, "")
	if strings.HasPrefix(token, "/") {
		closing = true
		token = strings.TrimSpace(token[1:])
	}
	token = reDSMLCollapsed.ReplaceAllString(token, "$1")

	if !closing && reDirectName.MatchString(token) {
		return "<direct " + token + ">", true
	}
	m := reSemanticTag.FindStringSubmatch(token)
	if m == nil {
		return "", false
	}
	local := strings.ToLower(reSepChars.ReplaceAllString(m[1], ""))
	canonical := local
	if local == "toolcalls" || local == "functioncalls" {
		canonical = "tool_calls"
	}
	attrs := m[2]
	slash := ""
	if closing {
		attrs, slash = "", "/"
	}
	return "<" + slash + canonical + attrs + ">", true
}

// normalizeTags rewrites every <...> span (no nested angle brackets, at most
// 1024 runes) into its canonical DSML form when it is a tool-markup tag.
func normalizeTags(text string) string {
	text = strings.NewReplacer("＜", "<", "＞", ">").Replace(text)
	var b strings.Builder
	b.Grow(len(text))
	i := 0
	for i < len(text) {
		if text[i] != '<' {
			next := strings.IndexByte(text[i:], '<')
			if next == -1 {
				b.WriteString(text[i:])
				break
			}
			b.WriteString(text[i : i+next])
			i += next
			continue
		}
		end, runes := -1, 0
		for j := i + 1; j < len(text) && runes <= maxRawTagChars; {
			c := text[j]
			if c == '<' {
				break
			}
			if c == '>' {
				end = j
				break
			}
			_, size := utf8.DecodeRuneInString(text[j:])
			j += size
			runes++
		}
		if end == -1 || runes > maxRawTagChars {
			b.WriteByte('<')
			i++
			continue
		}
		if canonical, ok := canonicalizeTag(text[i+1 : end]); ok {
			b.WriteString(canonical)
		} else {
			b.WriteString(text[i : end+1])
		}
		i = end + 1
	}
	return b.String()
}

func decodeEntities(value string) string {
	for _, r := range entityReplacers {
		value = r.re.ReplaceAllLiteralString(value, r.out)
	}
	return value
}

func decodeParameterValue(raw string) string {
	if m := reCDATA.FindStringSubmatch(strings.TrimSpace(raw)); m != nil {
		return m[1]
	}
	return decodeEntities(raw)
}

func attribute(re *regexp.Regexp, attrs string) string {
	m := re.FindStringSubmatch(attrs)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

type tag struct {
	name        string
	attrs       string
	closing     bool
	selfClosing bool
	start, end  int
}

// readTagAt returns (tag, invalid). tag is nil when the '<' at start is not a
// structural tag. invalid means an unterminated structural tag was found.
func readTagAt(text string, start int) (*tag, bool) {
	if text[start] != '<' {
		return nil, false
	}
	prefix := strings.TrimLeft(text[start+1:min(len(text), start+40)], " \t\r\n\f\v")
	if !reTagPrefix.MatchString(prefix) {
		return nil, false
	}
	var quote byte
	end := -1
	scanEnd := min(len(text), start+maxDSMLTagChars+1)
	for i := start + 1; i < scanEnd; i++ {
		ch := text[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '>' {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, true
	}
	token := strings.TrimSpace(text[start+1 : end])
	closing, selfClosing := false, false
	if strings.HasPrefix(token, "/") {
		closing = true
		token = strings.TrimSpace(token[1:])
	}
	if !closing && strings.HasSuffix(token, "/") {
		selfClosing = true
		token = strings.TrimSpace(token[:len(token)-1])
	}
	m := reTagToken.FindStringSubmatch(token)
	if m == nil {
		return nil, false
	}
	attrs := m[2]
	if closing {
		attrs = ""
	}
	return &tag{name: strings.ToLower(m[1]), attrs: attrs, closing: closing, selfClosing: selfClosing, start: start, end: end + 1}, false
}

func scanStructuralTags(value string) ([]*tag, bool) {
	var tags []*tag
	for i := 0; i < len(value); {
		if i+9 <= len(value) && strings.EqualFold(value[i:i+9], "<![CDATA[") {
			cdataEnd := strings.Index(value[i+9:], "]]>")
			if cdataEnd == -1 {
				return nil, false
			}
			i += 9 + cdataEnd + 3
			continue
		}
		if value[i] != '<' {
			i++
			continue
		}
		t, invalid := readTagAt(value, i)
		if invalid {
			return nil, false
		}
		if t == nil {
			i++
			continue
		}
		tags = append(tags, t)
		if len(tags) > maxDSMLStructuralTags {
			return nil, false
		}
		i = t.end
	}
	return tags, true
}

func parseParameter(attrs, rawBody string, args *ojson.Object, seen map[string]bool) bool {
	name := attribute(reAttrName, attrs)
	if name == "" || !reToolName.MatchString(name) || seen[name] {
		return false
	}
	seen[name] = true
	stringMode := attribute(reAttrString, attrs)
	raw := decodeParameterValue(rawBody)
	if len(raw) > MaxArgumentChars {
		return false
	}
	var value any = raw
	if strings.EqualFold(stringMode, "false") {
		v, err := parseLenientJSON(strings.TrimSpace(raw))
		if err != nil {
			return false
		}
		value = v
	}
	args.Set(name, value)
	return true
}

func parseInvoke(name, body string) *Call {
	tags, ok := scanStructuralTags(body)
	if !ok {
		return nil
	}
	var params []*tag
	for _, t := range tags {
		if t.name != "parameter" {
			return nil
		}
		params = append(params, t)
	}
	args := ojson.Object{}
	seen := map[string]bool{}
	count, cursor := 0, 0
	for i := 0; i < len(params); i += 2 {
		opening := params[i]
		if opening.closing || opening.selfClosing || i+1 >= len(params) || !params[i+1].closing {
			return nil
		}
		closing := params[i+1]
		if strings.TrimSpace(body[cursor:opening.start]) != "" {
			return nil
		}
		count++
		if count > maxDSMLParameters {
			return nil
		}
		if !parseParameter(opening.attrs, body[opening.end:closing.start], &args, seen) {
			return nil
		}
		cursor = closing.end
	}
	if count > 0 {
		if strings.TrimSpace(body[cursor:]) != "" {
			return nil
		}
		return buildCall(name, args)
	}
	decoded := strings.TrimSpace(decodeEntities(body))
	if decoded == "" {
		return buildCall(name, ojson.Object{})
	}
	objects := extractBalancedJSONObjects(decoded, 2)
	if len(objects) != 1 || decoded != objects[0] {
		return nil
	}
	v, err := parseLenientJSON(objects[0])
	if err != nil {
		return nil
	}
	return buildCall(name, v)
}

func extractScope(normalized string) (string, bool) {
	tags, ok := scanStructuralTags(normalized)
	if !ok {
		return "", false
	}
	var wrappers, openings, closings []*tag
	for _, t := range tags {
		if t.name == "tool_calls" {
			wrappers = append(wrappers, t)
			if t.closing {
				closings = append(closings, t)
			} else {
				openings = append(openings, t)
			}
		}
	}
	if len(openings) > 0 {
		if len(openings) != 1 || openings[0].selfClosing || len(closings) == 0 {
			return "", false
		}
		opening, closing := openings[0], closings[len(closings)-1]
		for _, w := range wrappers {
			if w.closing && w.start < opening.end {
				return "", false
			}
		}
		if closing.start < opening.end {
			return "", false
		}
		for _, t := range tags {
			if t.name != "tool_calls" && (t.start < opening.end || t.start >= closing.start) {
				return "", false
			}
		}
		return normalized[opening.end:closing.start], true
	}
	// Narrow repair: tolerate a missing opening wrapper only when a closing
	// wrapper exists. A bare invoke without this sentinel is never executable.
	if len(closings) > 0 {
		closing := closings[len(closings)-1]
		var invokes []*tag
		for _, t := range tags {
			if t.name == "invoke" && !t.closing && t.start < closing.start {
				invokes = append(invokes, t)
			}
		}
		if len(invokes) == 1 && !invokes[0].selfClosing {
			for _, t := range tags {
				if t.name != "tool_calls" && (t.start < invokes[0].start || t.start >= closing.start) {
					return "", false
				}
			}
			return normalized[invokes[0].start:closing.start], true
		}
	}
	return "", false
}

// ParseDSML parses DeepSeek's DSML tool-call markup.
func ParseDSML(text string) *Call {
	if len(text) > MaxMarkupChars {
		return nil
	}
	scope, ok := extractScope(normalizeTags(text))
	if !ok {
		return nil
	}
	tags, ok := scanStructuralTags(scope)
	if !ok || len(tags) == 0 {
		return nil
	}
	first := tags[0]
	if strings.TrimSpace(scope[:first.start]) != "" {
		return nil
	}
	if first.name == "invoke" && !first.closing && !first.selfClosing {
		var invokes []*tag
		for _, t := range tags {
			if t.name == "invoke" {
				invokes = append(invokes, t)
			}
		}
		if len(invokes) != 2 || invokes[0] != first || !invokes[1].closing {
			return nil
		}
		closing := invokes[1]
		if strings.TrimSpace(scope[closing.end:]) != "" {
			return nil
		}
		for _, t := range tags {
			if t.name == "tool_calls" || t.name == "direct" {
				return nil
			}
		}
		if c := parseInvoke(attribute(reAttrName, first.attrs), scope[first.end:closing.start]); c != nil {
			slog.Debug("parseToolCall: success", "label", "dsml", "tool", c.Name, "args_chars", len(c.Arguments))
			return c
		}
	}
	if first.name == "direct" && !first.closing && !first.selfClosing {
		for _, t := range tags[1:] {
			if t.name == "direct" || t.name == "invoke" || t.name == "tool_calls" {
				return nil
			}
		}
		if c := parseInvoke(attribute(reAttrName, first.attrs), scope[first.end:]); c != nil {
			slog.Debug("parseToolCall: success", "label", "dsml-direct", "tool", c.Name, "args_chars", len(c.Arguments))
			return c
		}
	}
	return nil
}

// LooksLikeMarkup reports whether text appears to contain a tool request,
// even an invalid one. Used to repair or reject instead of leaking markup.
func LooksLikeMarkup(text string) bool { return reLooksLike.MatchString(text) }

// Parse extracts one executable tool call from model output, or nil.
func Parse(text string) *Call {
	if text == "" {
		return nil
	}
	if len(text) > MaxMarkupChars {
		slog.Debug("parseToolCall: refusing oversized candidate", "chars", len(text))
		return nil
	}
	if reDSMLDetect.MatchString(text) {
		if c := ParseDSML(text); c != nil {
			return c
		}
		slog.Debug("parseToolCall: tool markup found but wrapper/invoke was incomplete or malformed")
		return nil
	}
	// XML-ish wrappers used by some agent prompts.
	if m := reXMLToolCall.FindStringSubmatch(text); m != nil {
		if c := parseJSONCandidate(strings.TrimSpace(m[1]), "xml", true); c != nil {
			return c
		}
	}
	// Fenced JSON blocks.
	for _, m := range reFence.FindAllStringSubmatch(text, -1) {
		if c := parseJSONCandidate(strings.TrimSpace(m[1]), "fenced", false); c != nil {
			return c
		}
	}
	// Legacy TOOL_CALL: name + first balanced JSON object after it.
	if loc := reLegacyToolCall.FindStringSubmatchIndex(text); loc != nil {
		name := text[loc[2]:loc[3]]
		after := text[loc[1]:]
		if brace := strings.IndexByte(after, '{'); brace != -1 {
			if raw, ok := extractBalancedJSONAt(after, brace); ok {
				if v, err := parseLenientJSON(raw); err == nil {
					if c := buildCall(name, v); c != nil {
						slog.Debug("parseToolCall: success", "label", "legacy", "tool", name)
						return c
					}
				}
			}
		}
	}
	// Only explicit tool-call envelopes are executable; bare {name, arguments}
	// examples in prose are not.
	for _, raw := range extractBalancedJSONObjects(text, maxJSONCandidates) {
		if c := parseJSONCandidate(raw, "inline", false); c != nil {
			return c
		}
	}
	return nil
}
