//go:build !windows

package accmgr

// Stubs for non-Windows builds: no OS-level mouse control.
func osScreenSize() (int, int)     { return 0, 0 }
func osMouseDragTo(x, y, dist int) {}
func osMouseMoveTo(x, y int)       {}
func osMouseAvailable() bool       { return false }
func osFocusWindowByTitle(sub string) bool { return false }
