//go:build windows

package register

import (
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

var (
	registerKernel32                 = syscall.NewLazyDLL("kernel32.dll")
	registerCreateJobObject          = registerKernel32.NewProc("CreateJobObjectW")
	registerSetInformationJobObject  = registerKernel32.NewProc("SetInformationJobObject")
	registerAssignProcessToJobObject = registerKernel32.NewProc("AssignProcessToJobObject")
	registerTerminateJobObject       = registerKernel32.NewProc("TerminateJobObject")
	registerCloseHandle              = registerKernel32.NewProc("CloseHandle")
	registerOpenProcess              = registerKernel32.NewProc("OpenProcess")
	registerJobsMu                   sync.Mutex
	registerJobs                     = make(map[int]syscall.Handle)
)

const (
	registerJobExtendedLimitInformation = 9
	registerJobLimitKillOnClose         = 0x2000
)

type registerJobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type registerIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type registerJobExtendedLimit struct {
	BasicLimitInformation registerJobBasicLimit
	IoInfo                registerIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// hideConsoleWindow prevents a black console flash for short-lived python/pip/taskkill.
// Do NOT use this for the signup bot when Chrome must be visible — CREATE_NO_WINDOW /
// HideWindow can prevent GUI child processes (Chrome) from showing a window.
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// CREATE_NO_WINDOW
	cmd.SysProcAttr.CreationFlags |= 0x08000000
	cmd.SysProcAttr.HideWindow = true
}

// allowGUIChildren leaves console inheritance alone so Chromium can open a normal window.
// Prefer this for the long-running signup bot (headless=false).
func allowGUIChildren(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// No CREATE_NO_WINDOW / HideWindow — Chrome needs a visible desktop session.
}

// setupProcessJob assigns the Python worker to a Windows Job Object configured
// with KILL_ON_JOB_CLOSE. Chrome children remain in the job even if they detach
// or get re-parented, so closing the job cannot leave browser ghosts behind.
func setupProcessJob(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("process not started")
	}
	hJob, _, createErr := registerCreateJobObject.Call(0, 0, 0)
	if hJob == 0 {
		return fmt.Errorf("CreateJobObject failed: %v", createErr)
	}
	info := registerJobExtendedLimit{
		BasicLimitInformation: registerJobBasicLimit{LimitFlags: registerJobLimitKillOnClose},
	}
	ret, _, setErr := registerSetInformationJobObject.Call(
		hJob,
		uintptr(registerJobExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	if ret == 0 {
		registerCloseHandle.Call(hJob)
		return fmt.Errorf("SetInformationJobObject failed: %v", setErr)
	}
	hProcess, _, openErr := registerOpenProcess.Call(
		uintptr(syscall.PROCESS_TERMINATE|0x0100|0x0040), 0, uintptr(cmd.Process.Pid),
	)
	if hProcess == 0 {
		registerCloseHandle.Call(hJob)
		return fmt.Errorf("OpenProcess failed: %v", openErr)
	}
	ret, _, assignErr := registerAssignProcessToJobObject.Call(hJob, hProcess)
	registerCloseHandle.Call(hProcess)
	if ret == 0 {
		registerCloseHandle.Call(hJob)
		return fmt.Errorf("AssignProcessToJobObject failed: %v", assignErr)
	}
	registerJobsMu.Lock()
	registerJobs[cmd.Process.Pid] = syscall.Handle(hJob)
	registerJobsMu.Unlock()
	return nil
}

// releaseProcessJob closes the KILL_ON_JOB_CLOSE handle after Python exits,
// terminating any Chrome descendants that survived their normal finally block.
func releaseProcessJob(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	registerJobsMu.Lock()
	hJob := registerJobs[cmd.Process.Pid]
	delete(registerJobs, cmd.Process.Pid)
	registerJobsMu.Unlock()
	if hJob != 0 {
		registerCloseHandle.Call(uintptr(hJob))
	}
}

// killProcessTree terminates cmd and all descendants (Chrome launched by DrissionPage).
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	registerJobsMu.Lock()
	hJob := registerJobs[pid]
	delete(registerJobs, pid)
	registerJobsMu.Unlock()
	if hJob != 0 {
		registerTerminateJobObject.Call(uintptr(hJob), 1)
		registerCloseHandle.Call(uintptr(hJob))
		_ = cmd.Process.Kill()
		return
	}
	// /T = tree, /F = force
	c := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	hideConsoleWindow(c)
	_ = c.Run()
	_ = cmd.Process.Kill()
}

// killHint is appended to cancel/timeout messages on this OS.
func killHint() string {
	return fmt.Sprintf(" (process tree killed)")
}
