// Command freedeep is an OpenAI / Anthropic compatible proxy for the
// DeepSeek Web chat, plus the tools to manage its DeepSeek logins.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ertiti11/freedeep/internal/authcli"
	"github.com/ertiti11/freedeep/internal/config"
	"github.com/ertiti11/freedeep/internal/deepseek"
	"github.com/ertiti11/freedeep/internal/server"
	"github.com/ertiti11/freedeep/internal/state"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `freedeep — OpenAI/Anthropic-compatible proxy for DeepSeek Web chat

Usage:
  freedeep [serve]          Start the proxy (interactive menu on a TTY)
  freedeep login            Open Chrome, log in to DeepSeek, save auth
  freedeep import [flags]   Import deepseek-auth.json / browser cookies
  freedeep auth [flag]      Account menu (--login, --import, --status, --remove)
  freedeep doctor [--offline]  Check auth files and live connectivity
  freedeep ask "prompt"     One-shot question from the terminal (or stdin)
  freedeep probe            Probe which DeepSeek Web model modes work
  freedeep healthcheck      Exit 0 if the local proxy answers /health
  freedeep version          Print the version

Configuration is read from the environment and from ./.env (see .env.example).
`

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not read .env:", err)
	}
	setupLogging(os.Getenv("LOG_LEVEL"), os.Getenv("LOG_FORMAT"))

	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var code int
	switch cmd {
	case "serve", "start":
		code = runServe(ctx)
	case "login":
		code = runLogin(ctx)
	case "import", "auth-import":
		code = authcli.RunImport(authcli.ImportOptions{Args: args, DefaultOutput: config.SingleAuthPath(config.OSEnv),
			EnvToken: os.Getenv("DEEPSEEK_TOKEN"), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	case "auth":
		code = runAuthMenu(ctx, args)
	case "doctor":
		code = runDoctor(ctx, args)
	case "ask", "client":
		code = runAsk(ctx, args)
	case "probe":
		code = runProbe(ctx)
	case "healthcheck":
		code = runHealthcheck()
	case "version", "--version", "-v":
		fmt.Println("freedeep", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		code = 2
	}
	os.Exit(code)
}

func setupLogging(level, format string) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(strings.ToLower(strings.TrimSpace(level)))); err != nil {
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

func newClient(cfg *config.Config) *deepseek.Client {
	c := deepseek.NewClient()
	c.Timeout = cfg.FetchTimeout
	c.IdleTimeout = cfg.StreamIdleTimeout
	c.Locale = cfg.ClientLocale
	c.TimezoneOffset = cfg.ClientTZOffset
	return c
}

func printBanner() {
	fmt.Printf(`
███████ ██████  ███████ ███████ ██████  ███████ ███████ ███████ ██   ██
██      ██   ██ ██      ██      ██   ██ ██      ██      ██      ██  ██
█████   ██████  █████   █████   ██   ██ █████   █████   █████   █████
██      ██   ██ ██      ██      ██   ██ ██      ██      ██      ██  ██
██      ██   ██ ███████ ███████ ██████  ███████ ███████ ███████ ██   ██

   freedeep %s — API proxy for DeepSeek Web Chat
   ForgetMeAI: %s

`, version, server.Watermark)
}

func isTTY() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func prompt(r *bufio.Reader, question string) string {
	fmt.Print(question)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func loadAccounts(cfg *config.Config, store *state.Store) int {
	paths, err := cfg.AuthPaths()
	if err != nil {
		slog.Error("auth source", "err", err)
		return 0
	}
	n, errs := store.LoadAccounts(paths)
	for _, e := range errs {
		slog.Warn(e.Error())
	}
	if n > 0 {
		var ids []string
		for _, a := range store.Accounts() {
			ids = append(ids, a.ID)
		}
		slog.Info("loaded auth accounts", "count", n, "ids", strings.Join(ids, ","))
	}
	return n
}

func authSource(cfg *config.Config) string {
	if cfg.AuthDir != "" {
		return cfg.AuthDir
	}
	return cfg.AuthPath
}

func printStatus(cfg *config.Config, store *state.Store) {
	fmt.Printf("\nForgetMeAI: %s\n", server.Watermark)
	if store.HasReadyCredentials() {
		fmt.Println("Auth: ✅ OK")
	} else {
		fmt.Println("Auth: ❌ no usable deepseek-auth.json")
	}
	fmt.Println("Auth source:", authSource(cfg))
	var accts []string
	for _, a := range store.Accounts() {
		s := a.ID
		if a.Cooldown {
			s += " (cooldown)"
		}
		accts = append(accts, s)
	}
	if len(accts) == 0 {
		accts = []string{"none"}
	}
	fmt.Println("Accounts:", strings.Join(accts, ", "))
	fmt.Println("Working models:", strings.Join(deepseek.SupportedModelIDs(), ", "))
	fmt.Println("Unsupported/hidden aliases:", strings.Join(deepseek.UnsupportedModelIDs(), ", "))
	fmt.Println("Capabilities: GET /v1/model-capabilities")
}

// startupMenu returns false when the user chose to exit.
func startupMenu(ctx context.Context, cfg *config.Config, store *state.Store) bool {
	in := bufio.NewReader(os.Stdin)
	for {
		printStatus(cfg, store)
		fmt.Println("\n=== Menu ===")
		fmt.Println("1 - Authorize / update DeepSeek login")
		fmt.Println("2 - Import auth file / cookies")
		fmt.Println("3 - Show models and statuses")
		fmt.Println("4 - Run proxy (default)")
		fmt.Println("5 - Exit")
		choice := prompt(in, "Your choice (Enter = 4): ")
		switch choice {
		case "1":
			runLogin(ctx)
			loadAccounts(cfg, store)
		case "2":
			authcli.RunImport(authcli.ImportOptions{DefaultOutput: config.SingleAuthPath(config.OSEnv),
				EnvToken: os.Getenv("DEEPSEEK_TOKEN"), Stdin: in, Stdout: os.Stdout, Stderr: os.Stderr})
			loadAccounts(cfg, store)
		case "3":
			b, _ := json.MarshalIndent(deepseek.Models, "", "  ")
			fmt.Println(string(b))
			prompt(in, "\nPress Enter to return to the menu...")
		case "", "4":
			if !store.HasReadyCredentials() {
				fmt.Println("A deepseek-auth.json is required. Use option 1 or 2.")
				continue
			}
			return true
		case "5":
			return false
		}
		if ctx.Err() != nil {
			return false
		}
	}
}

func runServe(ctx context.Context) int {
	printBanner()
	cfg, err := config.Load(config.OSEnv)
	if err != nil {
		slog.Error("configuration", "err", err)
		return 1
	}
	if err := config.RequireProxyAPIKey(cfg.ProxyAPIKey, cfg.RequireProxyAPIKey); err != nil {
		slog.Error(err.Error())
		return 1
	}
	if !config.IsLoopbackHost(cfg.Host) && cfg.ProxyAPIKey == "" {
		slog.Warn("HOST exposes the proxy without authentication; set PROXY_API_KEY or bind to 127.0.0.1", "host", cfg.Host)
	}
	store := state.NewStore(cfg.AccountCooldown)
	loaded := loadAccounts(cfg, store)
	if cfg.NonInteractive || !isTTY() {
		if loaded == 0 {
			paths, _ := cfg.AuthPaths()
			slog.Error("could not load any auth config", "expected", strings.Join(paths, ", "))
			return 1
		}
	} else if !startupMenu(ctx, cfg, store) {
		return 0
	}

	srv := server.New(cfg, store, newClient(cfg))
	addr := fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
	fmt.Printf(`[DS-API] Server on %s (multi-agent sessions enabled)
[DS-API] POST /v1/chat/completions — OpenAI Chat Completions (stream=true|false)
[DS-API] POST /v1/messages        — Anthropic Messages (Claude Code)
[DS-API] POST /v1/responses       — OpenAI Responses API
[DS-API] GET  /v1/models          — supported models
[DS-API] GET  /v1/model-capabilities — real model mapping and capabilities
[DS-API] GET  /v1/sessions        — active agent sessions
[DS-API] POST /reset-session?agent=<id>|all — reset agent session(s)
`, addr)
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		if strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "Only one usage of each socket address") {
			slog.Error(fmt.Sprintf("port %d already in use; set PORT=<other> or stop the other instance", cfg.Port))
		} else {
			slog.Error("server error", "err", err)
		}
		return 1
	}
	return 0
}

func envInt(key string, def int) int {
	var n int
	if _, err := fmt.Sscan(os.Getenv(key), &n); err == nil && n > 0 {
		return n
	}
	return def
}

func runLogin(ctx context.Context) int {
	profile := os.Getenv("DEEPSEEK_CHROME_PROFILE")
	if profile == "" {
		profile = ".chrome-for-testing-profile-deepseek"
	}
	profile, _ = filepath.Abs(profile)
	out, _ := filepath.Abs(config.SingleAuthPath(config.OSEnv))
	return authcli.RunLogin(ctx, authcli.LoginOptions{
		ChromePath:  os.Getenv("CHROME_PATH"),
		Port:        envInt("DEEPSEEK_CHROME_PORT", 9334),
		ProfileDir:  profile,
		OutPath:     out,
		ReuseChrome: config.IsTruthy(os.Getenv("DEEPSEEK_REUSE_CHROME")),
		KeepProfile: config.IsTruthy(os.Getenv("DEEPSEEK_KEEP_CHROME_PROFILE")),
		Stdin:       os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	})
}

func runDoctor(ctx context.Context, args []string) int {
	cfg, err := config.Load(config.OSEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[doctor] ERROR:", err)
		return 1
	}
	paths, err := cfg.AuthPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[doctor] ERROR:", err)
		return 1
	}
	offline := config.IsTruthy(os.Getenv("DOCTOR_OFFLINE"))
	for _, a := range args {
		if a == "--offline" || a == "-offline" {
			offline = true
		}
	}
	desc := "DEEPSEEK_AUTH_PATH/default"
	if cfg.AuthDir != "" {
		desc = "DEEPSEEK_AUTH_DIR"
	}
	return authcli.RunDoctor(ctx, authcli.DoctorOptions{Paths: paths, SourceDesc: desc, Offline: offline, Client: newClient(cfg), Stdout: os.Stdout})
}

