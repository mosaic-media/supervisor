// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"fmt"
	"time"
)

// What an activation needs from the Manager, kept beside it rather than in
// child.go: changing which binary a child runs and waiting for it to serve are
// operations *on* the lifecycle rather than part of it.

// CommandOf returns a child's current argv.
//
// It copies, because the caller keeps it as the thing to restore on a failed
// activation — and a slice aliasing the live spec would be rewritten by the
// very SetCommand whose effect it exists to undo.
func (m *Manager) CommandOf(name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok {
		return nil, fmt.Errorf("supervisor: no child named %q", name)
	}
	if len(c.spec.Command) == 0 {
		return nil, fmt.Errorf("supervisor: child %q is externally managed; the Supervisor does not own its lifecycle", name)
	}
	return append([]string(nil), c.spec.Command...), nil
}

// SetCommand replaces the argv a child will be started with next.
//
// **It does not restart anything.** The running process keeps running until
// something stops it, and `superviseOne` re-reads the spec at the top of every
// iteration, so the new command takes effect on the next start and not before.
// That is what lets an activation set the command and then restart, rather than
// racing a change against a process that is already coming back.
//
// An externally managed child is refused rather than silently given a command:
// it would make the Supervisor start a process compose already owns.
func (m *Manager) SetCommand(name string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("supervisor: child %q needs a command", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok {
		return fmt.Errorf("supervisor: no child named %q", name)
	}
	if len(c.spec.Command) == 0 {
		return fmt.Errorf("supervisor: child %q is externally managed; the Supervisor does not own its lifecycle", name)
	}
	c.spec.Command = append([]string(nil), argv...)
	return nil
}

// Order returns the children in registration order — the order they are started
// and stopped in, and therefore the order an activation restarts them in.
func (m *Manager) Order() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

// WaitReady blocks until a child is serving, or until the timeout.
//
// **What it waits on is the state the probes decide**, which for the Platform
// and the Shell means a request to the socket a client actually reaches — not
// the child's opinion of itself. That is the whole reason an activation can
// gate on it: a component reporting every subsystem loaded while its listener
// is unbound is precisely the failure a self-report cannot see, and precisely
// the one that must trigger a revert.
//
// A child with no probes reports ready as soon as it is running, which is the
// strongest claim available for it and is stated on ChildSpec.Readiness. An
// activation of such a child therefore checks that it started and nothing more.
func (m *Manager) WaitReady(ctx context.Context, name string, timeout time.Duration) error {
	if _, err := m.CommandOf(name); err != nil {
		return err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	for {
		snap := m.snapshotOf(name)
		switch {
		case snap.State == ChildReady:
			return nil
		case snap.Unrecoverable:
			// It has already failed its ceiling, so waiting out the timeout
			// would only delay a revert that is now certain.
			return fmt.Errorf("supervisor: child %q is not coming up: %s", name, snap.LastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("supervisor: child %q was not serving within %s (state %s)",
				name, timeout, snap.State)
		case <-ticker.C:
		}
	}
}

// readyPollInterval is how often WaitReady looks. The readiness prober runs on
// its own schedule and this only reads the state it publishes, so it is cheap
// and can be frequent enough that an activation is not padded by it.
const readyPollInterval = 100 * time.Millisecond

// snapshotOf reads one child's health without allocating the whole Health.
func (m *Manager) snapshotOf(name string) ChildSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok {
		return ChildSnapshot{Name: name}
	}
	return ChildSnapshot{
		Name:          name,
		State:         c.state,
		PID:           c.pid,
		Unrecoverable: c.unrecoverable,
		LastErr:       c.lastErr,
	}
}
