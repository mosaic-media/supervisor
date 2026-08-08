// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"os"
	"testing"
	"time"
)

func running(t *testing.T, spec ChildSpec) (*Manager, context.CancelFunc) {
	t.Helper()
	m := NewManager("boot-1", nil)
	if err := m.Add(spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	waitUntil(t, "the child to start", func() bool { return snapshotOf(m, spec.Name).PID != 0 })
	return m, cancel
}

// A restart the Supervisor asked for replaces the process and is not a
// failure. Counting it would have a Generation activation or a Restart-class
// configuration change look like a crashing Platform, and eventually be
// reported as one that will not come up.
func TestADeliberateRestartIsNotAFailure(t *testing.T) {
	m, cancel := running(t, ChildSpec{Name: "platform", Command: []string{"sleep", "30"}, StopGrace: time.Second})
	defer cancel()

	before := snapshotOf(m, "platform").PID
	if err := m.Restart(context.Background(), "platform"); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	after := snapshotOf(m, "platform")
	if after.PID == before || after.PID == 0 {
		t.Fatalf("pid %d before, %d after — the process was not replaced", before, after.PID)
	}
	if after.ConsecutiveFailures != 0 {
		t.Errorf("a deliberate restart counted %d failures", after.ConsecutiveFailures)
	}
	if after.Unrecoverable {
		t.Error("a deliberate restart marked the child unrecoverable")
	}
	if after.LastErr != "" {
		t.Errorf("a deliberate restart recorded an error: %q", after.LastErr)
	}
}

// Restart returns once the replacement is running, so a caller that restarts
// and then probes is not answered by the process it just replaced.
func TestRestartReturnsOnlyOnceTheReplacementIsRunning(t *testing.T) {
	m, cancel := running(t, ChildSpec{Name: "platform", Command: []string{"sleep", "30"}, StopGrace: time.Second})
	defer cancel()

	before := snapshotOf(m, "platform").PID
	if err := m.Restart(context.Background(), "platform"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	// Immediately, with no waiting of the test's own.
	if got := snapshotOf(m, "platform").PID; got == before || got == 0 {
		t.Errorf("Restart returned with pid %d, the process it was asked to replace", got)
	}
}

// **The guard.** While an operation owns a child, the watchdog must not bring
// it back — otherwise the operation replacing it races the loop restoring it.
func TestAHeldChildIsNotRestartedByTheWatchdog(t *testing.T) {
	m, cancel := running(t, ChildSpec{
		Name: "platform", Command: []string{"sleep", "30"}, StopGrace: time.Second,
	})
	defer cancel()

	release, err := m.Hold("platform")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	// The child dies while the operation owns it.
	pid := snapshotOf(m, "platform").PID
	process, _ := os.FindProcess(pid)
	if err := process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitUntil(t, "the child to be seen as gone", func() bool { return snapshotOf(m, "platform").PID != pid })

	// Long enough for several backoff cycles to have brought it back.
	time.Sleep(2 * time.Second)
	if got := snapshotOf(m, "platform").PID; got != 0 && got != pid {
		t.Fatalf("the watchdog restarted a held child as pid %d", got)
	}

	// And releasing lets it come back, so a hold is a pause and not a stop.
	release()
	waitUntil(t, "the child to be restarted after release", func() bool {
		got := snapshotOf(m, "platform").PID
		return got != 0 && got != pid
	})
}

// Holds nest, so two overlapping operations do not release each other's.
func TestHoldsNest(t *testing.T) {
	m, cancel := running(t, ChildSpec{Name: "platform", Command: []string{"sleep", "30"}})
	defer cancel()

	first, err := m.Hold("platform")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	second, err := m.Hold("platform")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	first()
	if !m.held("platform") {
		t.Fatal("releasing the first hold freed the child while a second still owned it")
	}
	second()
	if m.held("platform") {
		t.Error("the child is still held after every hold was released")
	}
}

// Release is idempotent: a caller that defers it and also calls it must not
// drive the count negative, which would leave the child permanently held.
func TestReleasingTwiceIsHarmless(t *testing.T) {
	m, cancel := running(t, ChildSpec{Name: "platform", Command: []string{"sleep", "30"}})
	defer cancel()

	release, err := m.Hold("platform")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	release()
	release()

	if m.held("platform") {
		t.Fatal("still held")
	}
	// A fresh hold must still work, which it would not if the count had gone
	// negative.
	release2, err := m.Hold("platform")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if !m.held("platform") {
		t.Error("a hold taken after a double release did not take effect")
	}
	release2()
}

// An externally managed child is somebody else's to restart, and reporting
// success for something that never happened is worse than refusing.
func TestRestartingAnExternallyManagedChildIsRefused(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{Name: "platform"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	if err := m.Restart(context.Background(), "platform"); err == nil {
		t.Error("reported a restart of a process the Supervisor does not own")
	}
}

func TestOperationsOnAnUnknownChildAreRefused(t *testing.T) {
	m := NewManager("boot-1", nil)
	if _, err := m.Hold("nope"); err == nil {
		t.Error("held a child that does not exist")
	}
	if err := m.Restart(context.Background(), "nope"); err == nil {
		t.Error("restarted a child that does not exist")
	}
}
