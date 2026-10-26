// Package authcli implements the account management commands: importing
// credentials, logging in through Chrome, and diagnosing auth files.
package authcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ertiti11/freedeep/internal/deepseek"
)

// SecureWriteJSON writes v as indented JSON readable only by the owner.
// The file is written to a temp file and renamed, so a crash never leaves a
// half-written credentials file behind.
func SecureWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_ = tmp.Chmod(0o600) // no-op on Windows
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// ReadCredentials loads a deepseek-auth.json file.
func ReadCredentials(path string) (deepseek.Credentials, error) {
	var c deepseek.Credentials
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("invalid JSON: %w", err)
	}
	return c, nil
}

var reDeepseekDomain = regexp.MustCompile(`(?i)(^|\.)deepseek\.com$`)

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// cookieArrayToHeader converts a browser cookie export (array of
// {domain,name,value}) into a Cookie header, keeping deepseek.com cookies.
func cookieArrayToHeader(cookies []any) string {
	var parts []string
	for _, raw := range cookies {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, value, domain := str(c["name"]), str(c["value"]), str(c["domain"])
		if name == "" || value == "" || !reDeepseekDomain.MatchString(strings.TrimPrefix(domain, ".")) {
			continue
		}
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

func normalizeCookie(input any) string {
	switch v := input.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		return cookieArrayToHeader(v)
	case map[string]any:
		if arr, ok := v["cookies"].([]any); ok {
			return cookieArrayToHeader(arr)
		}
		if s := str(v["cookie"]); s != "" {
			return s
		}
		return str(v["cookies"])
	}
	return ""
}

var reBearer = regexp.MustCompile(`(?i)^Bearer\s+`)

// NormalizeAuth accepts a ready deepseek-auth.json, the Chrome extension
// export, or a browser cookie export. envToken fills in a missing token.
func NormalizeAuth(input any, envToken string) deepseek.Credentials {
	obj, _ := input.(map[string]any)
	first := func(keys ...string) string {
		for _, k := range keys {
			if s := str(obj[k]); s != "" {
				return s
			}
		}
		return ""
	}
	token := first("token", "access_token", "accessToken", "auth_token")
	if token == "" {
		token = strings.TrimSpace(envToken)
	}
	c := deepseek.Credentials{
		Token:   reBearer.ReplaceAllString(token, ""),
		HifDliq: first("hif_dliq", "x-hif-dliq"),
		HifLeim: first("hif_leim", "x-hif-leim"),
		Cookie:  normalizeCookie(input),
		WasmURL: first("wasmUrl", "wasm_url"),
	}
	if c.WasmURL == "" {
		c.WasmURL = deepseek.DefaultWasmURL
	}
	return c
}

// Validate lists missing required fields.
func Validate(c deepseek.Credentials) []string {
	var errs []string
	if c.Token == "" {
		errs = append(errs, "token missing")
	}
	if c.Cookie == "" {
		errs = append(errs, "cookie missing")
	}
	// wasmUrl is optional: the proof-of-work is solved natively.
	return errs
}
