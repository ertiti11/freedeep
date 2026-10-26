package toolcall

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ertiti11/freedeep/internal/chat"
	"github.com/ertiti11/freedeep/internal/ojson"
)

func args(t *testing.T, c *Call) map[string]any {
	t.Helper()
	if c == nil {
		t.Fatal("expected a tool call, got nil")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(c.Arguments), &m); err != nil {
		t.Fatalf("arguments are not JSON: %v", err)
	}
	return m
}

func TestCanonicalDSML(t *testing.T) {
	dsml := strings.Join([]string{
		"I will inspect it.",
		"<｜DSML｜tool_calls>",
		`<｜DSML｜invoke name="execute_code">`,
		`<｜DSML｜parameter name="code" string="true">print("ok")</｜DSML｜parameter>`,
		`<｜DSML｜parameter name="timeout" string="false">30</｜DSML｜parameter>`,
		`<｜DSML｜parameter name="capture" string="false">true</｜DSML｜parameter>`,
		"</｜DSML｜invoke>",
		"</｜DSML｜tool_calls>",
	}, "\n")
	c := Parse(dsml)
	got := args(t, c)
	want := map[string]any{"code": `print("ok")`, "timeout": float64(30), "capture": true}
	if c.Name != "execute_code" || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s %v", c.Name, got)
	}
	// Parameter order is preserved in the serialized arguments.
	if c.Arguments != `{"code":"print(\"ok\")","timeout":30,"capture":true}` {
		t.Errorf("arguments not order-preserving: %s", c.Arguments)
	}
}

func TestDoubledBarDSMLVariant(t *testing.T) {
	c := Parse("<｜｜DSML｜｜ Tool Calls>\n<｜｜DSML｜｜ name=\"web_search\">{\"query\":\"DeepSeek DSML\"}\n</｜｜DSML｜｜ Tool Calls>")
	if c == nil || c.Name != "web_search" || args(t, c)["query"] != "DeepSeek DSML" {
		t.Fatalf("got %+v", c)
	}
}

func TestDSMLVariants(t *testing.T) {
	zero := Parse(`<|DSML|tool_calls><|DSML|invoke name="ping"></|DSML|invoke></|DSML|tool_calls>`)
	if zero == nil || *zero != (Call{Name: "ping", Arguments: "{}"}) {
		t.Errorf("zero-arg: %+v", zero)
	}
	cdata := Parse("<tool_calls><invoke name=\"write_file\"><parameter name=\"content\"><![CDATA[line 1\n<line 2>\n</parameter>\n</invoke>\n</tool_calls>]]></parameter></invoke></tool_calls>")
	if got := args(t, cdata)["content"]; got != "line 1\n<line 2>\n</parameter>\n</invoke>\n</tool_calls>" {
		t.Errorf("cdata: %q", got)
	}
	collapsed := Parse(`<DSMLtool_calls><DSMLinvoke name="read_file"><DSMLparameter name="path">/tmp/a</DSMLparameter></DSMLinvoke></DSMLtool_calls>`)
	if args(t, collapsed)["path"] != "/tmp/a" {
		t.Error("collapsed")
	}
	prefixed := Parse(`<abc:tool_calls><abc:invoke name="read_file"><abc:parameter name="path">/tmp/b</abc:parameter></abc:invoke></abc:tool_calls>`)
	if args(t, prefixed)["path"] != "/tmp/b" {
		t.Error("prefixed")
	}
}

func TestFullwidthAndMissingOpeningWrapper(t *testing.T) {
	fw := Parse(`＜｜DSML｜Tool Calls＞＜｜DSML｜Invoke name=“read_file”＞＜｜DSML｜Parameter name=“path”＞/tmp/c＜/｜DSML｜Parameter＞＜/｜DSML｜Invoke＞＜/｜DSML｜Tool Calls＞`)
	if args(t, fw)["path"] != "/tmp/c" {
		t.Error("fullwidth")
	}
	repaired := Parse(`<invoke name="read_file"><parameter name="path">/tmp/d</parameter></invoke></tool_calls>`)
	if args(t, repaired)["path"] != "/tmp/d" {
		t.Error("repaired")
	}
}

