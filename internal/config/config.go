// Package config reads runtime settings from the environment.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config is the full runtime configuration.
type Config struct {
	Host               string
	Port               int
	ProxyAPIKey        string
	RequireProxyAPIKey bool
	CORSOrigins        []string
	AuthPath           string
	AuthDir            string
	AccountCooldown    time.Duration
	MaxPromptChars     int
	MaxEmptyRetries    int
	MaxConcurrent      int
	RequestDeadline    time.Duration
	FetchTimeout       time.Duration
	StreamIdleTimeout  time.Duration
	ContinueOverChars  int
	MediaInjection     bool
	ShowReasoning      bool
	StreamTools        bool
	NonInteractive     bool
	ClientLocale       string
	ClientTZOffset     string
	LogLevel           string
	LogFormat          string
}

// MinPromptChars is the floor for DEEPSEEK_MAX_PROMPT_CHARS.
const MinPromptChars = 16000

// DefaultAuthFile is the default single-account credentials file.
const DefaultAuthFile = "deepseek-auth.json"

// Env abstracts environment lookup (tests pass a map).
type Env func(string) string

// OSEnv reads the process environment.
func OSEnv(key string) string { return os.Getenv(key) }

// MapEnv builds an Env from a map.
func MapEnv(m map[string]string) Env { return func(k string) string { return m[k] } }

// IsTruthy accepts 1/true/yes/on (case-insensitive).
func IsTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func intOr(env Env, key string, def int) int {
	if v := strings.TrimSpace(env(key)); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return int(n)
		}
	}
	return def
}

func msOr(env Env, key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(env(key)); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
			return time.Duration(n * float64(time.Millisecond))
		}
	}
	return def
}

func strOr(env Env, key, def string) string {
	if v := env(key); v != "" {
		return v
	}
	return def
}

// Load builds a Config from env. It fails only on unreadable secret files.
func Load(env Env) (*Config, error) {
	key, err := LoadProxyAPIKey(env)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Host:               strOr(env, "HOST", "127.0.0.1"),
		Port:               intOr(env, "PORT", 9655),
		ProxyAPIKey:        key,
		RequireProxyAPIKey: IsTruthy(env("REQUIRE_PROXY_API_KEY")),
		AuthPath:           strOr(env, "DEEPSEEK_AUTH_PATH", DefaultAuthFile),
		AuthDir:            env("DEEPSEEK_AUTH_DIR"),
		AccountCooldown:    msOr(env, "DEEPSEEK_ACCOUNT_COOLDOWN_MS", 10*time.Minute),
		MaxConcurrent:      max(1, intOr(env, "DEEPSEEK_MAX_CONCURRENT", 24)),
		RequestDeadline:    msOr(env, "DEEPSEEK_REQUEST_DEADLINE_MS", 120*time.Second),
		FetchTimeout:       msOr(env, "DEEPSEEK_FETCH_TIMEOUT_MS", 60*time.Second),
		StreamIdleTimeout:  msOr(env, "DEEPSEEK_STREAM_IDLE_TIMEOUT_MS", 90*time.Second),
		ContinueOverChars:  max(0, intOr(env, "DEEPSEEK_CONTINUE_OVER_CHARS", 25000)),
		MediaInjection:     env("DEEPSEEK_MEDIA_INJECTION") == "" || IsTruthy(env("DEEPSEEK_MEDIA_INJECTION")),
		ShowReasoning:      env("DEEPSEEK_SHOW_REASONING") == "" || IsTruthy(env("DEEPSEEK_SHOW_REASONING")),
		StreamTools:        env("DEEPSEEK_STREAM_WITH_TOOLS") == "" || IsTruthy(env("DEEPSEEK_STREAM_WITH_TOOLS")),
		NonInteractive:     IsTruthy(env("NON_INTERACTIVE")) || IsTruthy(env("SKIP_ACCOUNT_MENU")),
		ClientLocale:       strOr(env, "DEEPSEEK_CLIENT_LOCALE", "ru"),
		ClientTZOffset:     strOr(env, "DEEPSEEK_CLIENT_TIMEZONE_OFFSET", "14400"),
		LogLevel:           strOr(env, "LOG_LEVEL", "info"),
		LogFormat:          strOr(env, "LOG_FORMAT", "text"),
	}
	c.MaxEmptyRetries = 2
	if v := strings.TrimSpace(env("DEEPSEEK_MAX_RETRIES")); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.MaxEmptyRetries = max(0, min(10, int(n)))
		}
	}
	c.MaxPromptChars = 80000
	if v := strings.TrimSpace(env("DEEPSEEK_MAX_PROMPT_CHARS")); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.MaxPromptChars = max(MinPromptChars, int(n))
		}
	}
	for _, o := range strings.Split(env("PROXY_CORS_ORIGINS"), ",") {
		if o = NormalizeOrigin(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	return c, nil
}

// LoadProxyAPIKey returns PROXY_API_KEY, or the trimmed content of
// PROXY_API_KEY_FILE. A missing secret file counts as "no key"; deployments
// that need one set REQUIRE_PROXY_API_KEY=1 and fail closed.
func LoadProxyAPIKey(env Env) (string, error) {
	if k := env("PROXY_API_KEY"); k != "" {
		return k, nil
	}
	path := strings.TrimSpace(env("PROXY_API_KEY_FILE"))
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("could not read PROXY_API_KEY_FILE (%s): %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// RequireProxyAPIKey fails when a key is required but missing.
func RequireProxyAPIKey(key string, required bool) error {
	if required && key == "" {
		return errors.New("PROXY_API_KEY is required. Set PROXY_API_KEY or mount a secret and set PROXY_API_KEY_FILE")
	}
	return nil
}

// AuthPaths lists the account files: every *.json in AuthDir, a
// comma-separated AuthPath, or the single AuthPath.
func (c *Config) AuthPaths() ([]string, error) {
	return AuthPaths(c.AuthDir, c.AuthPath)
}

// AuthPaths is the env-independent form of Config.AuthPaths.
func AuthPaths(dir, path string) ([]string, error) {
	if dir != "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("could not read DEEPSEEK_AUTH_DIR: %w", err)
		}
		var out []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
		sort.Strings(out)
		return out, nil
	}
	if strings.Contains(path, ",") {
		var out []string
		for _, p := range strings.Split(path, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	}
	if path == "" {
		path = DefaultAuthFile
	}
	return []string{path}, nil
}

// SingleAuthPath is the output path for auth writers: DEEPSEEK_AUTH_PATH
// unless it is a comma-separated list.
func SingleAuthPath(env Env) string {
	p := env("DEEPSEEK_AUTH_PATH")
	if p == "" || strings.Contains(p, ",") {
		return DefaultAuthFile
	}
	return p
}

// LoadDotEnv sets variables from a KEY=VALUE file without overriding the
// existing environment. A missing file is not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if _, exists := os.LookupEnv(k); !exists && k != "" {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
