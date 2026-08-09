// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errRestartRequested ends a run that the Supervisor asked to end. It is
// distinct from every other way a run finishes, because it is the one that
// must not count against the child: a planned stop is not a crash, and
// treating it as one would have a Generation activation look like a failing
// Platform and eventually be reported unrecoverable.
var errRestartRequested = errors.New("restart requested")

// Hold stops the watchdog acting on a child while an operation owns it, and
// returns the release.
//
// This is the guard Home Assistant's Supervisor has and Mosaic did not: theirs
// refuses to act "while a task is in progress", and without it an operation
// that stops the Platform on purpose — activating a Generation, applying a
// Restart-class configuration change — races the restart loop, which sees a
// dead child and brings it back underneath the operation that was replacing
// it.
//
// Holds nest, so two operations can overlap without the first release
// unblocking the child from under the second. Release is idempotent: calling
// it twice is a no-op rather than a negative count that would leave the child
// permanently held.
func (m *Manager) Hold(name string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok {
		return nil, fmt.Errorf("no child named %q", name)
	}
	c.holds++

	var once bool
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if once {
			return
		}
		once = true
		if c.holds > 0 {
			c.holds--
		}
	}, nil
}

// held reports whether an operation currently owns this child.
func (m *Manager) held(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	return ok && c.holds > 0
}

// waitWhileHeld blocks until no operation owns the child, or ctx ends.
//
// Polling rather than signalling, deliberately: a hold is measured in the
// seconds an activation takes, the check is a mutex and a comparison, and a
// condition variable here would add a second synchronisation shape to a file
// that already has one.
func (m *Manager) waitWhileHeld(ctx context.Context, name string) {
	for m.held(name) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(holdPollInterval):
		}
	}
}

const holdPollInterval = 100 * time.Millisecond

// Restart stops a child and lets it be started again, without the stop
// counting against it.
//
// It takes the hold itself, so a caller cannot forget to: the window between
// asking a process to stop and seeing the replacement running is exactly the
// window where the watchdog would otherwise intervene.
//
// It returns once the replacement is running, not once it is *ready* —
// readiness is what the probes report and what a caller should wait on if it
// needs to know the new process is serving. Blocking here would make a
// Restart of a Platform that cannot start hang until its ceiling instead of
// reporting what happened.
func (m *Manager) Restart(ctx context.Context, name string) error {
	m.mu.Lock()
	c, ok := m.children[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no child named %q", name)
	}
	if len(c.spec.Command) == 0 && !c.spec.Managed {
		m.mu.Unlock()
		// An externally managed child is somebody else's to restart, and
		// pretending otherwise would report success for something that never
		// happened. A *managed* child with no command is the other case: it is
		// waiting for its first one, and this is the signal that it has one.
		return fmt.Errorf("child %q is externally managed; the Supervisor does not own its lifecycle", name)
	}
	before := c.starts
	restart := c.restart
	m.mu.Unlock()

	m.tel.Info(componentChild, "restarting", String("child", name))
	select {
	case restart <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Wait for the replacement to have been started, so a caller that
	// restarts and then immediately probes is not answered by the process it
	// just replaced.
	for {
		m.mu.Lock()
		started := c.starts > before
		m.mu.Unlock()
		if started {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(holdPollInterval):
		}
	}
}
