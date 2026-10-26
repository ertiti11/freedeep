package authcli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ertiti11/freedeep/internal/cdp"
	"github.com/ertiti11/freedeep/internal/deepseek"
)

// LoginOptions configures RunLogin.
type LoginOptions struct {
	ChromePath  string
	Port        int
	ProfileDir  string
	OutPath     string
	ReuseChrome bool
	KeepProfile bool
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

// ChromeInstallHelp explains how to point the helper at a browser.
func ChromeInstallHelp(missing string) string {
	if missing != "" {
		missing = ": " + missing
	}
	return `Chrome/Chrome for Testing not found` + missing + `.

How to fix:
  Windows PowerShell:
    $env:CHROME_PATH="C:\Program Files\Google\Chrome\Application\chrome.exe"; freedeep login
    # or install Chrome normally: https://www.google.com/chrome/

  macOS:
    CHROME_PATH="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" freedeep login
    # or install Chrome for Testing / Google Chrome.

  Linux / Chromium:
    CHROME_PATH=$(which chromium) freedeep login
    # Ubuntu example: sudo apt install chromium-browser || sudo apt install chromium

If Chrome is installed elsewhere, set CHROME_PATH to the real executable path.`
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ResolveChromePath finds a Chromium-based browser executable.
func ResolveChromePath(override string) string {
	if override != "" {
		return override
	}
	var candidates []string
	home, _ := os.UserHomeDir()
	if home != "" {
		// Puppeteer's "Chrome for Testing" cache, newest version first.
		cacheRoot := filepath.Join(home, ".cache", "puppeteer", "chrome")
		if entries, err := os.ReadDir(cacheRoot); err == nil {
			for i := len(entries) - 1; i >= 0; i-- {
				base := filepath.Join(cacheRoot, entries[i].Name())
				switch runtime.GOOS {
				case "darwin":
					for _, arch := range []string{"chrome-mac-arm64", "chrome-mac-x64"} {
						candidates = append(candidates, filepath.Join(base, arch, "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"))
					}
				case "windows":
					candidates = append(candidates, filepath.Join(base, "chrome-win64", "chrome.exe"))
				default:
					candidates = append(candidates, filepath.Join(base, "chrome-linux64", "chrome"))
				}
			}
		}
	}
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
			if base := os.Getenv(env); base != "" {
				candidates = append(candidates, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
		candidates = append(candidates,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`)
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
			if base := os.Getenv(env); base != "" {
				candidates = append(candidates, filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"))
			}
		}
	case "darwin":
		candidates = append(candidates,
			"/Applications/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge")
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"} {
			if p, err := exec.LookPath(name); err == nil {
				candidates = append(candidates, p)
			}
		}
	}
	for _, c := range candidates {
		if exists(c) {
			return c
		}
	}
	return ""
}

func killExistingTestingChrome(port int, profileDir string) {
	if runtime.GOOS != "darwin" {
		return
	}
	for _, pattern := range []string{fmt.Sprintf("--remote-debugging-port=%d", port), profileDir} {
		safe := strings.NewReplacer(`\`, ".", `"`, ".", `'`, ".").Replace(pattern)
		_ = exec.Command("/usr/bin/pkill", "-f", safe).Run()
	}
	time.Sleep(800 * time.Millisecond)
}

func removeProfile(dir string, w io.Writer) {
	if _, err := os.Stat(dir); err != nil {
		return
	}
	for i := 0; i < 5; i++ {
		if err := os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	stale := fmt.Sprintf("%s.stale-%d", dir, time.Now().UnixMilli())
	if err := os.Rename(dir, stale); err == nil {
		_ = os.RemoveAll(stale)
		fmt.Fprintf(w, "[login] Old profile was busy; moved it aside: %s\n", stale)
	}
}

// pageAuthScript collects storage and resource URLs from the DeepSeek page.
const pageAuthScript = `(() => {
  const out = {href: location.href, localStorage:{}, sessionStorage:{}, resources: []};
  for (let i=0;i<localStorage.length;i++){ const k=localStorage.key(i); out.localStorage[k]=localStorage.getItem(k); }
  for (let i=0;i<sessionStorage.length;i++){ const k=sessionStorage.key(i); out.sessionStorage[k]=sessionStorage.getItem(k); }
  out.resources = performance.getEntriesByType('resource').map(r => r.name).filter(n => /wasm|chat\/completion|pow|chat_session/.test(n)).slice(-100);
  return out;
})()`

type pageState struct {
	Href           string            `json:"href"`
	LocalStorage   map[string]string `json:"localStorage"`
	SessionStorage map[string]string `json:"sessionStorage"`
	Resources      []string          `json:"resources"`
}

// NormalizeToken unwraps tokens stored as JSON ({"value": "..."}).
func NormalizeToken(raw string) string {
	if raw == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) == nil {
		for _, k := range []string{"value", "token", "access_token", "accessToken"} {
			if s, ok := obj[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	return strings.TrimSpace(raw)
}

var (
	reTokenKey  = regexp.MustCompile(`(?i)token`)
	reSha3Wasm  = regexp.MustCompile(`sha3.*\.wasm`)
	reBearerHdr = regexp.MustCompile(`(?i)^Bearer\s+`)
)

func tokenFromStores(stores ...map[string]string) string {
	for _, s := range stores {
		for _, k := range []string{"userToken", "token", "auth_token", "access_token", "accessToken"} {
			if t := NormalizeToken(s[k]); t != "" {
				return t
			}
		}
	}
	for _, s := range stores {
		for k, v := range s {
			if reTokenKey.MatchString(k) {
				if t := NormalizeToken(v); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

type capturedAuth struct {
	creds        deepseek.Credentials
	href         string
	cookiesCount int
}

func readPageAuth(ctx context.Context, conn *cdp.Conn) (capturedAuth, error) {
	var out capturedAuth
	var evalRes struct {
		Result struct {
			Value pageState `json:"value"`
		} `json:"result"`
	}
	if err := conn.Call(ctx, "Runtime.evaluate", map[string]any{"expression": pageAuthScript, "returnByValue": true}, &evalRes); err != nil {
		return out, err
	}
	ps := evalRes.Result.Value
	token := tokenFromStores(ps.LocalStorage, ps.SessionStorage)

	var cookieRes struct {
		Cookies []struct {
			Name, Value, Domain string
		} `json:"cookies"`
	}
	if err := conn.Call(ctx, "Network.getAllCookies", nil, &cookieRes); err != nil {
		return out, err
	}
	var parts []string
	for _, c := range cookieRes.Cookies {
		if strings.HasSuffix(c.Domain, "deepseek.com") {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}

	var dliq, leim string
	for _, ev := range conn.Events() {
		var p struct {
			Headers map[string]any `json:"headers"`
			Request struct {
				Headers map[string]any `json:"headers"`
			} `json:"request"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			continue
		}
		headers := p.Headers
		if headers == nil {
			headers = p.Request.Headers
		}
		for k, v := range headers {
			val := fmt.Sprint(v)
			switch strings.ToLower(k) {
			case "x-hif-dliq":
				dliq = val
			case "x-hif-leim":
				leim = val
			case "authorization":
				if token == "" && reBearerHdr.MatchString(val) {
					token = reBearerHdr.ReplaceAllString(val, "")
				}
			}
		}
	}
	wasm := deepseek.DefaultWasmURL
	for _, r := range ps.Resources {
		if reSha3Wasm.MatchString(r) {
			wasm = r
			break
		}
	}
	out.creds = deepseek.Credentials{Token: token, Cookie: strings.Join(parts, "; "), HifDliq: dliq, HifLeim: leim, WasmURL: wasm}
	out.href = ps.Href
	out.cookiesCount = len(parts)
	return out, nil
}

// RunLogin implements `freedeep login`: it opens a dedicated Chrome
// profile, waits for the user to log in, and saves the captured auth.
func RunLogin(ctx context.Context, o LoginOptions) int {
	chrome := ResolveChromePath(o.ChromePath)
	if !exists(chrome) {
		fmt.Fprintln(o.Stderr, ChromeInstallHelp(chrome))
		return ExitError
	}
	if !o.ReuseChrome {
		killExistingTestingChrome(o.Port, o.ProfileDir)
		if !o.KeepProfile {
			removeProfile(o.ProfileDir, o.Stdout)
		}
	}
	if err := os.MkdirAll(o.ProfileDir, 0o700); err != nil {
		fmt.Fprintln(o.Stderr, "[login] ERROR:", err)
		return ExitError
	}

	if o.ReuseChrome && cdp.DevtoolsReady(ctx, o.Port) {
		fmt.Fprintf(o.Stdout, "[login] Reusing Chrome DevTools on port %d\n", o.Port)
	} else {
		fmt.Fprintf(o.Stdout, "[login] Starting a clean Chrome profile: %s\n", o.ProfileDir)
		fmt.Fprintf(o.Stdout, "[login] Browser executable: %s\n", chrome)
		cmd := exec.Command(chrome,
			"--user-data-dir="+o.ProfileDir,
			fmt.Sprintf("--remote-debugging-port=%d", o.Port),
			"--use-mock-keychain",
			"--password-store=basic",
			"--disable-sync",
			"--disable-extensions",
			"--disable-component-extensions-with-background-pages",
			"--disable-features=AutofillServerCommunication,OptimizationHints,MediaRouter,InterestFeedContentSuggestions,Translate",
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-infobars",
			"https://chat.deepseek.com/",
		)
		detach(cmd)
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(o.Stderr, "[login] ERROR: could not start Chrome:", err)
			return ExitError
		}
		_ = cmd.Process.Release()
	}

	ready := false
	for i := 0; i < 80 && !ready; i++ {
		if ready = cdp.DevtoolsReady(ctx, o.Port); !ready {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if !ready {
		fmt.Fprintln(o.Stderr, "[login] ERROR: Chrome DevTools endpoint did not start")
		return ExitError
	}
	var target *cdp.Target
	var err error
	for i := 0; i < 40; i++ {
		if target, err = cdp.PageTarget(ctx, o.Port, "chat.deepseek.com"); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if target == nil {
		fmt.Fprintln(o.Stderr, "[login] ERROR:", err)
		return ExitError
	}
	conn, err := cdp.Dial(ctx, target.WebSocketDebuggerURL)
	if err != nil {
		fmt.Fprintln(o.Stderr, "[login] ERROR: could not attach to Chrome:", err)
		return ExitError
	}
	defer conn.Close()
	for _, m := range []string{"Runtime.enable", "Network.enable"} {
		if err := conn.Call(ctx, m, nil, nil); err != nil {
			fmt.Fprintln(o.Stderr, "[login] ERROR:", err)
			return ExitError
		}
	}

	fmt.Fprintln(o.Stdout, "\n[login] Chrome is open. Log in to DeepSeek in THIS separate window.")
	fmt.Fprintln(o.Stdout, "[login] After logging in, send DeepSeek a short message, for example: ok")
	fmt.Fprint(o.Stdout, "[login] Once logged in and the test message is sent, press ENTER here: ")
	_, _ = bufio.NewReader(o.Stdin).ReadString('\n')

	var auth capturedAuth
	for i := 0; i < 20; i++ {
		if auth, err = readPageAuth(ctx, conn); err != nil {
			fmt.Fprintln(o.Stderr, "[login] ERROR:", err)
			return ExitError
		}
		if auth.creds.Ready() {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := SecureWriteJSON(o.OutPath, auth.creds); err != nil {
		fmt.Fprintln(o.Stderr, "[login] ERROR:", err)
		return ExitError
	}
	status := func(ok bool, okText string) string {
		if ok {
			return okText
		}
		return "MISSING"
	}
	fmt.Fprintf(o.Stdout, "[login] Saved: %s\n", o.OutPath)
	fmt.Fprintf(o.Stdout, "[login] page: %s\n", auth.href)
	fmt.Fprintf(o.Stdout, "[login] token: %s\n", status(auth.creds.Token != "", fmt.Sprintf("OK (%d chars)", len(auth.creds.Token))))
	fmt.Fprintf(o.Stdout, "[login] cookie: %s\n", status(auth.creds.Cookie != "", fmt.Sprintf("OK (%d cookies)", auth.cookiesCount)))
	hif := "not captured/optional"
	if auth.creds.HifDliq != "" || auth.creds.HifLeim != "" {
		hif = "captured"
	}
	fmt.Fprintf(o.Stdout, "[login] hif headers: %s\n", hif)
	if !auth.creds.Ready() {
		return ExitInvalid
	}
	return ExitOK
}
