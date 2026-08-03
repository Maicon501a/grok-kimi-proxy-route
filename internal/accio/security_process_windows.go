//go:build windows

package accio

import (
	"os/exec"
	"syscall"
)

func hideSecuritySidecarWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
