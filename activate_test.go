// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An activation harness: two Generations, each holding a "binary" that
// announces which one it is and then sleeps, and a listener standing in for the
// surface a client reaches.
type activation struct {
	gens    *Generations
	mgr     *Manager
	act     *Activator
	cancel  context.CancelFunc
	ranLog  string
	serving *atomic.Bool
}

// fakeBinary writes a script that records the path it was started from and then
// sleeps. Recording argv[0] is what makes "which Generation is running" a fact
// the test reads rather than infers from a pid changing.
func fakeBinary(t *testing.T, dir, name, ranLog, says string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"echo \"$0\" >> " + ranLog + "\n" +
		"echo '" + says + "'\n" +
		"exec sleep 300\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// newActivation stands up a Manager running v0.1.0 with a functional probe, and
// stages v0.2.0 ready to activate.
func newActivation(t *testing.T) *activation {
	t.Helper()
	g := generations(t)
	ranLog := filepath.Join(t.TempDir(), "ran.log")

	for _, v := range []string{"v0.1.0", "v0.2.0"} {
		dir, err := g.Stage(v)
		if err != nil {
			t.Fatal(err)
		}
		fakeBinary(t, dir, "mosaic-platform", ranLog, "platform "+v+" starting")
		if err := g.Commit(v); err != nil {
			t.Fatal(err)
		}
	}

	// The surface a client reaches. When it stops answering, the Serving probe
	// stops passing — which is the functional failure an activation must revert
	// on, as opposed to a child that merely says it is unwell.
	serving := &atomic.Bool{}
	serving.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !serving.Load() {
			// Hijacked and closed, so the client gets no response at all —
			// the shape of a listener that is not there, rather than an error
			// status, which the Serving probe deliberately accepts.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:      "platform",
		Command:   []string{filepath.Join(g.Dir("v0.1.0"), "mosaic-platform")},
		Serving:   probeFor(t, srv.URL),
		StopGrace: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	waitUntil(t, "the first generation to serve", func() bool {
		return snapshotOf(m, "platform").State == ChildReady
	})
	if err := g.Activate("v0.1.0"); err != nil {
		t.Fatal(err)
	}

	return &activation{
		gens: g, mgr: m, cancel: cancel, ranLog: ranLog, serving: serving,
		act: &Activator{
			Generations:  g,
			Manager:      m,
			Targets:      []ActivationTarget{{Child: "platform", Binary: "mosaic-platform"}},
			ReadyTimeout: 3 * time.Second,
			Now:          func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) },
		},
	}
}

// ran reports whether a binary from this version has been started.
func (a *activation) ran(t *testing.T, version string) bool {
	t.Helper()
	b, err := os.ReadFile(a.ranLog)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), a.gens.Dir(version)+string(os.PathSeparator))
}

