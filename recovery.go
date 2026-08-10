// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"strings"

	sdui "github.com/mosaic-media/contracts/sdui"
	"github.com/mosaic-media/contracts/ui"
)

// Recovery SDUI: what the Supervisor has to say about itself, as a tree every
// client can draw.
//
// [supervisor#2](https://github.com/mosaic-media/supervisor/blob/main/docs/adr/0002-supervisor-guarantees-an-interface.md)
// decided the shape and it is one emitter with many renderers. The Shell draws
// this when it is running, a native client draws it in its own skin, and the
// embedded renderer draws it for a browser when there is no Shell yet — which
// is the state a first boot spends its first seconds in, because the Supervisor
// fetches the Shell before the Platform precisely so somebody who opens the URL
// early sees the install happening.
//
// **Primitives only, and no definitions** (supervisor#6). A definition is data the
// *Platform* delivers on connect, and there is no Platform in the states this
// describes — that is the whole reason these screens exist. Shipping a
// definition library inside the Supervisor would put a second copy of the
// composition set in the one binary that must not grow, and it would be a copy
// nothing drift-guards. So: Box, Text, Icon, ProgressBar, Pressable, and
// nothing that has to be looked up.
//
// **It says what is happening and what will happen next, never "please wait".**
// The states here are the ones where a person has no other source of
// information — the Platform cannot answer, so nothing else can tell them
// whether this is a thirty-second install or a box that will never come up.

// Phase is what the Supervisor is doing, as a closed set.
//
// Closed because the emitter branches on it to choose words, which is platform#11's
// test for a closed vocabulary. A phase this build does not know would render
// as nothing, and "nothing" is the one answer these screens exist to prevent.
type Phase string

const (
	// PhaseProvisioning is a first boot: there is no Generation yet and the
	// Supervisor is fetching one.
	PhaseProvisioning Phase = "provisioning"
	// PhaseStarting is a Generation on disk whose children are not serving yet
	// — an ordinary boot, and the state a restart passes through.
	PhaseStarting Phase = "starting"
	// PhaseUpgrading is an activation in progress.
	PhaseUpgrading Phase = "upgrading"
	// PhaseDegraded is a child that is not coming up, with the Supervisor
	// still trying.
	PhaseDegraded Phase = "degraded"
	// PhaseReady is everything serving. It is here so the emitter can answer
	// for a state a client should not be showing — which is more useful than
	// refusing, since a client that asks in the wrong state has a bug worth
	// seeing rather than a blank screen.
	PhaseReady Phase = "ready"
)

// RecoveryState is what the Supervisor knows about itself, flattened for the
// emitter.
//
// A struct rather than the Manager itself, because the emitter must be
// callable from a test without a process tree — and because what a screen shows
// is a *view*, so the place that decides what is worth showing should be
// separable from the place that draws it.
type RecoveryState struct {
	Phase Phase
	// Version is the active Generation, empty on a first boot.
	Version string
	// Detail is the one sentence that varies with what is actually happening —
	// which artefact is downloading, which child will not start.
	Detail string
	// Progress is 0..1 for a phase that has a measurable one, and negative for
	// a phase that does not. Negative rather than zero because zero is a real
	// value: "nothing downloaded yet" and "this cannot be measured" are
	// different, and a bar sitting at empty is a lie about the second.
	Progress float64
	// Children is what each child is doing, for the detail a person reads when
	// the summary is not enough.
	Children []ChildSnapshot
	// BootID stitches this screen to the logs of the same boot (supervisor#5).
	BootID string
}

// RecoveryScreen renders the Supervisor's state as a tree.
//
// One screen with a branch per phase rather than a screen per phase, because
// the states are the same page saying different things — a client re-renders it
// as the Supervisor moves through provisioning, starting and ready, and a
// change of screen would make that a navigation.
func RecoveryScreen(s RecoveryState) sdui.Node { return recoveryScreen(s).Build() }

func recoveryScreen(s RecoveryState) *ui.Element {
	body := []ui.El{
		// **A CSS length, not a size token.** `Icon.size` is `string|number` in
		// the spec and reaches the element unchanged — there is no token scale
		// behind it — so "lg" produced `width="lg"`, which a browser discards.
		// It drew nothing, and nothing said so. Found by rendering this tree in
		// the Shell, which is the whole argument for that rung existing.
		ui.Icon(ui.Name(phaseIcon(s.Phase)), ui.Prop("size", "2rem")),
		heading("Mosaic"),
		line(headline(s), "md", "text-muted"),
	}

	if s.Progress >= 0 {
		body = append(body, ui.ProgressBar(
			// A number, not a string: ProgressBar's `value` is `number` in the
			// spec while the field primitives' `value` is a string, so the
			// typed ui.Value helper is the wrong one here. See the note in
			// recovery_test.go — it is a contract finding, not a local choice.
			ui.Prop("value", clampProgress(s.Progress)),
			ui.StatusTone(phaseTone(s.Phase)),
		))
	}
	if detail := strings.TrimSpace(s.Detail); detail != "" {
		body = append(body, line(detail, "sm", "text-faint"))
	}
	if summary := childrenLine(s.Children); summary != "" {
		body = append(body, line(summary, "sm", "text-faint"))
	}
	// The boot id, so a screenshot of this and a log of the same boot can be
	// put beside each other (supervisor#5). Small and last, because it is for
	// somebody diagnosing rather than for somebody waiting.
	if s.BootID != "" {
		body = append(body, line("boot "+s.BootID, "xs", "text-faint"))
	}

	// **The centring is in the payload rather than in a client.** This tree is
	// the whole page wherever it is drawn — there is no app shell around it,
	// because the thing that emits one is what is missing — so it states that
	// it fills the viewport and centres itself instead of each renderer
	// carrying a stylesheet rule for it. The embedded renderer is the one
	// exception and knows it is one: it drops the style keys it has no class
	// for and centres from the page's own body rule, which is the single place
	// a rule is cheaper than a third of a style vocabulary.
	//
	// **Two boxes, because a box cannot centre itself.** The outer one fills
	// the viewport and centres its child on both axes; the inner one is the
	// column that column has a maximum width. Putting maxWidth on the outer box
	// makes *it* 520 wide and leaves it against the left edge — which is what it
	// did, and which the page's own `margin: 0 auto` hid until the Shell drew
	// the same tree without one.
	return ui.Box(
		ui.Prop("style", map[string]any{
			"direction": "column", "align": "center", "justify": "center",
			"minHeight": "screen", "p": 8,
		}),
		ui.Box(
			ui.Prop("style", map[string]any{
				"direction": "column", "gap": 4, "align": "center", "maxWidth": 520,
			}),
			ui.Group(body...),
		),
	)
}

