// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	sdui "github.com/mosaic-media/contracts/sdui"
)

// The embedded renderer is measured against the emitter, not trusted.
//
// This is the web client's `check-vocabulary.mjs` at a hundredth of the size
// and for the same reason: a renderer that has drifted from what it is sent
// looks exactly like one that has not, and on this surface the consequence is a
// missing sentence in the one state with nothing else to read.

// rendererKeys extracts the primitives the embedded renderer implements from
// its RENDERERS object.
//
// Parsing the JavaScript rather than running it, because this module's test
// container has no browser and adding one to gate a 6KB file would be the same
// mistake the file exists to avoid. What it can prove is the pair of sets, and
// that is the drift this guards.
func rendererKeys(t *testing.T) map[string]bool {
	t.Helper()
	start := strings.Index(recoveryPage, "var RENDERERS = {")
	if start < 0 {
		t.Fatal("the renderer no longer declares a RENDERERS object — this guard would pass by finding nothing")
	}
	body := recoveryPage[start:]
	if end := strings.Index(body, "\n  };"); end > 0 {
		body = body[:end]
	}
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s{4}(\w+): function`).FindAllStringSubmatch(body, -1) {
		keys[m[1]] = true
	}
	if len(keys) == 0 {
		t.Fatal("no renderers were found in RENDERERS — the extraction is broken, not the renderer")
	}
	return keys
}

// emitted is every node type the Supervisor's own emitter can produce, across
// every phase it knows.
func emitted(t *testing.T) map[string]bool {
	t.Helper()
	types := map[string]bool{}
	for _, state := range everyState() {
		walk(RecoveryScreen(state), func(n sdui.Node) { types[n.GetType()] = true })
	}
	return types
}

// **The load-bearing guard.** Growing the emitter without growing the renderer
// must fail here rather than on somebody's screen during a first boot.
func TestTheEmbeddedRendererCoversEveryPrimitiveTheEmitterUses(t *testing.T) {
	implemented := rendererKeys(t)
	var missing []string
	for typ := range emitted(t) {
		if !implemented[typ] {
			missing = append(missing, typ)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the emitter produces %v and the embedded renderer implements none of them — "+
			"add them to RENDERERS in recoveryui/index.html", missing)
	}
}

// The reverse direction is a statement rather than an error: the renderer may
// implement a primitive the emitter does not use yet, and that is fine. What is
// not fine is it implementing something the *contract* does not have, which is
// a typo that will never render.
func TestTheEmbeddedRendererImplementsOnlyRealPrimitives(t *testing.T) {
	known := map[string]bool{}
	for _, p := range sdui.Primitives {
		known[p.Type] = true
	}
	for typ := range rendererKeys(t) {
		if !known[typ] {
			t.Errorf("the renderer implements %q, which is not a primitive in the contract", typ)
		}
	}
}

// The renderer carries no dependency, which is the whole of what makes it the
// rung below the Shell. A script or style pulled from anywhere is one more
// thing that has to work on the worst day this install has — and on a first
// boot there may be no route to the internet configured at all.
func TestTheEmbeddedRendererFetchesNothingButItsOwnOrigin(t *testing.T) {
	for _, forbidden := range []string{
		"http://", "https://", "//cdn", "<script src", "<link rel=\"stylesheet\"",
		"import ", "require(",
	} {
		if strings.Contains(recoveryPage, forbidden) {
			t.Errorf("the embedded renderer contains %q — it must have no dependency and no external fetch", forbidden)
		}
	}
	// The one request it makes is to the Supervisor's own path.
	if !strings.Contains(recoveryPage, `fetch("/supervisor/ui"`) {
		t.Error("the renderer does not fetch the Supervisor's own state")
	}
}

// It stays small. Not a style rule: this is the page served when the Shell is
// absent, which on a first boot is before anything has been downloaded — so it
// is the one asset that must arrive over whatever connection the box has.
func TestTheEmbeddedRendererStaysSmall(t *testing.T) {
	const limit = 16 << 10
	if len(recoveryPage) > limit {
		t.Errorf("the embedded renderer is %d bytes, over the %d-byte limit — "+
			"it is the page a first boot loads before anything else exists", len(recoveryPage), limit)
	}
	t.Logf("embedded renderer: %d bytes", len(recoveryPage))
}

// It hands back to the Shell rather than drawing a finished state, and it reads
// the phase as data rather than string-matching the sentences — which would
// make the wording load-bearing.
func TestTheEmbeddedRendererHandsBackWhenReady(t *testing.T) {
	if !strings.Contains(recoveryPage, `envelope.phase === "ready"`) {
		t.Error("the renderer does not act on the phase")
	}
	if !strings.Contains(recoveryPage, "location.reload()") {
		t.Error("the renderer never gets out of the way once the Shell is serving")
	}
}

// Somebody with JavaScript off is told something rather than shown a blank
// page, which is the failure this whole surface exists to prevent.
func TestTheEmbeddedRendererSaysSomethingWithoutJavaScript(t *testing.T) {
	if !strings.Contains(recoveryPage, "<noscript>") {
		t.Error("no noscript fallback — a blank page is what this surface exists to prevent")
	}
}
