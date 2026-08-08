// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// NewID returns a short random hex identifier, matching the shape the
// Platform's telemetry resource uses so boot ids look alike in a log somebody
// is reading across both processes.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// ChildState is what the Supervisor believes about one child.
type ChildState string

const (
	ChildStarting ChildState = "starting"
	ChildReady    ChildState = "ready"
	ChildStopped  ChildState = "stopped"
	ChildFailed   ChildState = "failed"
)

// ChildSnapshot is the reportable view, carried by the health probe.
type ChildSnapshot struct {
	Name     string     `json:"name"`
	State    ChildState `json:"state"`
	PID      int        `json:"pid,omitempty"`
	Restarts int        `json:"restarts"`
	LastErr  string     `json:"lastError,omitempty"`
}

// ChildSpec describes a process the Supervisor owns.
type ChildSpec struct {
	// Name is the reporting name, e.g. "platform" or "shell".
	Name string
	// Command is argv. Empty means this child is externally managed and the
	// Supervisor only fronts it — which is the shape the dev stack uses.
	Command []string
	// Env is added to the Supervisor's own environment.
	Env []string
	// ReadinessURL is polled to decide whether the child is ready. Empty
	// means "running is ready", which is weaker but honest for a process
	// with no probe.
	ReadinessURL string
	// WorkingDir, when set, is the child's working directory. It matters for
	// more than tidiness: the Platform resolves several paths relative to it,
	// including the extension install directory (ADR 0081), so a child
	// started from the wrong directory finds none of its installed modules.
	WorkingDir string
	// StopGrace is how long this child gets between SIGTERM and SIGKILL.
	// Zero means defaultStopGrace.
	//
	// It is per child because the right answer differs by an order of
	// magnitude: the Shell serves static files and has nothing to finish,
	// while the Platform may be mid-transaction or draining a playback
	// session, and killing it early turns a planned shutdown into the kind
	// of unclean stop that costs a recovery on the next boot.
	StopGrace time.Duration
}

// defaultStopGrace is deliberately generous. Waiting too long for a process
// that has already finished costs nothing — the wait ends as soon as it exits.
const defaultStopGrace = 15 * time.Second

// Manager runs the children and keeps them running.
//
// Restart policy is exponential backoff capped at a minute, reset once a child
// has stayed up long enough to be considered healthy. The reset matters: a
// process that crashes every hour must not inherit the backoff earned by a
// crash loop last week and then take a minute to come back.
type Manager struct {
	mu       sync.Mutex
	children map[string]*child
	order    []string
	bootID   string
	probe    *http.Client
	log      func(string, ...any)
	// out and outMu are the children's console destination and the lock that
	// keeps one child's line from interleaving with another's mid-way.
	out   io.Writer
	outMu sync.Mutex
}

type child struct {
	spec  ChildSpec
	state ChildState
	pid   int
	// starts counts every start including the first. Restarts are reported as
	// starts-1, because the first start is not a restart — a freshly booted
	// install reporting "1 restart" reads as a crash that never happened, and
	// this number is one an operator judges stability by.
	starts  int
	lastErr string
	cmd     *exec.Cmd
}

// NewManager builds a Manager. log may be nil.
func NewManager(bootID string, log func(string, ...any)) *Manager {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Manager{
		children: map[string]*child{},
		bootID:   bootID,
		probe:    &http.Client{Timeout: readinessTimeout},
		log:      log,
		out:      os.Stdout,
	}
}

// Add registers a child. It does not start it.
func (m *Manager) Add(spec ChildSpec) error {
	if spec.Name == "" {
		return errors.New("a child needs a name")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.children[spec.Name]; exists {
		return fmt.Errorf("child %q is already registered", spec.Name)
	}
	m.children[spec.Name] = &child{spec: spec, state: ChildStopped}
	m.order = append(m.order, spec.Name)
	return nil
}

// Run supervises every registered child until ctx is cancelled, then stops
// them **in registration order** and returns. It blocks.
//
// The order is the point, and it is deliberately *not* the conventional
// stop-dependents-first. That rule exists to drain traffic through the
// dependent before the thing it depends on goes away, and it does not apply
// here: clients reach the Platform through the front door directly, never
// through the Shell, which serves static files and has nothing in flight. So
// stopping the Shell first would drain nothing — it would only destroy the
// best interface still standing.
//
// Register children in the order they should be given up, which means most
// expendable first and the interface last. ADR 0005 has the Supervisor use the
// richest available presentation layer, and a shutdown is where that ladder is
// walked rather than skipped: the Platform goes, and the Shell — still up —
// renders its offline state; then the Shell goes and the holding page answers;
// then the front door closes. Taking the Shell first would jump straight to
// the holding page while a far better screen was still available.
//
// Each child is fully stopped before the next is asked to, so a child's stop
// grace is its own rather than a share of one global deadline. The cost is
// that shutdown is the sum of the children's stops rather than the longest of
// them, which is what buys the ordering.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	names := append([]string(nil), m.order...)
	m.mu.Unlock()

	// Each child gets its own cancellation so they can be stopped one at a
	// time. Deriving them from context.Background() rather than from ctx is
	// what makes that possible: a child derived from ctx would be cancelled
	// the instant ctx was, and every "ordered" stop would in fact be
	// simultaneous.
	stops := make([]context.CancelFunc, len(names))
	dones := make([]chan struct{}, len(names))

	for i, name := range names {
		childCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		stops[i], dones[i] = cancel, done

		go func(name string) {
			defer close(done)
			m.superviseOne(childCtx, name)
		}(name)
	}

	<-ctx.Done()

	for i := range names {
		m.log("stopping child %s", names[i])
		stops[i]()
		<-dones[i]
	}
}

