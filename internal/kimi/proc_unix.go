//go:build !windows

package kimi

import "os/exec"

func hideConsoleWindow(cmd *exec.Cmd)      {}
func fullyHideConsoleWindow(cmd *exec.Cmd) {}

// Unix has no Windows Job Object. The regular process handle is enough for
// the fallback cleanup path used by the browser login flow.
func SetupProcessJob(_ *exec.Cmd) error { return nil }

func KillProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
