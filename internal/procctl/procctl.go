//go:build windows

// Package procctl handles the detached-process lifecycle: where the running
// instance records itself, how a second invocation finds it, and how to
// relaunch without a console.
package procctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

// State is what a running instance publishes for later commands to find.
type State struct {
	PID         int       `json:"pid"`
	URL         string    `json:"url"`
	Local       string    `json:"local_url"`
	Started     time.Time `json:"started"`
	Fingerprint string    `json:"cert_fingerprint,omitempty"`
	// Args are the serve flags this instance was started with, so an update
	// can restart it the same way.
	Args []string `json:"args,omitempty"`
}

// ErrNotRunning means no live instance was found.
var ErrNotRunning = errors.New("procctl: no running instance")

// Dir returns the per-user state directory, creating it on demand.
func Dir() (string, error) {
	base, err := os.UserCacheDir() // %LOCALAPPDATA% on Windows
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "ooi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("procctl: create state dir: %w", err)
	}
	return dir, nil
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

// Save writes the state file.
func Save(dir string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("procctl: marshal state: %w", err)
	}
	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("procctl: write state: %w", err)
	}
	if err := os.Rename(tmp, statePath(dir)); err != nil {
		return fmt.Errorf("procctl: commit state: %w", err)
	}
	return nil
}

// Clear removes the state file.
func Clear(dir string) { os.Remove(statePath(dir)) }

// Load reads the state file and verifies the process is still alive.
// A stale file is removed and ErrNotRunning is returned.
func Load(dir string) (State, error) {
	b, err := os.ReadFile(statePath(dir))
	if err != nil {
		return State{}, ErrNotRunning
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		os.Remove(statePath(dir))
		return State{}, ErrNotRunning
	}
	if !Alive(s.PID) {
		os.Remove(statePath(dir))
		return State{}, ErrNotRunning
	}
	return s, nil
}

// Windows process constants. The CreateProcess flags are not all exported by
// the syscall package, so they are declared here.
const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
	processTerminate               = 0x0001

	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess     = kernel32.NewProc("OpenProcess")
	procGetExitCodeProc = kernel32.NewProc("GetExitCodeProcess")
	procTerminateProc   = kernel32.NewProc("TerminateProcess")
	procCloseHandle     = kernel32.NewProc("CloseHandle")
)

// Alive reports whether a PID refers to a running process.
//
// os.FindProcess always succeeds on Windows, so this asks the OS directly.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer procCloseHandle.Call(h)

	var code uint32
	r, _, _ := procGetExitCodeProc.Call(h, uintptr(unsafe.Pointer(&code)))
	if r == 0 {
		return false
	}
	return code == stillActive
}

// Kill force-terminates a PID. It is the fallback when the graceful HTTP
// shutdown cannot be reached.
func Kill(pid int) error {
	h, _, err := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
	if h == 0 {
		return fmt.Errorf("procctl: open process %d: %w", pid, err)
	}
	defer procCloseHandle.Call(h)
	r, _, err := procTerminateProc.Call(h, 1)
	if r == 0 {
		return fmt.Errorf("procctl: terminate %d: %w", pid, err)
	}
	return nil
}

// Detach relaunches this executable with the given arguments as a console-less
// background process and returns its PID.
//
// DETACHED_PROCESS drops the console entirely (rather than CREATE_NO_WINDOW,
// which merely hides it), so the child survives the parent shell closing.
func Detach(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("procctl: locate executable: %w", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
		HideWindow:    true,
	}
	// A detached child has no console; give it null handles rather than
	// inheriting ours, which would keep the parent's pipes open.
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
		defer null.Close()
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("procctl: start detached: %w", err)
	}
	pid := cmd.Process.Pid
	// Release so the parent can exit without leaving a zombie handle.
	cmd.Process.Release()
	return pid, nil
}
