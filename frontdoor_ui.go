// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import "net/http"

// Serving the Supervisor's Recovery SDUI (ADR 0005).
//
// **JSON rather than protobuf on the wire, and that is the whole reason this
// is a plain endpoint rather than a Connect service.** The clients that need
// this most are the ones with the least available: a browser with no Shell
// yet, and the embedded renderer that has to work when everything that would
// make it nicer is the thing that is broken. `protojson` over `GET` is
// readable with `curl`, needs no generated client, and costs nothing the
// Supervisor was not already carrying.
//
// It is deliberately **not** an answer on the Platform's own routes. A client
// asking the Platform for a screen and silently receiving the Supervisor's
// would have no way to tell that what it is drawing is not Mosaic; asking here
// is asking a different question, and every client that renders this knows
// which question it asked.

// serveUI answers with the Supervisor's current state as an SDUI tree.
func (f *FrontDoor) serveUI(w http.ResponseWriter, r *http.Request) {
	body, err := RecoveryScreenJSON(f.recoveryState())
	if err != nil {
		http.Error(w, "recovery ui encoding failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Never cached. This is a state that changes second by second during a
	// first boot, and a cached copy of "downloading the interface" shown after
	// it finished is worse than no page at all.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// recoveryState turns what the front door knows into what the emitter draws.
//
// **The front door infers the phase rather than being told it**, because the
// thing that knows is the health report it already has: an install with no
// children serving is starting, one whose child has passed its ceiling is
// degraded. An activation in progress is the one phase it cannot infer — the
// Activator knows and the front door does not — and rather than guess, this
// reports what it can see. Wiring the Activator's phase in is the next step and
// is deliberately not faked here.
func (f *FrontDoor) recoveryState() RecoveryState {
	state := RecoveryState{
		Phase:    PhaseStarting,
		Progress: -1,
		BootID:   f.cfg.BootID,
	}
	if f.health == nil {
		return state
	}
	reported := f.health()
	state.Children = reported.Children

	ready, degraded := 0, false
	for _, c := range reported.Children {
		if c.State == ChildReady {
			ready++
		}
		if c.Unrecoverable {
			degraded = true
		}
	}
	switch {
	case degraded:
		state.Phase = PhaseDegraded
	case len(reported.Children) > 0 && ready == len(reported.Children):
		state.Phase = PhaseReady
	}
	return state
}
