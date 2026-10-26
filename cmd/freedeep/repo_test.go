package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const root = "../.."

func TestSensitiveFilesAreNotTracked(t *testing.T) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skip("git metadata is not available")
	}
	for _, raw := range strings.Split(string(out), "\x00") {
		name := strings.ToLower(filepath.ToSlash(raw))
		base := filepath.Base(name)
		if name == "" || base == "auth.example.json" || base == ".env.example" {
			continue
		}
		sensitiveName := base == ".env" || strings.HasPrefix(base, ".env.") ||
			strings.Contains(base, "deepseek-auth") || strings.Contains(base, "cookies") ||
			strings.Contains(base, "credentials") || base == "auth.json" ||
			strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") ||
			strings.HasSuffix(base, ".p12") || strings.HasSuffix(base, ".pfx") ||
			strings.HasSuffix(base, ".session.json")
		sensitivePath := strings.Contains(name, "/accounts/") ||
			(strings.Contains(name, ".chrome") && strings.Contains(name, "profile"))
		if sensitiveName || sensitivePath {
			t.Errorf("sensitive-looking file is tracked: %s", raw)
		}
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func TestContainerfileIsMinimalAndFailClosed(t *testing.T) {
	cf := read(t, "Containerfile")
	ignore := read(t, ".containerignore")
	readme := read(t, "README.md")

	var copies []string
	for _, line := range strings.Split(cf, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "COPY ") {
			copies = append(copies, l)
		}
	}
	want := []string{
		"COPY go.mod ./",
		"COPY cmd ./cmd",
		"COPY internal ./internal",
		"COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
		"COPY --from=build /out/freedeep /freedeep",
	}
	if strings.Join(copies, "\n") != strings.Join(want, "\n") {
		t.Errorf("COPY lines changed:\n%s", strings.Join(copies, "\n"))
	}
	mustMatch := func(name, s string, patterns ...string) {
		for _, p := range patterns {
			if !regexp.MustCompile(p).MatchString(s) {
				t.Errorf("%s does not match %q", name, p)
			}
		}
	}
	if regexp.MustCompile(`(?m)^\s*(?:COPY|ADD)\s+\.\s`).MatchString(cf) {
		t.Error("Containerfile must not copy the whole context")
	}
	mustMatch("Containerfile", cf, `(?m)^FROM scratch$`, `(?m)^USER 1000:1000$`, `HOST=0\.0\.0\.0`, `NON_INTERACTIVE=1`,
		`REQUIRE_PROXY_API_KEY=1`, `PROXY_API_KEY_FILE=/run/secrets/proxy-api-key`, `(?m)^HEALTHCHECK `, `"healthcheck"`)
	mustMatch(".containerignore", ignore, `(?m)^\*$`)
	if regexp.MustCompile(`(?mi)^!.*(?:auth|secret|\.env)`).MatchString(ignore) {
		t.Error(".containerignore must not re-include secrets")
	}
	mustMatch("README.md", readme, `--publish 127\.0\.0\.1:9655:9655`, `--secret freedeep-auth[^\n]*mode=0400`,
		`--secret freedeep-proxy-key[^\n]*mode=0400`, `--read-only`, `--cap-drop=ALL`, `--security-opt=no-new-privileges`)
}

func TestChromeExtensionManifestIconsExist(t *testing.T) {
	dir := filepath.Join(root, "chrome-extension")
	var manifest map[string]any
	if err := json.Unmarshal([]byte(read(t, "chrome-extension/manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	var icons []string
	var walk func(v any, key string)
	walk = func(v any, key string) {
		switch x := v.(type) {
		case string:
			if key == "icons" || key == "default_icon" {
				icons = append(icons, x)
			}
		case map[string]any:
			for k, c := range x {
				if key == "icons" || key == "default_icon" {
					if s, ok := c.(string); ok {
						icons = append(icons, s)
					}
					continue
				}
				walk(c, k)
			}
		}
	}
	walk(manifest, "")
	for _, icon := range icons {
		if _, err := os.Stat(filepath.Join(dir, icon)); err != nil {
			t.Errorf("missing extension icon declared in manifest: %s", icon)
		}
	}
}
