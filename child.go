// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors
// Linking exception: see LICENSE-EXCEPTION.

package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	// WorkingDir, when set, is the child's working directory.
	WorkingDir string
}

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
}

type child struct {
	spec     ChildSpec
	state    ChildState
	pid      int
	restarts int
	lastErr  string
	cmd      *exec.Cmd
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
// them and returns. It blocks.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	m.mu.Lock()
	names := append([]string(nil), m.order...)
	m.mu.Unlock()

	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			m.superviseOne(ctx, name)
		}(name)
	}
	wg.Wait()
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
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
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
	if c.restarts >= 0 {
		c.restarts++
	}
	m.mu.Unlock()
	m.log("child %s started (pid %d)", name, cmd.Process.Pid)

	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	go m.pollReadiness(probeCtx, name)

	// Stop the child when the Supervisor is asked to stop.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		m.stop(cmd)
		<-done
		return ctx.Err()
	}
}

// stop asks politely, then insists. A Platform mid-transaction deserves the
// chance to finish it; a Platform that ignores SIGTERM must not be able to
// block an activation forever.
func (m *Manager) stop(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-time.After(15 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	case <-waitFor(cmd):
	}
}

// waitFor closes its channel once the process has gone, polling because
// cmd.Wait is already owned by the caller.
func waitFor(cmd *exec.Cmd) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		for range 150 {
			if cmd.ProcessState != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return ch
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
		out = append(out, ChildSnapshot{
			Name:     name,
			State:    c.state,
			PID:      c.pid,
			Restarts: c.restarts,
			LastErr:  c.lastErr,
		})
	}
	return Health{Children: out}
}
