// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"embed"
	"html"
	"net/http"
	"strings"
	"time"
)

// The embedded renderer's two endpoints (supervisor#2's third rung).
//
//	/supervisor/ui/fragment  HTML — the Supervisor's screen, rendered
//	/supervisor/ui/events    SSE  — a ping when that rendering changes
//
// **These serve the recovery page and nothing else.** A client with its own
// renderer does not come here at all: it calls the Platform's routes, and while
// the Platform is not serving the Supervisor answers them itself (supervisor#7).
// There was a `/supervisor/ui` here answering the tree as JSON for exactly that
// audience, and removing it is the point rather than a tidy-up — an endpoint a
// client has to know to ask is a rule every client has to implement.
//
// Everything still comes from RecoveryScreen, so the page, a native client and
// the Shell cannot disagree about what the Supervisor is saying.

//go:embed recoveryui/vendor/htmx.min.js recoveryui/vendor/sse.js
var recoveryAssets embed.FS

// serveUIFragment answers with the rendered HTML htmx swaps in, wrapped in an
// element carrying the phase as data.
//
// **The phase has to travel with the fragment**, because the page has to know
// when to get out of the way and the only alternative is reading the prose —
// which would make the wording load-bearing and untranslatable. It rides a data
// attribute rather than a header so it survives the swap and can be read from
// the DOM afterwards, on whichever rung delivered it.
func (f *FrontDoor) serveUIFragment(w http.ResponseWriter, r *http.Request) {
	state := f.recoveryState()
	body := `<div data-phase="` + html.EscapeString(string(state.Phase)) + `">` +
		RecoveryFragment(state) + `</div>`
	writeNoStore(w, r, "text/html; charset=utf-8", []byte(body))
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
// **Two sources, and the deliberate one wins.** An operation the Supervisor is
// performing on itself — fetching a Generation, switching onto one — is
// reported by whichever component is doing it, because only that component
// knows. Everything else is inferred from the health report the front door
// already holds: children that are not serving are starting, a child past its
// restart ceiling is degraded.
//
// The order matters. During an upgrade the children *are* down, so the
// inference would call it degraded — technically true and the wrong thing to
// tell somebody, because "not coming up" and "coming back in a moment" are the
// two states this screen exists to separate. What is being done on purpose is
// therefore preferred over what can be observed.
func (f *FrontDoor) recoveryState() RecoveryState {
	state := RecoveryState{
		Phase:    PhaseStarting,
		Progress: -1,
		BootID:   f.cfg.BootID,
	}
	if doing, ok := f.Activity.Current(); ok {
		doing.BootID = state.BootID
		if f.health != nil {
			// The children still travel with it: an upgrade is the moment
			// somebody most wants to know which half is back.
			doing.Children = f.health().Children
		}
		return doing
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
