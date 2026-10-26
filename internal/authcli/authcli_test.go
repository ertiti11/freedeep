package authcli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runImport(t *testing.T, args []string, envToken string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunImport(ImportOptions{Args: args, DefaultOutput: "deepseek-auth.json", EnvToken: envToken,
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut})
	return code, out.String(), errOut.String()
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestImportCopiesValidAuthWithOwnerOnlyMode(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source.json"), filepath.Join(dir, "deepseek-auth.json")
	writeJSON(t, src, map[string]any{"token": "tok_123", "cookie": "ds_session_id=abc; other=def", "hif_dliq": "d", "hif_leim": "l", "wasmUrl": "https://example.com/sha3.wasm"})
	code, out, errOut := runImport(t, []string{"--input", src, "--output", dst}, "")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	c, err := ReadCredentials(dst)
	if err != nil || c.Token != "tok_123" || !strings.Contains(c.Cookie, "ds_session_id=abc") || c.HifDliq != "d" {
		t.Errorf("%+v %v", c, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(dst); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", st.Mode().Perm())
		}
	}
}

func TestImportBrowserCookieExportWithEnvToken(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "cookies.json"), filepath.Join(dir, "out.json")
	writeJSON(t, src, []any{
		map[string]any{"domain": ".deepseek.com", "name": "ds_session_id", "value": "abc"},
		map[string]any{"domain": "chat.deepseek.com", "name": "smidV2", "value": "smid"},
		map[string]any{"domain": "example.com", "name": "ignored", "value": "nope"},
		map[string]any{"domain": "notdeepseek.com", "name": "evil", "value": "x"},
	})
	if code, out, errOut := runImport(t, []string{"-i", src, "-o", dst}, "tok_env"); code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	c, _ := ReadCredentials(dst)
	if c.Token != "tok_env" || c.Cookie != "ds_session_id=abc; smidV2=smid" {
		t.Errorf("%+v", c)
	}
}

func TestImportRefusesTokenArgumentBeforeReadingFiles(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "cookies.json"), filepath.Join(dir, "out.json")
	writeJSON(t, src, []any{map[string]any{"domain": ".deepseek.com", "name": "ds_session_id", "value": "abc"}})
	for _, args := range [][]string{
		{"--input", src, "--output", dst, "--token", "tok_cli"},
		{"--token", "tok_cli"},
		{"--input", filepath.Join(dir, "missing.json"), "--token=tok_cli"},
	} {
		code, _, errOut := runImport(t, args, "")
		if code != ExitInvalid || !strings.Contains(errOut, "Refusing --token") {
			t.Errorf("%v: code %d %s", args, code, errOut)
		}
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("output must not be written")
	}
}

func TestImportRejectsIncompleteAuth(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "cookies.json")
	writeJSON(t, src, []any{map[string]any{"domain": ".deepseek.com", "name": "ds_session_id", "value": "abc"}})
	code, _, errOut := runImport(t, []string{"--input", src, "--output", filepath.Join(dir, "o.json")}, "")
	if code != ExitInvalid || !strings.Contains(errOut, "token missing") {
		t.Errorf("%d %s", code, errOut)
	}
}

func TestImportHelpShowsDefaultOutput(t *testing.T) {
	code, out, _ := runImport(t, []string{"--help"}, "")
	if code != 0 || !strings.Contains(out, "deepseek-auth.json") || !strings.Contains(out, "DEEPSEEK_TOKEN") {
		t.Errorf("%d %s", code, out)
	}
}

func TestNormalizeAuthVariants(t *testing.T) {
	c := NormalizeAuth(map[string]any{"access_token": "Bearer abc", "cookies": []any{map[string]any{"domain": "deepseek.com", "name": "a", "value": "1"}}, "x-hif-dliq": "d"}, "")
	if c.Token != "abc" || c.Cookie != "a=1" || c.HifDliq != "d" || c.WasmURL == "" {
		t.Errorf("%+v", c)
	}
}

func TestDoctorReportsProblemsOffline(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	writeJSON(t, broken, map[string]any{"token": "", "cookie": ""})
	var out bytes.Buffer
	code := RunDoctor(context.Background(), DoctorOptions{Paths: []string{broken, filepath.Join(dir, "missing.json")}, Offline: true, Stdout: &out})
	s := out.String()
	if code == 0 || !strings.Contains(s, "token missing") || !strings.Contains(s, "cookie missing") || !strings.Contains(s, "auth file missing") {
		t.Errorf("%d %s", code, s)
	}
}

func TestLoginExplainsMissingChromeForEveryOS(t *testing.T) {
	var out, errOut bytes.Buffer
	code := RunLogin(context.Background(), LoginOptions{ChromePath: filepath.Join(t.TempDir(), "missing-chrome"),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut})
	msg := errOut.String()
	if code == 0 {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"Windows", "macOS", "Linux", "CHROME_PATH"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %s", want, msg)
		}
	}
}

func TestNormalizeToken(t *testing.T) {
	if NormalizeToken(`{"value":"abc","__version":"0"}`) != "abc" || NormalizeToken(" raw ") != "raw" || NormalizeToken("") != "" {
		t.Error("NormalizeToken")
	}
	if tokenFromStores(map[string]string{"other": "x"}, map[string]string{"myToken": `{"token":"t2"}`}) != "t2" {
		t.Error("fallback key scan")
	}
}
