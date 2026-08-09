// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Activating a Generation, and reverting as the same path's failure branch.
//
// **The revert is not a second feature.** Home Assistant's update installs,
// starts, health-checks and — on failure — puts the previous version back and
// starts that: one path, with the revert as its `else`. Written as two
// operations they drift, and the one that runs least is the one that has to
// work.
//
// Two orderings carry most of the correctness here.
//
// **The pointer is written last, after every child is serving.** A Supervisor
// that dies mid-activation then starts the *old* Generation on its next boot,
// because the pointer still names it — the failure falls backwards. Writing the
// pointer first would have a crash leave an install committed to a Generation
// nobody proved.
//
// **The health check is the Serving probe, not the child's own opinion.** A
// Platform can report every subsystem loaded while the listener a client
// arrives at is unbound, and the state this waits on requires whichever probes
// a child declares — so for the Platform and the Shell it is a request to the
// socket a user reaches. Gating a revert on a self-report is how a broken
// upgrade stays.

// ErrActivationFailed wraps whatever went wrong, so a caller can tell an
// activation that reverted from one that never started.
var ErrActivationFailed = errors.New("supervisor: the generation did not come up")

// defaultReadyTimeout is how long a child gets to serve after being restarted
// onto a new Generation.
//
// Two minutes because the Platform migrates on boot and a migration over a
// large library on a domestic NAS is the slow case this must not mistake for a
// failure. It is long enough that a genuinely broken Platform costs two minutes
// before the revert, which is the right way round: reverting a working upgrade
// is worse than a slow one.
const defaultReadyTimeout = 2 * time.Minute

// ActivationTarget maps a child to the binary it runs from a Generation.
//
// The argv is built here rather than stored on the ChildSpec because it is the
// one part that changes per Generation: everything else about a child — its
// probes, its grace, its working directory — is a property of what it is, not
// of which version it happens to be.
type ActivationTarget struct {
	// Child is the name the Manager knows it by.
	Child string
	// Binary is the file within the Generation directory.
	Binary string
	// Args follow the binary.
	Args []string
}

// Activator turns a completed Generation into the running one.
type Activator struct {
	Generations *Generations
	Manager     *Manager
	Targets     []ActivationTarget
	// ReadyTimeout is how long each child gets to serve. Zero takes
	// defaultReadyTimeout.
	ReadyTimeout time.Duration
	// Now is the clock, injectable so the evidence file's name is testable.
	Now func() time.Time
	// Tel is where the switch is recorded (ADR 0060). A Generation activated
	// that immediately dies is the failure whose interesting question is what
	// changed between the previous Generation and this one, and only this
	// process is in a position to answer it.
	Tel *Telemetry
	// Activity is where the switch reports itself for the screen somebody is
	// watching. This is the phase a person is most likely to *be* watching:
	// their Mosaic went away and they want to know whether it is coming back.
	Activity *Activity
	// Spool is where a failed activation is recorded for the Platform to adopt
	// (ADR 0119). **This is the case that document was written for**: an
	// install that silently undoes its own upgrade and says so only in a log
	// produces a bug report three days after the evidence rotated away.
	Spool *Spool
}

// Activate restarts every target onto version and, only if all of them serve,
// records it as the live Generation.
//
// On any failure it writes what the children said to a file and puts the
// previous commands back — so an install that was working before an upgrade is
// working after a failed one, and the reason the upgrade failed outlives it.
func (a *Activator) Activate(ctx context.Context, version string) (err error) {
	if a.Generations == nil || a.Manager == nil {
		return errors.New("supervisor: an activator needs generations and a manager")
	}
	if len(a.Targets) == 0 {
		return errors.New("supervisor: an activation with no targets would change nothing and record success")
	}
	if !a.Generations.Complete(version) {
		return fmt.Errorf("%w: %s", ErrIncomplete, version)
	}

	dir := a.Generations.Dir(version)
	// Every binary is checked to exist before anything is stopped. Discovering
	// a missing one halfway through means a revert that never needed to happen,
	// and the check costs a stat.
	for _, t := range a.Targets {
		if _, err := os.Stat(filepath.Join(dir, t.Binary)); err != nil {
			return fmt.Errorf("supervisor: generation %s has no %s: %w", version, t.Binary, err)
		}
	}

	previous := map[string][]string{}
	for _, t := range a.Targets {
		argv, err := a.Manager.CommandOf(t.Child)
		if err != nil {
			return err
		}
		previous[t.Child] = argv
	}

	// Recording starts before the first child is stopped, because the useful
	// output is what the new Generation says on its way down.
	evidence := a.Manager.Capture()

	from, _ := a.Generations.Active()
	started := a.now()
	a.Tel.Info(componentGeneration, "activating",
		String("version", version), String("from", from))
	// Cleared however this ends. On the failure branch below, the honest screen
	// is what the reverted children are actually doing — which the front door
	// infers on its own — rather than an "upgrading" that has stopped.
	defer a.Activity.Done()
	if err := a.switchTo(ctx, dir, version); err != nil {
		a.revert(ctx, version, previous, evidence())
		a.Spool.Record(FindingGenerationRolledBack, ContextGeneration, version, errText(err))
		return fmt.Errorf("%w: %v", ErrActivationFailed, err)
	}

	// Serving, so the pointer may be written. Last, so a crash before this
	// leaves the install on the Generation it was already running.
	evidence()
	if err := a.Generations.Activate(version); err != nil {
		return err
	}
	a.Tel.Info(componentGeneration, "is live",
		String("version", version), String("from", from),
		Duration("took", a.now().Sub(started)))
	return nil
}