// heading and line are the two shapes of text this surface uses.
//
// **They exist because the Text primitive's props have no typed helpers.**
// `ui.spec.json` generates a helper for every prop a *definition* binds, and
// `genui -lint` enforces that — but a primitive's own props are authored
// directly, so `text` and `style` are set through the generic ui.Prop, which is
// "a string that compiles whatever you spell". Two local constructors are the
// smallest way to spell each of them once. The Platform reaches for the same
// generic constructor at its own Text call sites, which is what makes this a
// contract gap rather than a Supervisor one.
// Both centre their own text, for the same reason the root Box centres itself:
// a column that centres its children centres each paragraph as a block and
// leaves its lines ragged, and the fix belongs in the payload rather than in
// each client's stylesheet.
func heading(text string) ui.El {
	return ui.Text(ui.Prop("text", text), ui.Prop("style", map[string]any{
		"variant": "2xl", "weight": "bold", "tracking": "tight", "align": "center",
	}))
}

func line(text, variant, color string) ui.El {
	return ui.Text(ui.Prop("text", text), ui.Prop("style", map[string]any{
		"variant": variant, "color": color, "align": "center",
	}))
}

// headline is the sentence a person reads, and it is the whole product of this
// file.
//
// Each says what is happening and what happens next. None says "please wait":
// the Platform cannot answer in these states, so this text is the only source
// of the difference between a thirty-second install and a box that will not
// come up.
func headline(s RecoveryState) string {
	switch s.Phase {
	case PhaseProvisioning:
		return "Setting up for the first time. Mosaic is downloading what it needs, " +
			"and the setup screen appears here as soon as it is ready."
	case PhaseStarting:
		if s.Version != "" {
			return "Starting " + s.Version + ". This page becomes Mosaic once it is serving."
		}
		return "Starting. This page becomes Mosaic once it is serving."
	case PhaseUpgrading:
		return "Installing " + upgradeTarget(s) + ". Mosaic comes back on its own, " +
			"and returns to the version it was running if the new one does not start."
	case PhaseDegraded:
		return "Mosaic is not running, and the Supervisor is still trying to start it. " +
			"Nothing has been lost — this page is here because the part that draws " +
			"Mosaic is the part that is down."
	case PhaseReady:
		return "Mosaic is running."
	default:
		// An unknown phase is a bug in the caller, and saying so is more use
		// than a blank screen — which is the failure this whole surface exists
		// to prevent.
		return "The Supervisor is in a state this page does not have words for " +
			"(" + string(s.Phase) + "). Mosaic is not being served."
	}
}

func upgradeTarget(s RecoveryState) string {
	if strings.TrimSpace(s.Version) == "" {
		return "an update"
	}
	return s.Version
}

// childrenLine summarises what each child is doing, for the person who wants
// more than the headline. Empty when there is nothing to add.
func childrenLine(children []ChildSnapshot) string {
	if len(children) == 0 {
		return ""
	}
	parts := make([]string, 0, len(children))
	for _, c := range children {
		state := string(c.State)
		if c.Unrecoverable {
			// The distinction a person acts on: still starting, or not coming
			// up. A state alone reads the same for both.
			state += ", not coming up"
		}
		parts = append(parts, c.Name+": "+state)
	}
	return strings.Join(parts, " · ")
}

// phaseIcon names a glyph, and **the name is the whole contract** — `iconName`
// is open text in the spec, on the stated grounds that the glyph set is a
// client asset rather than data. So there is nothing to check an emitter
// against: name one a client does not ship and it draws an empty svg, silently.
// This emitter named "alert" for its whole life, which the web client has never
// had (it ships "warning"), and only rendering the tree in the Shell found it.
//
// recoveryGlyphs below is the local half of that coupling, and it is tested.
// The other half — what a *client* ships — is a gap recorded in the roadmap.
func phaseIcon(p Phase) string {
	switch p {
	case PhaseDegraded:
		return "warning"
	case PhaseReady:
		return "check"
	default:
		return "refresh"
	}
}

func phaseTone(p Phase) string {
	if p == PhaseDegraded {
		return sdui.ToneDanger
	}
	return sdui.ToneInfo
}

// clampProgress keeps a bar inside its own bounds. A caller computing a
// fraction from a download can produce something slightly over one, and a bar
// past its end is a rendering bug in whichever client draws it least carefully.
func clampProgress(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