// superviseOne is the restart loop for one child.
func (m *Manager) superviseOne(ctx context.Context, name string) {
	backoff := time.Second
	const maxBackoff = time.Minute
	// healthyFor is how long a child must stay up before its backoff resets.
	const healthyFor = 30 * time.Second

	for ctx.Err() == nil {
		spec := m.specOf(name)

		// A child with no command is externally managed: the Supervisor
		// fronts it and reports on it, but does not own its lifecycle. That
		// is the dev stack's shape, where compose owns the processes.
		if len(spec.Command) == 0 {
			m.pollReadiness(ctx, name)
			return
		}

		started := time.Now()
		err := m.runOnce(ctx, name, spec)
		if ctx.Err() != nil {
			return
		}

		m.setFailed(name, err)
		if time.Since(started) >= healthyFor {
			backoff = time.Second
		}
		m.log("child %s exited (%v); restarting in %s", name, err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce starts the child and waits for it, probing readiness meanwhile.
func (m *Manager) runOnce(ctx context.Context, name string, spec ChildSpec) error {
	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.WorkingDir
	// Attributed, because three processes share one terminal. Both streams go
	// to the same destination through the same lock, so a child's stderr line
	// cannot land inside its own stdout line.
	stdout := newPrefixWriter(m.out, &m.outMu, name)
	stderr := newPrefixWriter(m.out, &m.outMu, name)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The boot id reaches every child, which is what stitches the three
	// processes' records into one timeline (ADR 0060).
	cmd.Env = append(append(os.Environ(), "MOSAIC_BOOT_ID="+m.bootID), spec.Env...)
	// Its own process group, so stopping the Supervisor can stop a child's
	// whole tree rather than orphaning grandchildren.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}

	m.mu.Lock()
	c := m.children[name]
	c.cmd, c.pid, c.state, c.lastErr = cmd, cmd.Process.Pid, ChildStarting, ""
	c.starts++
	m.mu.Unlock()
	m.log("child %s started (pid %d)", name, cmd.Process.Pid)

	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	go m.pollReadiness(probeCtx, name)

	// Stop the child when the Supervisor is asked to stop.
	//
	// `done` is buffered and carries the exit error; `exited` is closed
	// alongside it purely as a broadcast, so stop can wait for the process
	// without consuming the value runOnce still needs to return.
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(exited)
	}()

	// A crashing process's last line often has no trailing newline. Flushing
	// on the way out is what keeps it from being swallowed — which would lose
	// exactly the line that says why the child died.
	defer func() {
		stdout.Flush()
		stderr.Flush()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		m.stop(cmd, spec.StopGrace, exited)
		<-done
		return ctx.Err()
	}
}

// stop asks politely, then insists. A Platform mid-transaction deserves the
// chance to finish it; a Platform that ignores SIGTERM must not be able to
// block an activation forever.
//
// The signal goes to the process *group* (the negative pid), which is why
// runOnce sets Setpgid: a child that spawned its own children — an extension
// module's process, an ffmpeg — would otherwise leave them orphaned and still
// holding the port the replacement is about to want.
func (m *Manager) stop(cmd *exec.Cmd, grace time.Duration, exited <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	if grace <= 0 {
		grace = defaultStopGrace
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(grace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	}
}

// pollReadiness moves a child to ready once its probe answers, and back to
// starting when it stops answering.
func (m *Manager) pollReadiness(ctx context.Context, name string) {
	spec := m.specOf(name)
	if spec.ReadinessURL == "" {
		// No probe: running is the strongest claim available.
		m.setState(name, ChildReady)
		return
	}
	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()
	for {
		if m.checkOnce(ctx, spec.ReadinessURL) {
			m.setState(name, ChildReady)
		} else {
			m.setState(name, ChildStarting)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) checkOnce(ctx context.Context, target string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	resp, err := m.probe.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func (m *Manager) specOf(name string) ChildSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.children[name].spec
}

func (m *Manager) setState(name string, state ChildState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.children[name]; ok {
		c.state = state
	}
}

func (m *Manager) setFailed(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok {
		return
	}
	c.state = ChildFailed
	c.pid = 0
	if err != nil {
		c.lastErr = err.Error()
	}
}

// Snapshot reports every child, in registration order so the output is stable.
func (m *Manager) Snapshot() Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChildSnapshot, 0, len(m.order))
	for _, name := range m.order {
		c := m.children[name]
		restarts := c.starts - 1
		if restarts < 0 {
			restarts = 0
		}
		out = append(out, ChildSnapshot{
			Name:     name,
			State:    c.state,
			PID:      c.pid,
			Restarts: restarts,
			LastErr:  c.lastErr,
		})
	}
	return Health{Children: out}
}
