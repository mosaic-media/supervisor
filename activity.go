// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"fmt"
	"sync"
)

// What the Supervisor is doing right now, for the screen that says so.
//
// A health report can only distinguish starting, degraded and ready, so the
// front door cannot infer provisioning or upgrading — nothing draws a progress
// bar for them unless something reports them here. The Fetcher and the
// Activator do, and the front door prefers what they say over what it can
// infer. Without that, a first boot downloading two binaries looks exactly like
// a box that will never come up, which is the one distinction this surface
// exists to make.

// Activity is the one operation the Supervisor is performing, if any.
//
// It holds one at a time. Fetching and activating a Generation are steps of a
// single sequence and there is no arrangement in which two run at once — a
// second would be two upgrades racing for the same children — so a report
// replaces whatever came before it rather than joining a set that would then
// need reconciling.
type Activity struct {
	mu      sync.Mutex
	present bool
	state   RecoveryState
}

// Report records what is happening. progress is 0..1 where it can be measured
// and negative where it cannot — the same convention RecoveryState uses, so
// nothing has to translate between two ways of saying "unmeasurable".
func (a *Activity) Report(phase Phase, version, detail string, progress float64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.present = true
	a.state = RecoveryState{Phase: phase, Version: version, Detail: detail, Progress: progress}
}

// Done clears it. It is called when the operation finishes, successfully or
// not: after a failure the honest screen is whatever the children are actually
// doing, which is what the front door infers on its own. A failed download left
// showing "downloading" is a progress bar stuck forever.
func (a *Activity) Done() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.present = false
	a.state = RecoveryState{}
}

// Current returns what is happening, and whether anything is.
func (a *Activity) Current() (RecoveryState, bool) {
	if a == nil {
		return RecoveryState{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, a.present
}

// fetchProgress reports a download as a fraction of the whole release.
//
// It counts artefacts, with the current one's bytes as the fine detail. An
// artefact's size is only knowable from a Content-Length, which is a claim by
// the server, and the download cap is deliberately enforced on the way through
// rather than from that header for exactly that reason. A bar may believe a
// claim where a limit may not: the worst a lying Content-Length does here is
// draw a bar badly, and counting whole artefacts means it still advances
// truthfully even when every claim is absent.
func fetchProgress(index, total int, done, size int64) float64 {
	if total <= 0 {
		return -1
	}
	within := 0.0
	if size > 0 && done >= 0 {
		within = float64(done) / float64(size)
		if within > 1 {
			within = 1
		}
	}
	return (float64(index) + within) / float64(total)
}

// fetchDetail is the sentence beside the bar: which artefact, and where it sits
// in the set. Named rather than counted alone, because "2 of 3" tells somebody
// how long is left and "the Platform" tells them what is happening.
func fetchDetail(name string, index, total int) string {
	return fmt.Sprintf("downloading %s (%d of %d)", name, index+1, total)
}