// The whole path: the child is restarted onto the new binary, it serves, and
// only then is the Generation recorded as live.
func TestActivationRestartsTheChildOntoTheNewGeneration(t *testing.T) {
	a := newActivation(t)

	if err := a.act.Activate(context.Background(), "v0.2.0"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !a.ran(t, "v0.2.0") {
		t.Error("the new generation's binary was never started")
	}
	if v, _ := a.gens.Active(); v != "v0.2.0" {
		t.Errorf("active = %q", v)
	}
	if v, _ := a.gens.Previous(); v != "v0.1.0" {
		t.Errorf("previous = %q — the rollback target was not recorded", v)
	}
	argv, err := a.mgr.CommandOf("platform")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(argv[0], "v0.2.0") {
		t.Errorf("the child's command is %q", argv[0])
	}
}

// **The load-bearing test.** A Generation that starts but does not serve is
// reverted, the pointer is left alone, and what the failing child said is kept
// — because the revert is what would otherwise destroy it.
func TestAFailedActivationRevertsAndKeepsTheEvidence(t *testing.T) {
	a := newActivation(t)
	a.serving.Store(false) // the listener a client reaches is gone

	err := a.act.Activate(context.Background(), "v0.2.0")
	if !errors.Is(err, ErrActivationFailed) {
		t.Fatalf("Activate: %v, want ErrActivationFailed", err)
	}

	// The pointer was never moved, so a Supervisor that died here would come
	// back on the Generation it was already running.
	if v, _ := a.gens.Active(); v != "v0.1.0" {
		t.Errorf("active = %q — the pointer moved for a generation that never served", v)
	}
	// And the command is back, so the next restart is the old binary.
	argv, err := a.mgr.CommandOf("platform")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(argv[0], "v0.1.0") {
		t.Errorf("the child's command is %q — the revert did not restore it", argv[0])
	}

	// The evidence: what the new Generation said before it was taken away.
	entries, err := os.ReadDir(filepath.Join(a.gens.root, "failed"))
	if err != nil {
		t.Fatalf("no failed-activation directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("failed/ holds %d files, want 1", len(entries))
	}
	if want := "v0.2.0-20260809T120000Z.log"; entries[0].Name() != want {
		t.Errorf("evidence file is %q, want %q", entries[0].Name(), want)
	}
	body, err := os.ReadFile(filepath.Join(a.gens.root, "failed", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "platform v0.2.0 starting") {
		t.Errorf("the evidence does not contain what the failing generation said:\n%s", body)
	}
}

// A revert puts the previous binary back into service, not merely into the
// spec: an install that worked before a failed upgrade has to work after it.
func TestARevertLeavesThePreviousGenerationRunning(t *testing.T) {
	a := newActivation(t)
	a.serving.Store(false)

	if err := a.act.Activate(context.Background(), "v0.2.0"); !errors.Is(err, ErrActivationFailed) {
		t.Fatalf("Activate: %v", err)
	}
	// The probe is answering again, as it would once the cause was removed.
	a.serving.Store(true)

	waitUntil(t, "the reverted child to serve again", func() bool {
		return snapshotOf(a.mgr, "platform").State == ChildReady
	})
	argv, _ := a.mgr.CommandOf("platform")
	if !strings.Contains(argv[0], "v0.1.0") {
		t.Errorf("running %q after the revert", argv[0])
	}
}

// An incomplete Generation is refused before anything is stopped — the same
// marker the fetch path writes last is what an activation reads first.
func TestAnIncompleteGenerationIsNotActivated(t *testing.T) {
	a := newActivation(t)
	if _, err := a.gens.Stage("v0.3.0"); err != nil {
		t.Fatal(err)
	}

	if err := a.act.Activate(context.Background(), "v0.3.0"); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Activate: %v, want ErrIncomplete", err)
	}
	if a.ran(t, "v0.3.0") {
		t.Error("an incomplete generation was started")
	}
}

// A Generation missing a binary this install needs is refused before anything
// is stopped, rather than discovered halfway and reverted from.
func TestAGenerationMissingABinaryIsRefusedBeforeStopping(t *testing.T) {
	a := newActivation(t)
	a.act.Targets = append(a.act.Targets, ActivationTarget{Child: "platform", Binary: "mosaic-shell"})

	err := a.act.Activate(context.Background(), "v0.2.0")
	if err == nil || !strings.Contains(err.Error(), "mosaic-shell") {
		t.Fatalf("Activate: %v, want a refusal naming the missing binary", err)
	}
	if a.ran(t, "v0.2.0") {
		t.Error("a generation missing a binary was started anyway")
	}
	if v, _ := a.gens.Active(); v != "v0.1.0" {
		t.Errorf("active = %q", v)
	}
}

// An activation naming no targets would change nothing and record success,
// which is the worst available outcome: an install reporting a version it is
// not running.
func TestAnActivationWithNoTargetsIsRefused(t *testing.T) {
	a := newActivation(t)
	a.act.Targets = nil

	if err := a.act.Activate(context.Background(), "v0.2.0"); err == nil {
		t.Fatal("an activation with no targets succeeded")
	}
	if v, _ := a.gens.Active(); v != "v0.1.0" {
		t.Errorf("active = %q", v)
	}
}

// A deliberate rollback after a successful activation — distinct from the
// revert above, which nobody asked for.
func TestRollbackReturnsToThePreviousGeneration(t *testing.T) {
	a := newActivation(t)
	if err := a.act.Activate(context.Background(), "v0.2.0"); err != nil {
		t.Fatal(err)
	}

	back, err := a.act.Rollback(context.Background())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back != "v0.1.0" {
		t.Errorf("rolled back to %q", back)
	}
	if v, _ := a.gens.Active(); v != "v0.1.0" {
		t.Errorf("active = %q", v)
	}
	argv, _ := a.mgr.CommandOf("platform")
	if !strings.Contains(argv[0], "v0.1.0") {
		t.Errorf("running %q after the rollback", argv[0])
	}
}

// Rolling back with nowhere to go is refused rather than leaving the install
// pointing at nothing.
func TestRollbackWithNoPreviousIsRefused(t *testing.T) {
	a := newActivation(t)
	if _, err := a.act.Rollback(context.Background()); !errors.Is(err, ErrNoPrevious) {
		t.Errorf("Rollback: %v, want ErrNoPrevious", err)
	}
}

// The capture keeps the most recent output and is bounded, because a child that
// fails by logging furiously is the ordinary case and the Supervisor is the
// process that must not grow.
func TestTheCaptureKeepsTheTailAndIsBounded(t *testing.T) {
	r := newRing(32)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(r, "line %d\n", i)
	}
	got := r.Bytes()
	if len(got) > 32 {
		t.Errorf("held %d bytes, limit is 32", len(got))
	}
	if !strings.Contains(string(got), "line 99") {
		t.Errorf("the tail was not kept: %q", got)
	}
	if strings.Contains(string(got), "line 0\n") {
		t.Errorf("the head was kept: %q", got)
	}
}

// A capture attached after a child started still sees its output — which is the
// whole reason the Manager's console destination is consulted per write rather
// than captured when a child is constructed.
func TestACaptureSeesAChildThatWasAlreadyRunning(t *testing.T) {
	a := newActivation(t)
	stop := a.mgr.Capture()

	// Restarting the already-running child makes it announce itself again.
	if err := a.mgr.Restart(context.Background(), "platform"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the restarted child to announce itself", func() bool {
		return strings.Contains(string(peek(a.mgr)), "platform v0.1.0 starting")
	})
	if !strings.Contains(string(stop()), "platform v0.1.0 starting") {
		t.Error("the capture did not see a child that was already running")
	}
}

// peek reads the live capture without stopping it.
func peek(m *Manager) []byte {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	if m.capture == nil {
		return nil
	}
	return m.capture.Bytes()
}
