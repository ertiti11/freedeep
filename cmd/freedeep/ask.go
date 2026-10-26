package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ertiti11/freedeep/internal/authcli"
	"github.com/ertiti11/freedeep/internal/config"
	"github.com/ertiti11/freedeep/internal/deepseek"
)

// askCredentials reads DEEPSEEK_* variables, falling back to the auth file
// (or the legacy auth.json).
func askCredentials() deepseek.Credentials {
	c := deepseek.Credentials{
		Token:   os.Getenv("DEEPSEEK_TOKEN"),
		HifDliq: os.Getenv("DEEPSEEK_HIF_DLIQ"),
		HifLeim: os.Getenv("DEEPSEEK_HIF_LEIM"),
		Cookie:  os.Getenv("DEEPSEEK_COOKIE"),
		WasmURL: os.Getenv("DEEPSEEK_WASM_URL"),
	}
	if c.Token == "" {
		for _, file := range []string{config.SingleAuthPath(config.OSEnv), "auth.json"} {
			if f, err := authcli.ReadCredentials(file); err == nil && f.Token != "" {
				c = f
				break
			}
		}
	}
	return c
}

func runAsk(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	model := fs.String("model", deepseek.DefaultModel, "model alias (see /v1/models)")
	save := fs.Bool("save", true, "save the answer to a temp file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" && !isTTY() {
		b, _ := io.ReadAll(bufio.NewReader(os.Stdin))
		text = strings.TrimSpace(string(b))
	}
	if text == "" {
		fmt.Fprintln(os.Stderr, `Usage: freedeep ask [--model deepseek-reasoner] "your prompt here"`)
		return 1
	}
	creds := askCredentials()
	if creds.Token == "" {
		fmt.Fprintln(os.Stderr, "Error: DeepSeek auth is not set. Run `freedeep login`, or provide DEEPSEEK_TOKEN/DEEPSEEK_COOKIE via env.")
		return 1
	}
	m, ok := deepseek.LookupModel(*model)
	if !ok || !m.Supported {
		fmt.Fprintf(os.Stderr, "Error: unknown or unsupported model %q. Supported: %s\n", *model, strings.Join(deepseek.SupportedModelIDs(), ", "))
		return 1
	}
	cfg, _ := config.Load(config.OSEnv)
	client := newClient(cfg)

	powHeader, err := client.SolvePoW(ctx, creds)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[!] Error:", err)
		return 1
	}
	sessionID, err := client.CreateSession(ctx, creds)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[!] Error:", err)
		return 1
	}
	body, err := client.Completion(ctx, creds, powHeader, deepseek.CompletionRequest{
		ChatSessionID: sessionID, ModelType: m.ModelType, Prompt: text,
		ThinkingEnabled: m.ThinkingEnabled, SearchEnabled: m.SearchEnabled,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[!] Error:", err)
		return 1
	}
	defer body.Close()

	var printedContent, printedReasoning int
	res, err := deepseek.ReadStream(body, func(content, reasoning string) {
		// Reasoning goes to stderr so stdout stays a clean answer.
		if len(reasoning) > printedReasoning {
			fmt.Fprint(os.Stderr, reasoning[printedReasoning:])
			printedReasoning = len(reasoning)
		}
		if len(content) > printedContent {
			fmt.Print(content[printedContent:])
			printedContent = len(content)
		}
	})
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[!] Error:", err)
		return 1
	}
	if res.ModelError != nil && strings.TrimSpace(res.Content) == "" {
		fmt.Fprintln(os.Stderr, "[!] DeepSeek error:", res.ModelError.Content)
		return 1
	}
	if *save {
		out := filepath.Join(os.TempDir(), fmt.Sprintf("deepseek_response_%d.txt", time.Now().UnixMilli()))
		if err := os.WriteFile(out, []byte(strings.TrimSpace(res.Content)), 0o600); err == nil {
			fmt.Fprintf(os.Stderr, "\n[*] Saved %s\n", out)
		}
	}
	return 0
}

func runProbe(ctx context.Context) int {
	creds := askCredentials()
	if creds.Token == "" {
		fmt.Fprintln(os.Stderr, "Error: DeepSeek auth is not set. Run `freedeep login` first.")
		return 1
	}
	cfg, _ := config.Load(config.OSEnv)
	client := newClient(cfg)
	combos := []struct {
		modelType        string
		thinking, search bool
	}{
		{"default", false, false}, {"default", true, false}, {"default", false, true}, {"default", true, true},
		{"expert", false, false}, {"expert", true, false}, {"expert", false, true}, {"vision", false, false},
	}
	for _, c := range combos {
		label := fmt.Sprintf("%s thinking=%v search=%v", c.modelType, c.thinking, c.search)
		sessionID, err := client.CreateSession(ctx, creds)
		if err != nil {
			fmt.Printf("\n## %s ERROR %v\n", label, err)
			continue
		}
		powHeader, err := client.SolvePoW(ctx, creds)
		if err != nil {
			fmt.Printf("\n## %s ERROR %v\n", label, err)
			continue
		}
		body, err := client.Completion(ctx, creds, powHeader, deepseek.CompletionRequest{
			ChatSessionID: sessionID, ModelType: c.modelType, Prompt: "Reply with exactly: OK",
			ThinkingEnabled: c.thinking, SearchEnabled: c.search,
		})
		if err != nil {
			fmt.Printf("\n## %s ERROR %v\n", label, err)
			continue
		}
		res, err := deepseek.ReadStream(body, nil)
		body.Close()
		status := "OK"
		switch {
		case err != nil:
			status = "STREAM ERROR " + err.Error()
		case res.ModelError != nil:
			status = "MODEL ERROR " + res.ModelError.Content
		case strings.TrimSpace(res.Content) == "":
			status = "EMPTY"
		}
		fmt.Printf("\n## %s: %s\n  content=%q reasoning_chars=%d finish=%q\n", label, status, truncate(res.Content, 200), len(res.Reasoning), res.FinishReason)
		if ctx.Err() != nil {
			return 1
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