func TestRejectsBareInvokesAndExamples(t *testing.T) {
	bare := `<|DSML|invoke name="execute_code"><|DSML|parameter name="code">danger()</|DSML|parameter></|DSML|invoke>`
	if Parse(bare) != nil || !LooksLikeMarkup(bare) {
		t.Error("bare invoke must be rejected but recognized as markup")
	}
	for _, s := range []string{
		`For example return {"name":"execute_code","arguments":{"code":"danger()"}} when appropriate.`,
		"```json\n{\"name\":\"execute_code\",\"arguments\":{\"code\":\"danger()\"}}\n```",
	} {
		if Parse(s) != nil {
			t.Errorf("bare JSON example parsed: %s", s)
		}
	}
}

func TestExplicitJSONEnvelopes(t *testing.T) {
	if args(t, Parse(`Use this: {"tool_call":{"name":"read_file","arguments":{"path":"/tmp/a"}}}`))["path"] != "/tmp/a" {
		t.Error("tool_call envelope")
	}
	openai := `{"tool_calls":[{"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/tmp/b\"}"}}]}`
	if args(t, Parse(openai))["path"] != "/tmp/b" {
		t.Error("openai envelope")
	}
	if Parse(`{"tool_call":{"name":"read_file","arguments":"not-json"}}`) != nil {
		t.Error("non-JSON string arguments accepted")
	}
	if Parse(`{"tool_calls":[{"function":{"name":"read_file","arguments":"{}"}},{"function":{"name":"write_file","arguments":"{}"}}]}`) != nil {
		t.Error("multiple tool calls accepted")
	}
	legacy := Parse("TOOL_CALL: read_file\narguments: {\"path\":\"/tmp/l\"}")
	if args(t, legacy)["path"] != "/tmp/l" {
		t.Error("legacy")
	}
	xml := Parse(`<tool_call>{"name":"read_file","arguments":{"path":"/tmp/x"}}</tool_call>`)
	if args(t, xml)["path"] != "/tmp/x" {
		t.Error("xml wrapper with bare object")
	}
}

func TestBoundsAndLinearTime(t *testing.T) {
	if Parse("<|DSML|tool_calls>"+strings.Repeat("x", 256*1024)+"</|DSML|tool_calls>") != nil {
		t.Error("oversized markup parsed")
	}
	start := time.Now()
	if Parse(strings.Repeat("{", 64*1024)) != nil {
		t.Error("unmatched braces parsed")
	}
	malformed := `<tool_calls><invoke name="` + strings.Repeat(`<invoke name="`, 16000) + `</tool_calls>`
	if Parse(malformed) != nil {
		t.Error("malformed tags parsed")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("pathological inputs took %v", d)
	}
}

func TestRefusesIncompleteDSMLWithEmbeddedJSON(t *testing.T) {
	malformed := strings.Join([]string{
		"<｜DSML｜tool_calls>",
		`<｜DSML｜invoke name="execute_code">`,
		`{"name":"dangerous_fallback","code":"rm -rf /"}`,
		"</｜DSML｜tool_calls>",
	}, "\n")
	if Parse(malformed) != nil || !LooksLikeMarkup(malformed) {
		t.Error("incomplete DSML must be refused")
	}
}

