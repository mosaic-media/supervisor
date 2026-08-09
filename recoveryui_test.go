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

// rendererKeys is the set of primitives RecoveryFragment actually draws,
// discovered by rendering one and seeing what comes out.
//
// **By behaviour rather than by reading the source**, which is what makes it a
// guard: a `case` that exists and falls through to the default branch is a
// primitive the renderer does not implement, and a source scan would count it.
func rendererKeys(t *testing.T) map[string]bool {
	t.Helper()
	drawn := map[string]bool{}
	// Each primitive the emitter can use, alone, so what appears in the output
	// is attributable to it.
	probes := map[string]RecoveryState{
		"Box":         {Phase: PhaseStarting, Progress: -1},
		"Text":        {Phase: PhaseStarting, Progress: -1},
		"Icon":        {Phase: PhaseStarting, Progress: -1},
		"ProgressBar": {Phase: PhaseProvisioning, Progress: 0.5},
	}
	marks := map[string]string{
		"Box": `<div class="box`, "Text": "<p class=", "Icon": `<div class="icon`,
		"ProgressBar": `role="progressbar"`,
	}
	for typ, state := range probes {
		if strings.Contains(RecoveryFragment(state), marks[typ]) {
			drawn[typ] = true
		}
	}
	if len(drawn) == 0 {
		t.Fatal("the renderer drew none of the primitives — the probe is broken, not the renderer")
	}
	return drawn
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
		t.Errorf("the emitter produces %v and the recovery renderer draws none of them — "+
			"add them to renderNode in recoveryhtml.go", missing)
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
	for _, forbidden := range []string{"http://", "https://", "//cdn", "//unpkg"} {
		if strings.Contains(recoveryPage, forbidden) {
			t.Errorf("the recovery page contains %q — every asset is vendored into the binary, "+
				"because this draws when there may be no route to the internet at all", forbidden)
		}
	}
	// Every script it loads is one the binary carries.
	for _, m := range regexp.MustCompile(`<script src="([^"]+)"`).FindAllStringSubmatch(recoveryPage, -1) {
		src := m[1]
		if !strings.HasPrefix(src, recoveryAssetPrefix) {
			t.Errorf("script %q is not served from the binary", src)
			continue
		}
		name := strings.TrimPrefix(src, recoveryAssetPrefix)
		if _, err := recoveryAssets.ReadFile("recoveryui/vendor/" + name); err != nil {
			t.Errorf("script %q is referenced and not embedded: %v", src, err)
		}
	}
	// And the endpoints it drives are the Supervisor's own.
	for _, want := range []string{supervisorUIFragmentPath, supervisorUIEventsPath} {
		if !strings.Contains(recoveryPage, want) {
			t.Errorf("the page does not use %s", want)
		}
	}
}

// It stays small. Not a style rule: this is the page served when the Shell is
// absent, which on a first boot is before anything has been downloaded — so it
// is the one asset that must arrive over whatever connection the box has.
func TestTheEmbeddedRendererStaysSmall(t *testing.T) {
	// The page itself, and everything it pulls from the binary. htmx and its
	// SSE extension are most of it, and the budget is set where it is so that
	// adding a *second* library is a decision rather than a slide.
	total := len(recoveryPage)
	for _, name := range []string{"htmx.min.js", "sse.js"} {
		b, err := recoveryAssets.ReadFile("recoveryui/vendor/" + name)
		if err != nil {
			t.Fatalf("%s is not embedded: %v", name, err)
		}
		total += len(b)
	}
	const limit = 96 << 10
	if total > limit {
		t.Errorf("the recovery UI is %d bytes, over the %d-byte budget — "+
			"it is what a first boot loads before anything else exists", total, limit)
	}
	t.Logf("recovery UI: page %d bytes + vendored assets = %d bytes total", len(recoveryPage), total)
}

// It hands back to the Shell rather than drawing a finished state, and it reads
// the phase as data rather than string-matching the sentences — which would
// make the wording load-bearing.
func TestTheEmbeddedRendererHandsBackWhenReady(t *testing.T) {
	if !strings.Contains(recoveryPage, `"sse:ready"`) {
		t.Error("the page does not act on the ready event")
	}
	if !strings.Contains(recoveryPage, "location.reload()") {
		t.Error("the page never gets out of the way once the Shell is serving")
	}
}

// **Polling is the floor beneath the stream, and a meta refresh is the floor
// beneath that.** SSE is the thing most likely to be silently broken — a proxy
// that buffers turns a live page into a dead one with no error — so the page
// must not depend on it, and with scripting off it must still keep up.
func TestTheRecoveryPageHasAFloorBeneathTheStream(t *testing.T) {
	if !strings.Contains(recoveryPage, "every 5s") {
		t.Error("no polling trigger — a blocked SSE stream would freeze the page")
	}
	if !strings.Contains(recoveryPage, `<noscript><meta http-equiv="refresh"`) {
		t.Error("no meta refresh — with scripting off the page would show one frame forever")
	}
	// The fragment arrives by hx-get on every rung, so the three share one
	// content path rather than being three ways for content to arrive.
	if strings.Contains(recoveryPage, "sse-swap") {
		t.Error("the stream carries content — it must carry a signal, so a buffered stream " +
			"costs latency rather than the page")
	}
}

// Somebody with JavaScript off is told something rather than shown a blank
// page, which is the failure this whole surface exists to prevent.
func TestTheEmbeddedRendererSaysSomethingWithoutJavaScript(t *testing.T) {
	if !strings.Contains(recoveryPage, "{{fragment}}") {
		t.Error("the page has no server-rendered first paint — with scripting off it would be empty, " +
			"and with scripting on the first frame would be an empty box waiting for a fetch")
	}
}
