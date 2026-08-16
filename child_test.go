// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// probeFor builds a Probe for a plain http:// URL, splitting it into the
// endpoint and the path the way configuration does.
func probeFor(t *testing.T, raw string) *Probe {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	endpoint, err := ParseEndpoint(parsed.Scheme + "://" + parsed.Host)
	if err != nil {
		t.Fatalf("endpoint for %q: %v", raw, err)
	}
	path := parsed.Path
	if path == "" {
		path = "/"
	}
	return NewProbe(endpoint, path)
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
// never happened.
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
// (supervisor#5), so it has to reach the child's environment rather than only the
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
// last — so supervisor#2's ladder is walked down rather than skipped. Stopping the
// Shell first would drain nothing (clients reach the Platform through the front
// door, not through the Shell) and would replace its offline screen with the
// holding page while the better one was still available.
func TestChildrenStopInRegistrationOrder(t *testing.T) {
	// The Manager records each stop as it begins it, which is the only
	// ordering signal available from outside. Read off the console stream
	// rather than the file, so the assertion sees them as they happen.
	var console lineRecorder
	m := NewManager("boot-1", NewTelemetry(&console, LevelInfo))

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

	stopped := console.fields("child: stopping child=")
	if len(stopped) != 2 || stopped[0] != "platform" || stopped[1] != "shell" {
		t.Errorf("stopped in order %v, want [platform shell] — the interface goes last", stopped)
	}
}

// lineRecorder collects the Supervisor's console output so a test can assert on
// what it said and in what order. Locked because the Manager writes from every
// child's goroutine.
type lineRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *lineRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// fields returns what followed prefix on each line carrying it, in order.
func (r *lineRecorder) fields(prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, line := range r.lines {
		_, rest, found := strings.Cut(line, prefix)
		if !found {
			continue
		}
		value, _, _ := strings.Cut(rest, " ")
		out = append(out, value)
	}
	return out
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
// install directory relative to it (platform#51), so a child started from the
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

// A child that cannot start must eventually be reported as one that cannot
// start. Retrying forever while saying nothing is how a dead box looks
// identical to a slow one.
func TestAChildThatNeverStartsIsReportedUnrecoverable(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:                   "doomed",
		Command:                []string{"sh", "-c", "exit 1"},
		MaxConsecutiveFailures: 3,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to be given up on", func() bool {
		return snapshotOf(m, "doomed").Unrecoverable
	})

	s := snapshotOf(m, "doomed")
	if s.ConsecutiveFailures < 3 {
		t.Errorf("reported unrecoverable after %d failures, want at least 3", s.ConsecutiveFailures)
	}
	if s.LastErr == "" {
		t.Error("unrecoverable without saying why")
	}
	if s.State != ChildFailed {
		t.Errorf("state %q, want %q", s.State, ChildFailed)
	}
}

// Retries do not stop when the ceiling is crossed. A household box whose
// database was briefly away has to heal itself rather than wait to be noticed.
func TestAnUnrecoverableChildIsStillRetried(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:                   "doomed",
		Command:                []string{"sh", "-c", "exit 1"},
		MaxConsecutiveFailures: 2,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to be given up on", func() bool {
		return snapshotOf(m, "doomed").Unrecoverable
	})
	at := snapshotOf(m, "doomed").Restarts

	waitUntil(t, "another attempt after the ceiling", func() bool {
		return snapshotOf(m, "doomed").Restarts > at
	})
}

// And the verdict is one a child can leave: a run of failures that ends in a
// child which comes up and stays up must clear, or a transient outage brands
// the process for the life of the Supervisor.
func TestBecomingReadyClearsTheFailureRun(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()

	// Fails twice using a marker file, then runs.
	marker := t.TempDir() + "/attempts"
	script := "printf x >> " + marker + "; " +
		"if [ $(wc -c < " + marker + ") -le 2 ]; then exit 1; fi; sleep 30"

	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:                   "flaky",
		Command:                []string{"sh", "-c", script},
		MaxConsecutiveFailures: 2,
		Readiness:              probeFor(t, ready.URL),
		HealthyAfter:           500 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to be given up on", func() bool {
		return snapshotOf(m, "flaky").Unrecoverable
	})
	waitUntil(t, "the child to recover", func() bool {
		s := snapshotOf(m, "flaky")
		return s.State == ChildReady && !s.Unrecoverable
	})

	if got := snapshotOf(m, "flaky").ConsecutiveFailures; got != 0 {
		t.Errorf("failure run left at %d after recovery, want 0", got)
	}
}