func TestRejectsPartiallyConsumedDSML(t *testing.T) {
	var many strings.Builder
	for i := 0; i < 129; i++ {
		fmt.Fprintf(&many, `<parameter name="p%d">%d</parameter>`, i, i)
	}
	cases := []string{
		`<tool_calls><invoke name="write_file"><parameter name="path">/tmp/a</parameter><parameter name="content">truncated</invoke></tool_calls>`,
		`<tool_calls><invoke name="write_file"><parameter name="path">/tmp/a</parameter>GARBAGE</invoke></tool_calls>`,
		`<tool_calls><invoke name="ping"></invoke><invoke name="write_file"><parameter name="path">/tmp/a</tool_calls>`,
		`<tool_calls><invoke name="ping"></invoke><invoke name="ping"></invoke></tool_calls>`,
		`<tool_calls><invoke name="ping"></invoke></tool_calls><invoke name="write_file"><parameter name="path">/tmp/a</parameter></invoke>`,
		`<tool_calls><invoke name="ping"></invoke>GARBAGE</tool_calls>`,
		`<tool_calls><invoke name="write_file"><parameter name="content"><![CDATA[truncated</parameter></invoke></tool_calls>`,
		`<tool_calls><invoke name="write_file">` + many.String() + `</invoke></tool_calls>`,
	}
	for _, c := range cases {
		if Parse(c) != nil {
			t.Errorf("parsed malformed: %.120s", c)
		}
		if !LooksLikeMarkup(c) {
			t.Errorf("not recognized as markup: %.120s", c)
		}
	}
}

func mustParse(t *testing.T, s string) any {
	t.Helper()
	v, err := ojson.ParseString(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCompactSchemaDropsAnnotations(t *testing.T) {
	in := mustParse(t, `{"type":"object","description":"large top-level description","properties":{
		"command":{"type":"string","description":"large property description"},
		"count":{"type":"integer","minimum":1},
		"description":{"type":"string","description":"annotation, not the property name"},
		"title":{"type":"boolean","title":"annotation, not the property name"},
		"nested":{"anyOf":[{"type":"string","description":"x"},{"type":"integer","title":"y"}]}},
		"required":["command","description","title"]}`)
	got := ojson.MarshalString(CompactSchema(in))
	want := `{"type":"object","properties":{"command":{"type":"string"},"count":{"type":"integer","minimum":1},"description":{"type":"string"},"title":{"type":"boolean"},"nested":{"anyOf":[{"type":"string"},{"type":"integer"}]}},"required":["command","description","title"]}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestCompactSchemaPreservesLiterals(t *testing.T) {
	in := mustParse(t, `{"type":"object","description":"drop",
		"const":{"description":"literal field","title":"literal title","nested":{"examples":["literal"]}},
		"enum":[{"description":"first","value":1},{"title":"second","value":2}],
		"default":{"description":"default literal","title":"default title"},
		"properties":{"choice":{"description":"drop nested","const":{"description":"required argument value","title":"keep me"}}}}`)
	got := ojson.MarshalString(CompactSchema(in))
	want := `{"type":"object","const":{"description":"literal field","title":"literal title","nested":{"examples":["literal"]}},"enum":[{"description":"first","value":1},{"title":"second","value":2}],"default":{"description":"default literal","title":"default title"},"properties":{"choice":{"const":{"description":"required argument value","title":"keep me"}}}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestFormatDefinitionsCompactsOnlyLargeSchemas(t *testing.T) {
	small := []chat.Tool{{Type: "function", HasFunction: true, Name: "read_file", Description: "Read  a\nfile",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"keep me"}}}`)}}
	out := FormatDefinitions(small, FormatOptions{Host: "h", IP: "1.2.3.4", MaxPromptChars: 80000})
	for _, want := range []string{"## read_file\nRead a file\n", `"description":"keep me"`, "runs on h (1.2.3.4)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	big := small
	big[0].Parameters = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"` + strings.Repeat("d", 40000) + `"}}}`)
	out = FormatDefinitions(big, FormatOptions{MaxPromptChars: 80000})
	if strings.Contains(out, "ddd") || !strings.Contains(out, `Parameters: {"type":"object","properties":{"path":{"type":"string"}}}`) {
		t.Errorf("large schema not compacted")
	}
	if FormatDefinitions(nil, FormatOptions{}) != "" {
		t.Error("no tools should produce no adapter")
	}
}
