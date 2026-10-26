//go:build !windows

package authcli

import (
	"os/exec"
	"syscall"
)

// detach starts the browser in its own process group so it outlives us.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
