//go:build !windows

package warp

import "os/exec"

func hideCommandWindow(_ *exec.Cmd) {}
