// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors
// Linking exception: see LICENSE-EXCEPTION.

package supervisor

import (
	"context"
	"os"
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
