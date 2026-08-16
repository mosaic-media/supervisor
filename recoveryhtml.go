// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"fmt"
	"html"
	"sort"
	"strings"

	sdui "github.com/mosaic-media/contracts/sdui"
)

// The recovery UI's renderer: the same SDUI tree the Supervisor emits, as HTML.
//
// There is one renderer, and it is this one. The recovery UI is hypermedia —
// the Supervisor renders, htmx swaps — so the browser holds no component model
// and there is nothing to drift from. A second implementation of the component
// model is what this project has already paid for once.
//
// It is not a general SDUI renderer and must not become one. It draws the
// primitives the Supervisor's own emitter produces (supervisor#6: primitives only),
// which is four of them. The client that renders the whole vocabulary is the
// Shell, and growing this towards that would be building a second Shell inside
// the process whose job is to not need one.

// recoveryGlyphs is this renderer's icon set.
//
// The glyph set is a client asset (the spec says so, which is why iconName is
// open text), and this client has no vector set — so a name is drawn as a
// character. "?" for an unknown one beats drawing nothing, which reads as a
// broken page rather than as a missing glyph.
//
// It is a package-level map so it can be checked against phaseIcon, which is the
// half of the icon-name coupling that lives inside this repository and is
// therefore guardable. Every name phaseIcon can return must have an entry here.
var recoveryGlyphs = map[string]string{
	"refresh": "&#8635;",
	"warning": "!",
	"check":   "&#10003;",
}

// RecoveryFragment renders the state as the HTML fragment htmx swaps in.
//
// A fragment rather than a page: the page is loaded once and this is what
// replaces its content, so the two have different jobs and only this one is
// re-rendered per update.
func RecoveryFragment(s RecoveryState) string {
	var b strings.Builder
	renderNode(&b, RecoveryScreen(s))
	return b.String()
}

// renderNode walks the tree. Unknown primitives render their children rather
// than vanishing — dropping a branch would silently remove whatever it
// contained, which on this surface could be the only sentence explaining what
// is wrong.
func renderNode(b *strings.Builder, n sdui.Node) {
	if n == nil {
		return
	}
	props := n.GetProps().AsMap()

	switch n.GetType() {
	case "Box":
		fmt.Fprintf(b, `<div class="box%s">`, boxClasses(props))
		renderChildren(b, n)
		b.WriteString(`</div>`)
	case "Text":
		text, _ := props["text"].(string)
		fmt.Fprintf(b, `<p class="%s">%s`, textClasses(props), html.EscapeString(text))
		renderChildren(b, n)
		b.WriteString(`</p>`)
	case "Icon":
		name, _ := props["name"].(string)
		glyph := recoveryGlyphs[name]
		if glyph == "" {
			glyph = "?"
		}
		spin := ""
		if name == "refresh" {
			spin = " spin"
		}
		fmt.Fprintf(b, `<div class="icon%s" aria-hidden="true">%s</div>`, spin, glyph)
	case "ProgressBar":
		value, _ := props["value"].(float64)
		tone, _ := props["tone"].(string)
		danger := ""
		if tone == "danger" {
			danger = " danger"
		}
		fmt.Fprintf(b,
			`<div class="bar%s" role="progressbar" aria-valuemin="0" aria-valuemax="1" aria-valuenow="%.4f">`+
				`<i style="width:%.2f%%"></i></div>`,
			danger, value, clampProgress(value)*100)
	default:
		renderChildren(b, n)
	}
}

func renderChildren(b *strings.Builder, n sdui.Node) {
	for _, c := range n.GetChildren() {
		renderNode(b, c)
	}
}

// boxClasses and textClasses translate the style props the emitter sets into
// the small class set the page's stylesheet carries.
//
// A style key this does not know is dropped, not passed through: emitting an
// unknown key as a class would silently produce dead CSS, the same quiet failure
// as a prop nothing renders. The order is deterministic because the output is
// compared byte-for-byte to decide whether to push an SSE event.
func boxClasses(props map[string]any) string {
	style, _ := props["style"].(map[string]any)
	var classes []string
	if style["direction"] == "row" {
		classes = append(classes, "row")
	}
	if style["align"] == "center" {
		classes = append(classes, "center")
	}
	for _, key := range []string{"gap", "p"} {
		if v, ok := style[key].(float64); ok {
			classes = append(classes, fmt.Sprintf("%s-%d", key, int(v)))
		}
	}
	sort.Strings(classes)
	if len(classes) == 0 {
		return ""
	}
	return " " + strings.Join(classes, " ")
}

func textClasses(props map[string]any) string {
	style, _ := props["style"].(map[string]any)
	var classes []string
	for prefix, key := range map[string]string{"v": "variant", "w": "weight", "c": "color"} {
		if v, ok := style[key].(string); ok && v != "" {
			classes = append(classes, prefix+"-"+v)
		}
	}
	sort.Strings(classes)
	return strings.Join(classes, " ")
}

// RecoveryText is the screen's words, in order.
//
// It serves the no-script floor and anything else that needs what this surface
// says without drawing it. It walks the same tree, so it cannot disagree with
// the renderer about what the Supervisor is saying.
func RecoveryText(s RecoveryState) []string {
	var out []string
	var walk func(n sdui.Node)
	walk = func(n sdui.Node) {
		if n == nil {
			return
		}
		if text, ok := n.GetProps().AsMap()["text"].(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
		for _, c := range n.GetChildren() {
			walk(c)
		}
	}
	walk(RecoveryScreen(s))
	return out
}
