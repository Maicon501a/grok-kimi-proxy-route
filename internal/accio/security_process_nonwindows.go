//go:build !windows

package accio

import "os/exec"

func hideSecuritySidecarWindow(_ *exec.Cmd) {}
