//go:build windows

package procctl

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDirIsCreated(t *testing.T) {
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("state dir does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", dir)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := State{
		PID:         os.Getpid(), // alive, so Load accepts it
		URL:         "https://172.60.3.42:8787/",
		Local:       "https://127.0.0.1:8787/",
		Started:     time.Now().Truncate(time.Second),
		Fingerprint: "AA:BB:CC",
	}
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PID != want.PID || got.URL != want.URL || got.Local != want.Local ||
		got.Fingerprint != want.Fingerprint {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if !got.Started.Equal(want.Started) {
		t.Errorf("Started = %v, want %v", got.Started, want.Started)
	}
}

func TestLoadWithoutStateFile(t *testing.T) {
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Load on an empty dir = %v, want ErrNotRunning", err)
	}
}

// TestLoadRemovesStaleState is what stops a crashed run from blocking the
// next `serve --detach`.
func TestLoadRemovesStaleState(t *testing.T) {
	dir := t.TempDir()

	// Start and reap a real process so its PID is genuinely dead.
	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadPID := cmd.Process.Pid
	cmd.Wait()

	if err := Save(dir, State{PID: deadPID, URL: "https://x/"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Load with a dead PID = %v, want ErrNotRunning", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Error("stale state file was not removed")
	}
}

func TestLoadRemovesCorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Load on corrupt state = %v, want ErrNotRunning", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Error("corrupt state file was not removed")
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, State{PID: os.Getpid()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	Clear(dir)
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Error("Clear did not remove the state file")
	}
	Clear(dir) // must not panic when already gone
}

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(self) = false")
	}
	for _, pid := range []int{0, -1, -999} {
		if Alive(pid) {
			t.Errorf("Alive(%d) = true, want false", pid)
		}
	}

	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	cmd.Wait()
	if Alive(pid) {
		t.Errorf("Alive(%d) = true for an exited process", pid)
	}
}

// TestKill starts a long-lived process and terminates it.
func TestKill(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping", "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	defer cmd.Wait()

	if !Alive(pid) {
		t.Fatalf("process %d is not alive right after Start", pid)
	}
	if err := Kill(pid); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if Alive(pid) {
		t.Errorf("process %d survived Kill", pid)
	}
}

func TestKillNonexistent(t *testing.T) {
	if err := Kill(0); err == nil {
		t.Error("Kill(0) = nil, want an error")
	}
}

// TestSaveIsAtomic checks that Save leaves no temporary file behind, so a
// concurrent Load never sees a half-written file.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, State{PID: os.Getpid()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("Save left a temporary file behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("state dir has %d entries, want 1", len(entries))
	}
}