// A child that starts and dies immediately must still reach its ceiling.
// Readiness arrives at once for a child with no probe, so clearing the failure
// run on readiness alone would let every attempt forgive the one before it: the
// count would never rise and the condition would be unreportable forever.
func TestAChildThatDiesImmediatelyCannotForgiveItself(t *testing.T) {
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:                   "doomed",
		Command:                []string{"sh", "-c", "exit 1"},
		MaxConsecutiveFailures: 2,
		// No readiness or serving probe at all, which is the case that broke:
		// "running is ready" made every start look like a recovery.
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to be given up on", func() bool {
		return snapshotOf(m, "doomed").Unrecoverable
	})
}

// Readiness asks two independent questions. A component declaring itself
// healthy while the listener a client arrives at is unbound is exactly the
// case the second one exists for, and the first cannot see it.
func TestSelfReportedHealthIsNotEnoughToBeReady(t *testing.T) {
	selfReport := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer selfReport.Close()

	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:      "platform",
		Command:   []string{"sleep", "30"},
		Readiness: probeFor(t, selfReport.URL),
		// Nothing is listening here: the client-facing surface is down.
		Serving: probeFor(t, "http://127.0.0.1:1/mosaic.auth.v1.AuthService/Bootstrap"),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to start", func() bool { return snapshotOf(m, "platform").PID != 0 })
	// Give the probe several ticks to be wrong if it is going to be.
	time.Sleep(3 * readinessInterval)

	if s := snapshotOf(m, "platform"); s.State == ChildReady {
		t.Error("reported ready on the component's own say-so while its client-facing listener was down")
	}
}

// The client-facing probe must accept a refusal. Its natural target is a
// POST-only RPC path, which answers a GET with 405 — demanding success would
// mean issuing a real RPC against a rate-limited pre-auth surface.
func TestTheServingProbeAcceptsAnyAnswerFromTheListener(t *testing.T) {
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer serving.Close()

	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:    "platform",
		Command: []string{"sleep", "30"},
		Serving: probeFor(t, serving.URL+"/mosaic.auth.v1.AuthService/Bootstrap"),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to be ready on a 405", func() bool {
		return snapshotOf(m, "platform").State == ChildReady
	})
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

// The Generation id reaches the child too, and it is what settles an upgrade
// request (platform#77): a Platform compares the version it was asked to be
// against the one it is, because the process that would have acknowledged the
// upgrade has just been replaced by it. Nothing on the reading side reports an
// id that was never written, which is why this asserts it reaches the child's
// environment rather than only that it was set.
func TestTheGenerationIDReachesTheChildsEnvironment(t *testing.T) {
	out := t.TempDir() + "/env"
	m := NewManager("boot-1", nil)
	m.SetGenerationID("v0.4.0")
	if err := m.Add(ChildSpec{
		Name:    "recorder",
		Command: []string{"sh", "-c", "printf %s \"$MOSAIC_GENERATION_ID\" > " + out + "; sleep 30"},
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
	if string(data) != "v0.4.0" {
		t.Errorf("child saw MOSAIC_GENERATION_ID=%q, want the active Generation", data)
	}
}

// An install with nothing managing Generations sets no id rather than an empty
// one, so a Platform can tell "no Generation" from "a Generation called
// nothing" — the DIY path, where nobody was going to carry out a request
// either.
func TestNoGenerationIDIsSetWhenNothingManagesGenerations(t *testing.T) {
	out := t.TempDir() + "/env"
	m := NewManager("boot-1", nil)
	if err := m.Add(ChildSpec{
		Name:    "recorder",
		Command: []string{"sh", "-c", "printenv MOSAIC_GENERATION_ID > " + out + "; echo done >> " + out + "; sleep 30"},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitUntil(t, "the child to record its environment", func() bool {
		data, err := os.ReadFile(out)
		return err == nil && strings.Contains(string(data), "done")
	})
	data, _ := os.ReadFile(out)
	if strings.TrimSpace(string(data)) != "done" {
		t.Errorf("MOSAIC_GENERATION_ID was set to %q when nothing manages Generations", data)
	}
}