func runAuthMenu(ctx context.Context, args []string) int {
	authPath := config.SingleAuthPath(config.OSEnv)
	status := func() {
		fmt.Println("\nDeepSeek account:")
		c, err := authcli.ReadCredentials(authPath)
		if err != nil {
			fmt.Println("  ❌ deepseek-auth.json not found")
			return
		}
		fmt.Println("  ✅ auth file:", authPath)
		if c.Token != "" {
			fmt.Printf("  token: OK (%d chars)\n", len(c.Token))
		} else {
			fmt.Println("  token: MISSING")
		}
		if c.Cookie != "" {
			fmt.Println("  cookies: OK")
		} else {
			fmt.Println("  cookies: MISSING")
		}
	}
	remove := func() {
		_ = os.Remove(authPath)
		fmt.Println("deepseek-auth.json removed. The Chrome profile is kept to avoid logging out of the browser unnecessarily.")
	}
	importAuth := func(in io.Reader) {
		authcli.RunImport(authcli.ImportOptions{DefaultOutput: authPath, EnvToken: os.Getenv("DEEPSEEK_TOKEN"), Stdin: in, Stdout: os.Stdout, Stderr: os.Stderr})
	}
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(`freedeep — DeepSeek Web login management
Options:
  --login     Open Chrome and update auth
  --import    Import a ready deepseek-auth.json / browser cookies
  --status    Show auth status
  --remove    Remove the local deepseek-auth.json
Without options an interactive menu is shown.
`)
			return 0
		case "--login", "--add", "--relogin":
			return runLogin(ctx)
		case "--import":
			importAuth(os.Stdin)
			return 0
		case "--status", "--list":
			status()
			return 0
		case "--remove":
			remove()
			return 0
		}
	}
	in := bufio.NewReader(os.Stdin)
	for ctx.Err() == nil {
		fmt.Println("======================================================")
		fmt.Println("ForgetMeAI:", server.Watermark)
		status()
		fmt.Println("======================================================")
		fmt.Println("1 - Authorize / update DeepSeek login")
		fmt.Println("2 - Import auth file / cookies")
		fmt.Println("3 - Show status")
		fmt.Println("4 - Remove local auth file")
		fmt.Println("5 - Exit")
		switch prompt(in, "Your choice (Enter = 5): ") {
		case "1":
			runLogin(ctx)
		case "2":
			importAuth(in)
		case "3":
			status()
			prompt(in, "\nPress Enter to return to the menu...")
		case "4":
			remove()
		default:
			return 0
		}
	}
	return 0
}

func runHealthcheck() int {
	port := envInt("PORT", 9655)
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
