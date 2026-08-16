// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"strings"
	"testing"

	sdui "github.com/mosaic-media/contracts/sdui"
)

// walk visits every node in a tree, children and slots alike.
func walk(n sdui.Node, visit func(sdui.Node)) {
	if n == nil {
		return
	}
	visit(n)
	for _, c := range n.GetChildren() {
		walk(c, visit)
	}
	for _, slot := range n.GetSlots() {
		for _, c := range slot.GetNodes() {
			walk(c, visit)
		}
	}
}

func treeText(n sdui.Node) string {
	var b strings.Builder
	walk(n, func(node sdui.Node) {
		props := node.GetProps().AsMap()
		if t, ok := props["text"].(string); ok {
			b.WriteString(t)
			b.WriteString("\n")
		}
	})
	return b.String()
}

func everyState() []RecoveryState {
	return []RecoveryState{
		{Phase: PhaseProvisioning, Progress: 0.25, Detail: "downloading the interface", BootID: "abc123"},
		{Phase: PhaseStarting, Version: "v0.2.0", Progress: -1, BootID: "abc123"},
		{Phase: PhaseUpgrading, Version: "v0.3.0", Progress: 0.5},
		{Phase: PhaseDegraded, Version: "v0.2.0", Progress: -1,
			Children: []ChildSnapshot{{Name: "platform", State: ChildStarting, Unrecoverable: true}}},
		{Phase: PhaseReady, Version: "v0.2.0", Progress: -1},
		{Phase: Phase("something-new"), Progress: -1},
	}
}

// TestTheRecoveryScreenUsesOnlyPrimitives is the load-bearing guard. supervisor#6
// says the Supervisor emits primitives and no definitions: a definition is data
// the Platform delivers on connect, and there is no Platform in the states these
// screens describe. A component here would render as a placeholder in every
// client, in exactly the state where a person has nothing else to read.
//
// It is checked against sdui.Primitives — the contract's own generated
// vocabulary — rather than a list maintained here, so a primitive renamed in the
// contract fails this rather than drifting.
func TestTheRecoveryScreenUsesOnlyPrimitives(t *testing.T) {
	allowed := map[string]bool{}
	for _, p := range sdui.Primitives {
		allowed[p.Type] = true
	}
	if len(allowed) == 0 {
		t.Fatal("the contract published no primitives — this guard would pass by finding nothing")
	}

	for _, state := range everyState() {
		walk(RecoveryScreen(state), func(n sdui.Node) {
			if !allowed[n.GetType()] {
				t.Errorf("phase %q emits %q, which is not a primitive — a definition here "+
					"renders as a placeholder, in the one state with nothing else to read",
					state.Phase, n.GetType())
			}
		})
	}
}

// Every prop the emitter sets must be one the primitive declares. This is what
// `ui.Prop` costs: it compiles whatever you spell, so a typo is a prop nothing
// renders — the quiet failure the contract's typed helpers exist to prevent,
// and which primitives do not have helpers for.
func TestEveryPropIsOneThePrimitiveDeclares(t *testing.T) {
	declared := map[string]map[string]bool{}
	for _, p := range sdui.Primitives {
		props := map[string]bool{}
		for _, prop := range p.Props {
			props[prop.Key] = true
		}
		declared[p.Type] = props
	}

	for _, state := range everyState() {
		walk(RecoveryScreen(state), func(n sdui.Node) {
			known, ok := declared[n.GetType()]
			if !ok {
				return // the primitives test above reports this
			}
			for key := range n.GetProps().AsMap() {
				if !known[key] {
					t.Errorf("phase %q sets %q on %s, which declares no such prop — "+
						"it would render as nothing", state.Phase, key, n.GetType())
				}
			}
		})
	}
}

// Every phase says something. A blank screen is the exact failure this surface
// exists to prevent, so the emitter must have words for a phase it does not
// know as well as for the five it does.
func TestEveryPhaseSaysSomething(t *testing.T) {
	for _, state := range everyState() {
		text := treeText(RecoveryScreen(state))
		if strings.TrimSpace(text) == "" {
			t.Errorf("phase %q renders no text at all", state.Phase)
		}
		if !strings.Contains(text, "Mosaic") {
			t.Errorf("phase %q does not name the product: %q", state.Phase, text)
		}
	}
}

