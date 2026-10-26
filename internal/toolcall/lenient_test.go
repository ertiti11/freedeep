package toolcall

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ertiti11/freedeep/internal/chat"
)

func TestParseRepairsUnescapedStrings(t *testing.T) {
	cases := []struct {
		name, text, wantArgs string
	}{
		{"windows path", `{"tool_call":{"name":"Bash","arguments":{"command":"Copy-Item .\pipeprobe.exe C:\pentiti\pipeprobe.exe"}}}`,
			`{"command":"Copy-Item .\\pipeprobe.exe C:\\pentiti\\pipeprobe.exe"}`},
		{"raw newlines", "{\"tool_call\":{\"name\":\"Bash\",\"arguments\":{\"command\":\"@'\r\npackage main\n'@ | Out-File x.go\"}}}",
			`{"command":"@'\r\npackage main\n'@ | Out-File x.go"}`},
		{"tab and control", "{\"tool_call\":{\"name\":\"Bash\",\"arguments\":{\"command\":\"a\tb\x01\"}}}",
			`{"command":"a\tb\u0001"}`},
		{"valid escapes kept", `{"tool_call":{"name":"Bash","arguments":{"command":"echo \"hi\" \u00e9 \\x\n"}}}`,
			`{"command":"echo \"hi\" é \\x\n"}`},
		{"short unicode escape", `{"tool_call":{"name":"Bash","arguments":{"command":"C:\u12"}}}`,
			`{"command":"C:\\u12"}`},
		{"fenced", "```json\n" + `{"tool_call":{"name":"Read","arguments":{"path":"C:\Users\vatit\go.mod"}}}` + "\n```",
			`{"path":"C:\\Users\\vatit\\go.mod"}`},
		{"legacy", "TOOL_CALL: Read\narguments: " + `{"path":"D:\data\x.txt"}`,
			`{"path":"D:\\data\\x.txt"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := Parse(c.text)
			if tc == nil {
				t.Fatalf("not parsed: %s", c.text)
			}
			if tc.Arguments != c.wantArgs {
				t.Errorf("args = %s, want %s", tc.Arguments, c.wantArgs)
			}
		})
	}
}

func TestRepairJSONLeavesValidJSONAlone(t *testing.T) {
	for _, s := range []string{`{"a":"\\ \" \/ \b \f \n \r \t \u0041"}`, `{"a":[1,2,{"b":null}]}`, `"x"`} {
		if out, changed := repairJSON(s); changed || out != s {
			t.Errorf("repairJSON(%s) changed it to %s", s, out)
		}
	}
	if Parse(`{"tool_call":{"name":"Bash","arguments":{"command":"ls"`) != nil {
		t.Error("truncated JSON must still be rejected")
	}
}

func TestFormatDefinitionsAsksForEscapes(t *testing.T) {
	tools := []chat.Tool{{Type: "function", HasFunction: true, Name: "run", Parameters: json.RawMessage(`{"type":"object"}`)}}
	out := FormatDefinitions(tools, FormatOptions{Host: "h", IP: "1.2.3.4", MaxPromptChars: 80000})
	if !strings.Contains(out, `escape every backslash as \\`) {
		t.Error("tool prompt must ask for escaped backslashes")
	}
}
