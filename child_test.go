// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitFor polls until want is true or the deadline passes, so a test states
// the condition it is waiting for rather than a duration it hopes is enough.
func waitUntil(t *testing.T, why string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

func snapshotOf(m *Manager, name string) ChildSnapshot {
	for _, c := range m.Snapshot().Children {
		if c.Name == name {
			return c
		}
	}
	return ChildSnapshot{}
}

// A freshly started child has not restarted. The number is one an operator
// judges stability by, and "1 restart" on a healthy boot reads as a crash that
// never happened — which is exactly what this reported before it was fixed.
func TestAFirstStartIsNotARestart(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{Name: "sleeper", Command: []string{"sleep", "30"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to start", func() bool { return snapshotOf(m, "sleeper").PID != 0 })

	if got := snapshotOf(m, "sleeper").Restarts; got != 0 {
		t.Errorf("a first start reported %d restarts, want 0", got)
	}
}

// The restart loop is the half of this package a unit test can actually reach,
// and the half most likely to be wrong.
func TestAKilledChildComesBackWithADifferentPID(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{Name: "sleeper", Command: []string{"sleep", "30"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to start", func() bool { return snapshotOf(m, "sleeper").PID != 0 })
	first := snapshotOf(m, "sleeper").PID

	process, err := os.FindProcess(first)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	waitUntil(t, "the child to be restarted", func() bool {
		s := snapshotOf(m, "sleeper")
		return s.PID != 0 && s.PID != first
	})

	if got := snapshotOf(m, "sleeper").Restarts; got != 1 {
		t.Errorf("after one kill the child reported %d restarts, want 1", got)
	}
}

// The boot id is what stitches the Supervisor's records to its children's
// (ADR 0060), so it has to reach the child's environment rather than only the
// Supervisor's own logs.
func TestTheBootIDReachesTheChildsEnvironment(t *testing.T) {
	out := t.TempDir() + "/env"
	m := NewManager("boot-for-the-child", nil)
	if err := m.Add(ChildSpec{
		Name:    "recorder",
		Command: []string{"sh", "-c", "printf %s \"$MOSAIC_BOOT_ID\" > " + out + "; sleep 30"},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to record its environment", func() bool {
		data, err := os.ReadFile(out)
		return err == nil && len(data) > 0
	})

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "boot-for-the-child" {
		t.Errorf("child saw MOSAIC_BOOT_ID=%q, want the Supervisor's own", data)
	}
}

// A child with no command is externally managed — the dev stack's shape, where
// compose owns the processes. The Supervisor must report on it without trying
// to own its lifecycle.
func TestAnExternallyManagedChildIsReportedNotStarted(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{Name: "platform"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to be reported", func() bool {
		return snapshotOf(m, "platform").State == ChildReady
	})

	s := snapshotOf(m, "platform")
	if s.PID != 0 {
		t.Errorf("an externally managed child reported pid %d — the Supervisor started something it does not own", s.PID)
	}
	if s.Restarts != 0 {
		t.Errorf("an externally managed child reported %d restarts", s.Restarts)
	}
}

// Children stop in registration order — most expendable first, the interface
// last — so ADR 0005's ladder is walked down rather than skipped. Stopping the
// Shell first would drain nothing (clients reach the Platform through the front
// door, not through the Shell) and would replace its offline screen with the
// holding page while the better one was still available.
func TestChildrenStopInRegistrationOrder(t *testing.T) {
	var mu sync.Mutex
	var stopped []string

	m := NewManager("boot-1", func(format string, args ...any) {
		// The Manager announces each stop as it begins it, which is the only
		// ordering signal available from outside.
		if !strings.HasPrefix(format, "stopping child ") {
			return
		}
		mu.Lock()
		stopped = append(stopped, args[0].(string))
		mu.Unlock()
	})

	for _, name := range []string{"platform", "shell"} {
		if err := m.Add(ChildSpec{
			Name:      name,
			Command:   []string{"sleep", "30"},
			StopGrace: time.Second,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()

	waitUntil(t, "both children to start", func() bool {
		return snapshotOf(m, "platform").PID != 0 && snapshotOf(m, "shell").PID != 0
	})

	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stopped) != 2 || stopped[0] != "platform" || stopped[1] != "shell" {
		t.Errorf("stopped in order %v, want [platform shell] — the interface goes last", stopped)
	}
}

// The ordering above only means something if the front door is still serving
// while it happens. This pins the rung it exists to preserve: with the
// Platform gone and the Shell still up, the front door must answer a
// navigation from the Shell rather than falling through to the holding page.
func TestTheShellStillAnswersWhileThePlatformIsGone(t *testing.T) {
	_, shell := upstreams(t)
	// A Platform that has already stopped, and a Shell that has not.
	fd := frontDoor(t, "http://127.0.0.1:1", shell.URL, nil)

	resp := route(t, fd, "/library")
	if got := resp.Header.Get("X-Upstream"); got != "shell" {
		t.Fatalf("a navigation went to %q, want the Shell", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 from the Shell, got %d", resp.StatusCode)
	}

	// And the Platform call it then makes gets the interpretable error the
	// Shell's offline screen renders — not the holding page.
	api := route(t, fd, "/mosaic.auth.v1.AuthService/Bootstrap")
	if api.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("want 503 for the Platform call, got %d", api.StatusCode)
	}
	if ct := api.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("want a machine-readable error the client can render, got %q", ct)
	}
}

// Each child's stop is its own rather than a share of one global deadline,
// and a child that exits promptly must not wait out its grace.
func TestAStoppedChildIsWaitedForRatherThanTimedOut(t *testing.T) {
	m := NewManager("boot-1", nil)
	for _, name := range []string{"platform", "shell"} {
		if err := m.Add(ChildSpec{
			Name:      name,
			Command:   []string{"sleep", "30"},
			StopGrace: 10 * time.Second,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()

	waitUntil(t, "both children to start", func() bool {
		return snapshotOf(m, "platform").PID != 0 && snapshotOf(m, "shell").PID != 0
	})

	// `sleep` dies on SIGTERM immediately, so a correct implementation returns
	// in well under the two children's combined 20s of grace. A version that
	// waited out the grace regardless would blow this deadline.
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return; the grace is being waited out rather than the process")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("shutdown took %s — the stop is not observing the process exiting", elapsed)
	}
}

// A child that ignores SIGTERM has to be killed, and the grace is what bounds
// how long one can hold up a shutdown.
func TestAChildIgnoringSIGTERMIsKilledAfterItsGrace(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name: "stubborn",
		// trap '' TERM ignores the signal entirely.
		Command:   []string{"sh", "-c", "trap '' TERM; sleep 30"},
		StopGrace: 2 * time.Second,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()

	waitUntil(t, "the child to start", func() bool { return snapshotOf(m, "stubborn").PID != 0 })

	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a child ignoring SIGTERM was never killed")
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Errorf("killed after %s, before its 2s grace elapsed", elapsed)
	}
}

// The working directory is not cosmetic: the Platform resolves its extension
// install directory relative to it (ADR 0081), so a child started from the
// wrong place finds none of the modules a user installed.
func TestAChildRunsInItsConfiguredWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	out := t.TempDir() + "/cwd"

	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:       "recorder",
		Command:    []string{"sh", "-c", "pwd > " + out + "; sleep 30"},
		WorkingDir: dir,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to record its directory", func() bool {
		data, err := os.ReadFile(out)
		return err == nil && len(data) > 0
	})

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, want := strings.TrimSpace(string(data)), dir
	// TempDir can sit behind a symlink; compare resolved forms.
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	if got != want {
		t.Errorf("child ran in %q, want %q", got, want)
	}
}

func TestAChildNeedsANameAndCannotBeRegisteredTwice(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{}); err == nil {
		t.Error("a nameless child was accepted")
	}
	if err := m.Add(ChildSpec{Name: "shell"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(ChildSpec{Name: "shell"}); err == nil {
		t.Error("a duplicate name was accepted; two children under one name would report as one")
	}
}
