package deepseek

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/ertiti11/freedeep/internal/chat"
)

// ModelError is an {"type":"error"} event emitted inside the SSE stream.
type ModelError struct {
	Type         string
	Content      string
	FinishReason string
}

// StreamResult is the accumulated outcome of one completion stream.
type StreamResult struct {
	Content      string
	Reasoning    string
	MessageID    any // json.Number or string; nil when not seen
	FinishReason string
	ModelError   *ModelError
}

// IsOutputFragment reports whether a fragment is visible assistant output.
// SEARCH fragments carry the answer text of web-search turns.
func IsOutputFragment(f map[string]any) bool {
	t, _ := f["type"].(string)
	_, isStr := f["content"].(string)
	return isStr && (t == "RESPONSE" || t == "SEARCH")
}

// IsReasoningFragment reports whether a fragment is thinking output.
func IsReasoningFragment(f map[string]any) bool {
	t, _ := f["type"].(string)
	_, isStr := f["content"].(string)
	return isStr && (t == "THINK" || t == "REASONING")
}

// IsModelErrorEvent reports whether a decoded SSE event is a model error.
func IsModelErrorEvent(event map[string]any) bool {
	t, _ := event["type"].(string)
	return t == "error"
}

// RebuildFragmentText concatenates response and reasoning fragment text.
func RebuildFragmentText(fragments []map[string]any) (response, think string) {
	var r, t strings.Builder
	for _, f := range fragments {
		if IsOutputFragment(f) {
			r.WriteString(f["content"].(string))
		} else if IsReasoningFragment(f) {
			t.WriteString(f["content"].(string))
		}
	}
	return r.String(), t.String()
}

// ApplyResponsePatchOperations applies {"p":"fragments","o":"APPEND"} ops.
func ApplyResponsePatchOperations(ops any, appendFragments func(any)) bool {
	list, ok := ops.([]any)
	if !ok {
		return false
	}
	applied := false
	for _, raw := range list {
		op, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		v, hasV := op["v"]
		if op["p"] == "fragments" && op["o"] == "APPEND" && hasV {
			appendFragments(v)
			applied = true
		}
	}
	return applied
}

// jsString mirrors JavaScript's `${value}` for scalars.
func jsString(v any) string { return chat.String(v) }

type streamState struct {
	lastPath  any
	fragments []map[string]any
	res       StreamResult
}

func (s *streamState) rebuild() {
	resp, think := RebuildFragmentText(s.fragments)
	if resp != "" {
		s.res.Content = resp
	}
	s.res.Reasoning = think
}

func (s *streamState) appendFragments(v any) {
	items, ok := v.([]any)
	if !ok {
		items = []any{v}
	}
	for _, it := range items {
		if f, ok := it.(map[string]any); ok {
			cp := make(map[string]any, len(f))
			for k, val := range f {
				cp[k] = val
			}
			s.fragments = append(s.fragments, cp)
		}
	}
	s.rebuild()
}

func (s *streamState) handle(d map[string]any) {
	if id, ok := d["response_message_id"]; ok && s.res.MessageID == nil {
		s.res.MessageID = id
	}
	if IsModelErrorEvent(d) {
		s.res.ModelError = &ModelError{Type: "error", Content: jsString(d["content"]), FinishReason: jsString(d["finish_reason"])}
	}
	if chat.Truthy(d["finish_reason"]) {
		s.res.FinishReason = jsString(d["finish_reason"])
	}
	if p, ok := d["p"]; ok {
		s.lastPath = p
	}
	v, hasV := d["v"]
	if vm, ok := v.(map[string]any); ok {
		if resp, ok := vm["response"].(map[string]any); ok {
			if id, ok := resp["message_id"]; ok {
				s.res.MessageID = id
			}
			if c, ok := resp["content"]; ok {
				s.res.Content = jsString(c)
			}
			if frags, ok := resp["fragments"].([]any); ok {
				s.fragments = s.fragments[:0]
				s.appendFragments(frags)
			}
			if fr, ok := resp["finish_reason"]; ok {
				s.res.FinishReason = jsString(fr)
			}
		}
	}
	if !hasV {
		return
	}
	_, vIsMap := v.(map[string]any)
	_, vIsArr := v.([]any)
	scalar := v != nil && !vIsMap && !vIsArr
	switch s.lastPath {
	case "response/fragments":
		s.appendFragments(v)
	case "response":
		ApplyResponsePatchOperations(v, s.appendFragments)
	case "response/fragments/-1/content":
		if scalar && len(s.fragments) > 0 {
			last := s.fragments[len(s.fragments)-1]
			last["content"] = jsString(last["content"]) + jsString(v)
			s.rebuild()
		}
	case "response/content":
		if scalar {
			s.res.Content += jsString(v)
		}
	case "response/finish_reason":
		s.res.FinishReason = jsString(v)
	case "response/status":
		if v != "FINISHED" {
			s.res.FinishReason = jsString(v)
		}
	}
}

// ReadStream consumes a DeepSeek completion SSE stream. onUpdate (optional)
// is called with the accumulated content and reasoning whenever either one
// changes, which lets callers stream deltas to their own clients live.
func ReadStream(r io.Reader, onUpdate func(content, reasoning string)) (StreamResult, error) {
	st := &streamState{}
	br := bufio.NewReaderSize(r, 64<<10)
	var prevContent, prevReasoning string
	for {
		line, err := br.ReadString('\n')
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimSpace(line[len("data: "):])
			var d map[string]any
			dec := json.NewDecoder(strings.NewReader(payload))
			dec.UseNumber()
			if dec.Decode(&d) == nil && d != nil {
				st.handle(d)
				if onUpdate != nil && (st.res.Content != prevContent || st.res.Reasoning != prevReasoning) {
					prevContent, prevReasoning = st.res.Content, st.res.Reasoning
					onUpdate(prevContent, prevReasoning)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return st.res, nil
			}
			return st.res, err
		}
	}
}
