package authcli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Exit codes shared by the auth commands.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitInvalid = 2
)

// ImportOptions configures RunImport.
type ImportOptions struct {
	Args          []string
	DefaultOutput string
	EnvToken      string
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
}

// RunImport implements `freedeep import`.
func RunImport(o ImportOptions) int {
	// Refuse --token before anything else: CLI arguments leak into shell
	// history and process listings.
	for _, a := range o.Args {
		if a == "--token" || a == "-token" || strings.HasPrefix(a, "--token=") || strings.HasPrefix(a, "-token=") {
			fmt.Fprintln(o.Stderr, "[import] Refusing --token for safety: CLI args leak into shell history/process lists. Use DEEPSEEK_TOKEN=... instead.")
			return ExitInvalid
		}
	}
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(o.Stderr)
	var input, output string
	fs.StringVar(&input, "input", "", "source JSON: deepseek-auth.json, extension export, or browser cookie export")
	fs.StringVar(&input, "i", "", "shorthand for --input")
	fs.StringVar(&output, "output", o.DefaultOutput, "target auth file")
	fs.StringVar(&output, "o", o.DefaultOutput, "shorthand for --output")
	fs.Usage = func() { printImportHelp(o.Stdout, o.DefaultOutput) }
	if err := fs.Parse(o.Args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitInvalid
	}
	if input == "" {
		fmt.Fprint(o.Stdout, "Path to deepseek-auth.json / browser cookies JSON: ")
		line, _ := bufio.NewReader(o.Stdin).ReadString('\n')
		input = strings.TrimSpace(line)
		if input == "" {
			fmt.Fprintln(o.Stderr, "[import] No input file given.")
			return ExitInvalid
		}
	}
	raw, err := os.ReadFile(input)
	if err != nil {
		fmt.Fprintln(o.Stderr, "[import] ERROR:", err)
		return ExitError
	}
	var source any
	if err := json.Unmarshal(raw, &source); err != nil {
		fmt.Fprintln(o.Stderr, "[import] ERROR: invalid JSON:", err)
		return ExitError
	}
	auth := NormalizeAuth(source, o.EnvToken)
	if errs := Validate(auth); len(errs) > 0 {
		fmt.Fprintf(o.Stderr, "[import] Invalid auth import: %s\n", strings.Join(errs, ", "))
		fmt.Fprintln(o.Stderr, "[import] When importing browser cookies, pass the token via DEEPSEEK_TOKEN=...")
		return ExitInvalid
	}
	out, _ := filepath.Abs(output)
	if err := SecureWriteJSON(out, auth); err != nil {
		fmt.Fprintln(o.Stderr, "[import] ERROR:", err)
		return ExitError
	}
	fmt.Fprintf(o.Stdout, "[import] Imported auth to %s\n", out)
	fmt.Fprintf(o.Stdout, "[import] token: OK (%d chars)\n", len(auth.Token))
	n := 0
	for _, p := range strings.Split(auth.Cookie, ";") {
		if strings.TrimSpace(p) != "" {
			n++
		}
	}
	fmt.Fprintf(o.Stdout, "[import] cookie: OK (%d cookies)\n", n)
	if runtime.GOOS != "windows" {
		fmt.Fprintln(o.Stdout, "[import] permissions: 0600")
	}
	return ExitOK
}

func printImportHelp(w io.Writer, defaultOutput string) {
	fmt.Fprintf(w, `freedeep auth import

Usage:
  freedeep import --input ./deepseek-auth.json
  DEEPSEEK_TOKEN="<DeepSeek token>" freedeep import --input ./cookies.json

Options:
  --input, -i   Source JSON: a ready deepseek-auth.json, the Chrome extension
                export, or a browser cookie export
  --output, -o  Target auth path (default: %s)

Security:
  For cookies.json pass the token via DEEPSEEK_TOKEN, not as a CLI argument,
  so it does not leak into shell history or the process list.

VPS flow:
  1) On your desktop: freedeep login
  2) Copy deepseek-auth.json to the VPS
  3) On the VPS: freedeep import --input ./deepseek-auth.json
  4) Start: NON_INTERACTIVE=1 freedeep serve
`, defaultOutput)
}
