//go:build !windows

package register

import (
	"os/exec"
	"syscall"
)

func hideConsoleWindow(cmd *exec.Cmd) {}

func allowGUIChildren(cmd *exec.Cmd) {}

func prepareProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// New process group so we can kill the whole tree (Chrome children) on cancel.
	cmd.SysProcAttr.Setpgid = true
}

func setupProcessJob(cmd *exec.Cmd) error { return nil }

func releaseProcessJob(cmd *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative PID = process group
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

func killHint() string {
	return " (process group killed)"
}
