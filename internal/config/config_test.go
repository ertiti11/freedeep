package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProxyAuthorization(t *testing.T) {
	cases := []struct {
		auth, key string
		want      bool
	}{
		{"", "", true}, {"Bearer secret", "secret", true}, {"Bearer wrong", "secret", false},
		{"Basic secret", "secret", false}, {"Bearer secret ", "secret", false},
	}
	for _, c := range cases {
		if got := IsProxyAuthorized(c.auth, c.key); got != c.want {
			t.Errorf("IsProxyAuthorized(%q,%q)=%v", c.auth, c.key, got)
		}
	}
}

func TestProxyKeyFromSecretFile(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "proxy-api-key")
	if err := os.WriteFile(secret, []byte("mounted-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, _ := LoadProxyAPIKey(MapEnv(map[string]string{"PROXY_API_KEY_FILE": secret})); k != "mounted-secret" {
		t.Errorf("file key %q", k)
	}
	if k, _ := LoadProxyAPIKey(MapEnv(map[string]string{"PROXY_API_KEY": "env-secret", "PROXY_API_KEY_FILE": secret})); k != "env-secret" {
		t.Errorf("env precedence %q", k)
	}
	if k, err := LoadProxyAPIKey(MapEnv(map[string]string{"PROXY_API_KEY_FILE": filepath.Join(dir, "missing")})); k != "" || err != nil {
		t.Error("missing secret file should mean no key")
	}
	if RequireProxyAPIKey("mounted-secret", true) != nil || RequireProxyAPIKey("", true) == nil {
		t.Error("RequireProxyAPIKey")
	}
}

func TestLoopbackHosts(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "::1", "[::1]", "::ffff:127.0.0.1", "localhost"} {
		if !IsLoopbackHost(h) {
			t.Errorf("%s should be loopback", h)
		}
	}
	if IsLoopbackHost("0.0.0.0") {
		t.Error("0.0.0.0 is not loopback")
	}
}

func TestBrowserOriginGuard(t *testing.T) {
	allowed := []string{"https://ui.example.com", "chrome-extension://trusted-id"}
	for origin, want := range map[string]bool{
		"":                              true,
		"http://localhost:3000":         true,
		"http://127.0.0.1:8080":         true,
		"http://[::1]:3000":             true,
		"https://ui.example.com/path":   true,
		"HTTPS://UI.example.com:443":    true,
		"chrome-extension://trusted-id": true,
		"https://evil.example":          false,
		"chrome-extension://other-id":   false,
		"null":                          false,
	} {
		if got := IsBrowserOriginAllowed(origin, allowed); got != want {
			t.Errorf("%q: got %v want %v", origin, got, want)
		}
	}
}

func TestLoadDefaultsAndClamps(t *testing.T) {
	c, err := Load(MapEnv(map[string]string{"DEEPSEEK_MAX_PROMPT_CHARS": "100", "DEEPSEEK_MAX_RETRIES": "50", "PROXY_CORS_ORIGINS": "https://a.example/, ,http://b.example:80"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9655 || c.Host != "127.0.0.1" || c.MaxPromptChars != MinPromptChars || c.MaxEmptyRetries != 10 || !c.MediaInjection {
		t.Errorf("%+v", c)
	}
	if len(c.CORSOrigins) != 2 || c.CORSOrigins[0] != "https://a.example" || c.CORSOrigins[1] != "http://b.example" {
		t.Errorf("origins %v", c.CORSOrigins)
	}
}

func TestAuthPaths(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.json", "a.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := AuthPaths(dir, "")
	if len(got) != 2 || filepath.Base(got[0]) != "a.json" {
		t.Errorf("dir %v", got)
	}
	if got, _ := AuthPaths("", "x.json, y.json"); len(got) != 2 || got[1] != "y.json" {
		t.Errorf("list %v", got)
	}
	if SingleAuthPath(MapEnv(map[string]string{"DEEPSEEK_AUTH_PATH": "a,b"})) != DefaultAuthFile {
		t.Error("comma list must not be a write target")
	}
}

func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	content := "# c\nFREEDEEP_TEST_A=1\nexport FREEDEEP_TEST_B=\"two words\"\nFREEDEEP_TEST_C=x # comment\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FREEDEEP_TEST_A", "keep")
	os.Unsetenv("FREEDEEP_TEST_B")
	os.Unsetenv("FREEDEEP_TEST_C")
	t.Cleanup(func() { os.Unsetenv("FREEDEEP_TEST_B"); os.Unsetenv("FREEDEEP_TEST_C") })
	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FREEDEEP_TEST_A") != "keep" || os.Getenv("FREEDEEP_TEST_B") != "two words" || os.Getenv("FREEDEEP_TEST_C") != "x" {
		t.Error("dotenv values")
	}
}
