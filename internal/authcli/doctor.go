package authcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"time"

	"github.com/ertiti11/freedeep/internal/deepseek"
	"github.com/ertiti11/freedeep/internal/pow"
)

// FileCheck is the static check result for one auth file.
type FileCheck struct {
	File   string
	OK     bool
	Issues []string
	Creds  *deepseek.Credentials
}

// CheckAuthFile validates an auth file without network access.
func CheckAuthFile(file string) FileCheck {
	r := FileCheck{File: file}
	st, err := os.Stat(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.Issues = []string{"auth file missing"}
		} else {
			r.Issues = []string{err.Error()}
		}
		return r
	}
	c, err := ReadCredentials(file)
	if err != nil {
		r.Issues = []string{err.Error()}
		return r
	}
	r.Creds = &c
	r.Issues = Validate(c)
	if runtime.GOOS != "windows" {
		if mode := st.Mode().Perm(); mode&0o077 != 0 {
			r.Issues = append(r.Issues, fmt.Sprintf("permissions too open: %o (run: chmod 600 %s)", mode, file))
		}
	}
	r.OK = len(r.Issues) == 0
	return r
}

// DoctorOptions configures RunDoctor.
type DoctorOptions struct {
	Paths      []string
	SourceDesc string
	Offline    bool
	Client     *deepseek.Client
	Stdout     io.Writer
}

// RunDoctor implements `freedeep doctor`.
func RunDoctor(ctx context.Context, o DoctorOptions) int {
	w := o.Stdout
	fmt.Fprintln(w, "freedeep doctor")
	fmt.Fprintf(w, "Auth source: %s\n", o.SourceDesc)
	ok := true
	if len(o.Paths) == 0 {
		ok = false
		fmt.Fprintln(w, "  ❌ no auth files found")
	}
	for _, file := range o.Paths {
		r := CheckAuthFile(file)
		fmt.Fprintf(w, "\nAuth file: %s\n", file)
		if r.OK {
			fmt.Fprintln(w, "  ✅ auth file looks OK")
		} else {
			ok = false
			for _, issue := range r.Issues {
				fmt.Fprintf(w, "  ❌ %s\n", issue)
			}
		}
		if o.Offline || !r.OK || r.Creds == nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		ch, err := o.Client.CreatePoW(cctx, *r.Creds)
		cancel()
		if err != nil {
			ok = false
			fmt.Fprintf(w, "  ❌ live PoW challenge: %v\n", err)
			continue
		}
		fmt.Fprintln(w, "  ✅ live PoW challenge: OK")
		start := time.Now()
		if _, err := pow.Solve(ch); err != nil {
			ok = false
			fmt.Fprintf(w, "  ❌ PoW solve (%s, difficulty %d): %v\n", ch.Algorithm, ch.Difficulty, err)
		} else {
			fmt.Fprintf(w, "  ✅ PoW solve: %s difficulty %d in %s\n", ch.Algorithm, ch.Difficulty, time.Since(start).Round(time.Millisecond))
		}
	}
	fmt.Fprintln(w, "\nSession reuse: one x-agent-session/user => one DeepSeek chat until TTL/message limit/error reset.")
	fmt.Fprintln(w, `Reset: curl -X POST "http://localhost:9655/reset-session?agent=all"`)
	fmt.Fprintln(w, "VPS: import auth on the server, then run NON_INTERACTIVE=1 freedeep serve")
	if !ok {
		return ExitInvalid
	}
	return ExitOK
}