// switchTo points every target at dir and waits for each to serve, in the
// Manager's registration order — the Platform before the Shell, the same order
// they start and stop in, so an activation does not invent a third.
func (a *Activator) switchTo(ctx context.Context, dir, version string) error {
	targets := a.ordered()
	for i, t := range targets {
		// Counted in children rather than timed. How long a child takes to come
		// up is not knowable in advance — the Platform may be running
		// migrations — so a bar that guessed at seconds would be wrong in the
		// one direction that matters, and a person watching an upgrade wants to
		// know it is *progressing* rather than how many seconds are left.
		a.Activity.Report(PhaseUpgrading, version,
			fmt.Sprintf("restarting %s (%d of %d)", t.Child, i+1, len(targets)),
			float64(i)/float64(len(targets)))
		argv := append([]string{filepath.Join(dir, t.Binary)}, t.Args...)
		if err := a.Manager.SetCommand(t.Child, argv); err != nil {
			return err
		}
		if err := a.Manager.Restart(ctx, t.Child); err != nil {
			return fmt.Errorf("restarting %s: %w", t.Child, err)
		}
		if err := a.Manager.WaitReady(ctx, t.Child, a.readyTimeout()); err != nil {
			return fmt.Errorf("%s did not come up: %w", t.Child, err)
		}
		a.log("child %s is serving the new generation", t.Child)
		a.Activity.Report(PhaseUpgrading, version,
			fmt.Sprintf("restarting %s (%d of %d)", t.Child, i+1, len(targets)),
			float64(i+1)/float64(len(targets)))
	}
	return nil
}

// revert puts the previous commands back and keeps the evidence.
//
// **The evidence is written before anything is restarted.** The old Generation
// starts cleanly and says so, and on a console that is the same stream — so a
// revert that restarted first would bury the reason underneath the recovery.
//
// It is best-effort by necessity: this runs because something already went
// wrong, and a failure here is reported rather than returned, because the
// caller's error is the one that explains the situation.
func (a *Activator) revert(ctx context.Context, version string, previous map[string][]string, output []byte) {
	if path, err := a.writeEvidence(version, output); err != nil {
		a.log("could not keep the failed activation's output: %v", err)
	} else if path != "" {
		a.log("what generation %s said before the revert is in %s", version, path)
	}

	a.Tel.Error(componentGeneration, "did not come up and is being reverted",
		String("version", version))
	for _, t := range a.ordered() {
		argv, ok := previous[t.Child]
		if !ok || len(argv) == 0 {
			continue
		}
		if err := a.Manager.SetCommand(t.Child, argv); err != nil {
			a.log("could not restore %s's command: %v", t.Child, err)
			continue
		}
		// The context may already be cancelled — a shutdown during an
		// activation is one way to get here — and the revert still has to put
		// the commands back so the next boot is not left on the new binary.
		// The restart is what best-effort means; the command is not.
		if err := a.Manager.Restart(ctx, t.Child); err != nil {
			a.log("could not restart %s during the revert: %v", t.Child, err)
		}
	}
}

// Rollback returns a running install to the previous Generation, deliberately.
//
// Distinct from the revert above, which is an activation's failure branch and
// happens without anybody asking. This is somebody noticing after the fact that
// the new Generation is worse, which is a decision rather than a detection —
// and it is the reason [Generations.Rollback] swaps the pointers rather than
// dropping one.
func (a *Activator) Rollback(ctx context.Context) (version string, err error) {
	previous, ok := a.Generations.Previous()
	if !ok {
		return "", ErrNoPrevious
	}
	// The pointer moves first here, unlike an activation, and for the same
	// reason the pointer moves last there: falling backwards is safe. If this
	// dies halfway, the next boot starts the version being rolled back *to*,
	// which is where the operator was heading.
	if version, err = a.Generations.Rollback(); err != nil {
		return "", err
	}
	// Reported as an upgrade, because that is what it is from outside — children
	// restarting onto a different Generation — and the version named is the one
	// being moved to. A phase of its own would be a word for a difference
	// nobody watching can see.
	defer a.Activity.Done()
	if err := a.switchTo(ctx, a.Generations.Dir(previous), previous); err != nil {
		return version, fmt.Errorf("%w: rolled back to %s and it did not come up: %v",
			ErrActivationFailed, previous, err)
	}
	a.Tel.Info(componentGeneration, "rolled back",
		String("version", previous), String("from", version))
	return version, nil
}

// writeEvidence keeps a failed activation's console output beside the
// generations, named for the version and the moment.
func (a *Activator) writeEvidence(version string, output []byte) (string, error) {
	if len(output) == 0 {
		return "", nil
	}
	dir := filepath.Join(a.Generations.root, "failed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.log", version, a.now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, output, 0o600)
}

// ordered returns the targets in the Manager's registration order, so an
// activation restarts children in the order they start and stop in.
func (a *Activator) ordered() []ActivationTarget {
	byName := make(map[string]ActivationTarget, len(a.Targets))
	for _, t := range a.Targets {
		byName[t.Child] = t
	}
	out := make([]ActivationTarget, 0, len(a.Targets))
	for _, name := range a.Manager.Order() {
		if t, ok := byName[name]; ok {
			out = append(out, t)
			delete(byName, name)
		}
	}
	// Anything the Manager does not know keeps its declared order, so a
	// misconfigured target produces its own error rather than vanishing.
	for _, t := range a.Targets {
		if _, unplaced := byName[t.Child]; unplaced {
			out = append(out, t)
			delete(byName, t.Child)
		}
	}
	return out
}

func (a *Activator) readyTimeout() time.Duration {
	if a.ReadyTimeout > 0 {
		return a.ReadyTimeout
	}
	return defaultReadyTimeout
}

func (a *Activator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Activator) log(format string, args ...any) {
	a.Tel.Printf(format, args...)
}