// No phase says "please wait". In these states the Platform cannot answer, so
// this text is a person's only source of the difference between a thirty-second
// install and a box that will never come up.
func TestNoPhaseTellsSomebodyToWaitWithoutSayingWhatFor(t *testing.T) {
	for _, state := range everyState() {
		text := strings.ToLower(treeText(RecoveryScreen(state)))
		for _, empty := range []string{"please wait", "loading...", "something went wrong"} {
			if strings.Contains(text, empty) {
				t.Errorf("phase %q says %q, which tells a person nothing", state.Phase, empty)
			}
		}
	}
}

// An unknown phase says so rather than rendering an empty page. A client asking
// in a state this build does not know has a bug worth seeing.
func TestAnUnknownPhaseSaysSoRatherThanRenderingNothing(t *testing.T) {
	text := treeText(RecoveryScreen(RecoveryState{Phase: Phase("teleporting"), Progress: -1}))
	if !strings.Contains(text, "teleporting") {
		t.Errorf("an unknown phase does not name itself: %q", text)
	}
}

// A negative progress means "this cannot be measured", and must not draw a bar
// sitting at empty — which is a lie about a phase with no measurable progress.
func TestAnUnmeasurablePhaseDrawsNoBar(t *testing.T) {
	var bars int
	walk(RecoveryScreen(RecoveryState{Phase: PhaseStarting, Progress: -1}), func(n sdui.Node) {
		if n.GetType() == "ProgressBar" {
			bars++
		}
	})
	if bars != 0 {
		t.Errorf("an unmeasurable phase drew %d progress bars", bars)
	}

	bars = 0
	walk(RecoveryScreen(RecoveryState{Phase: PhaseProvisioning, Progress: 0}), func(n sdui.Node) {
		if n.GetType() == "ProgressBar" {
			bars++
		}
	})
	if bars != 1 {
		t.Errorf("a measurable phase at zero drew %d bars — zero is a value, not an absence", bars)
	}
}

// The bar stays inside its own bounds: a caller computing a fraction from a
// download can produce something slightly over one, and a bar past its end is a
// rendering bug in whichever client draws it least carefully.
func TestProgressIsClamped(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{{-0.0, 0}, {0.5, 0.5}, {1.5, 1}, {2, 1}} {
		var got float64
		walk(RecoveryScreen(RecoveryState{Phase: PhaseUpgrading, Progress: tc.in}), func(n sdui.Node) {
			if n.GetType() == "ProgressBar" {
				if v, ok := n.GetProps().AsMap()["value"].(float64); ok {
					got = v
				}
			}
		})
		if got != tc.want {
			t.Errorf("progress %v rendered as %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A child that is not coming up is distinguished from one still starting. The
// state alone reads the same for both, and they are the two things a person
// does different things about.
func TestAChildThatIsNotComingUpSaysSo(t *testing.T) {
	text := treeText(RecoveryScreen(RecoveryState{
		Phase: PhaseDegraded, Progress: -1,
		Children: []ChildSnapshot{
			{Name: "platform", State: ChildStarting, Unrecoverable: true},
			{Name: "shell", State: ChildReady},
		},
	}))
	if !strings.Contains(text, "platform") || !strings.Contains(text, "not coming up") {
		t.Errorf("a child at its ceiling is not distinguished: %q", text)
	}
	if !strings.Contains(text, "shell") {
		t.Errorf("a healthy child is not reported: %q", text)
	}
}

// The boot id is on the screen, so a screenshot and a log of the same boot can
// be put beside each other (supervisor#5).
func TestTheBootIDIsOnTheScreen(t *testing.T) {
	text := treeText(RecoveryScreen(RecoveryState{Phase: PhaseStarting, Progress: -1, BootID: "d5cb75b5"}))
	if !strings.Contains(text, "d5cb75b5") {
		t.Errorf("the boot id is absent: %q", text)
	}
}
