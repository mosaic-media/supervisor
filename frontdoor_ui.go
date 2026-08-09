// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Serving the Supervisor's own state (ADR 0005), in the two encodings its two
// kinds of client need.
//
//	/supervisor/ui           JSON — the SDUI tree, for native clients and the Shell
//	/supervisor/ui/fragment  HTML — the same tree rendered, for the recovery UI
//	/supervisor/ui/events    SSE  — a ping when the fragment changes
//
// **One state, one tree, two encodings.** The recovery UI is hypermedia — the
// Supervisor renders and htmx swaps — so it takes HTML; a native client has its
// own renderer and takes the tree. Both come from RecoveryScreen, so they cannot
// disagree about what the Supervisor is saying.
//
// It is deliberately **not** an answer on the Platform's own routes. A client
// asking the Platform for a screen and silently receiving the Supervisor's would
// have no way to tell that what it is drawing is not Mosaic; asking here is
// asking a different question, and every client that renders this knows which
// question it asked.

//go:embed recoveryui/vendor/htmx.min.js recoveryui/vendor/sse.js
var recoveryAssets embed.FS

// RecoveryEnvelope is what /supervisor/ui answers with: the tree to draw, and
// the state as data beside it.
//
// **The phase is a field rather than something to read out of the prose.** A
// renderer has to know when to stop drawing this and hand back to the Shell,
// and the alternative is every client string-matching the sentences — which
// would make the wording load-bearing and untranslatable.
type RecoveryEnvelope struct {
	Phase   Phase           `json:"phase"`
	Version string          `json:"version,omitempty"`
	BootID  string          `json:"bootId,omitempty"`
	UI      json.RawMessage `json:"ui"`
}

// serveUI answers with the SDUI tree and the phase beside it.
func (f *FrontDoor) serveUI(w http.ResponseWriter, r *http.Request) {
	state := f.recoveryState()
	tree, err := RecoveryScreenJSON(state)
	if err != nil {
		http.Error(w, "recovery ui encoding failed", http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(RecoveryEnvelope{
		Phase: state.Phase, Version: state.Version, BootID: state.BootID, UI: tree,
	})
	if err != nil {
		http.Error(w, "recovery ui encoding failed", http.StatusInternalServerError)
		return
	}
	writeNoStore(w, r, "application/json; charset=utf-8", body)
}

// serveUIFragment answers with the rendered HTML htmx swaps in.
func (f *FrontDoor) serveUIFragment(w http.ResponseWriter, r *http.Request) {
	writeNoStore(w, r, "text/html; charset=utf-8", []byte(RecoveryFragment(f.recoveryState())))
}

// serveUIEvents pushes a `state` event whenever the rendered fragment changes,
// and a `ready` event once Mosaic is serving.
//
// **The stream carries a signal, not the content.** htmx re-fetches the
// fragment on the event, so all three rungs of the page — stream, poll, meta
// refresh — share one content path. A stream carrying HTML would be a second
// way for the content to arrive and a second thing to get wrong.
//
// It compares the rendered output rather than watching for changes, which is
// the cheap and honest way round: the Supervisor's state has no change
// notification, and adding one would mean threading a signal through the child
// manager, the fetcher and the activator to save a string comparison in a
// process that is otherwise idle.
func (f *FrontDoor) serveUIEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flushing means no streaming, and a stream nobody can read is worse
		// than none — the page's poll is already the floor beneath this.
		http.Error(w, "streaming unsupported", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// Named because a reverse proxy in front of this — which a homelab is
	// likely to have — buffers by default, and would hold every event until the
	// stream closed: a live page turned dead with no error anywhere.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The client's reconnect delay, in the protocol rather than in the page: an
	// EventSource reconnects on its own, and how soon is the server's business.
	writeEvent(w, flusher, "", "retry: 2000")

	ticker := time.NewTicker(sseInterval)
	defer ticker.Stop()

	var last string
	for {
		state := f.recoveryState()
		if fragment := RecoveryFragment(state); fragment != last {
			last = fragment
			writeEvent(w, flusher, "state", "changed")
		}
		if state.Phase == PhaseReady {
			// Said once, and then the stream ends: the page reloads and the
			// front door hands it the Shell, so holding the connection open
			// would be holding one for a client that has gone.
			writeEvent(w, flusher, "ready", "serving")
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// sseInterval is how often the Supervisor re-renders to see whether anything
// changed. Twice a second: fast enough that a first boot's steps feel
// immediate, and a string comparison in an otherwise idle process.
const sseInterval = 500 * time.Millisecond

func writeEvent(w http.ResponseWriter, flusher http.Flusher, name, data string) {
	if name != "" {
		_, _ = w.Write([]byte("event: " + name + "\n"))
	}
	_, _ = w.Write([]byte("data: " + data + "\n\n"))
	flusher.Flush()
}

// serveRecoveryAsset serves the vendored htmx files from the binary, never from
// a CDN: this page draws when there may be no route to the internet configured
// at all.
func (f *FrontDoor) serveRecoveryAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, recoveryAssetPrefix)
	body, err := recoveryAssets.ReadFile("recoveryui/vendor/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	// Immutable for the life of a binary, and the binary is what changes when
	// they do — so a long cache costs nothing and saves the only requests this
	// page makes that are not about state.
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func writeNoStore(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	// Never cached. This is a state that changes second by second during a
	// first boot, and a cached "downloading the interface" shown after it
	// finished is worse than no page at all.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// recoveryHTML is the page with its initial content rendered in.
//
// **Server-rendered first paint, which is also the no-script floor.** The whole
// content of this page is a state, so an empty box waiting for a fetch would be
// the wrong first frame — and with scripting off it is the only frame, kept
// current by the meta refresh rather than by htmx.
func (f *FrontDoor) recoveryHTML() string {
	return strings.Replace(recoveryPage, "{{fragment}}", RecoveryFragment(f.recoveryState()), 1)
}

// recoveryState turns what the front door knows into what the emitter draws.
//
// **The front door infers the phase rather than being told it**, because the
// thing that knows is the health report it already has: an install whose
// children are not serving is starting, one whose child has passed its ceiling
// is degraded. Provisioning and upgrading are the two it cannot infer — the
// Fetcher and the Activator know and the front door does not — so it reports
// what it can see rather than guessing, and wiring those two in is what turns
// the progress bar from implemented into fed.
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
