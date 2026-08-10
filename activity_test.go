// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The progress bar was implemented on three renderers and fed by nothing, so
// the tests worth having are about what reaches the screen — not about the
// arithmetic, which is the part that was never the problem.

// **What is being done on purpose beats what can be observed.** During an
// upgrade the children are down, so the health inference would call it
// degraded — true, and the wrong thing to tell somebody: "not coming up" and
// "coming back in a moment" are the two states this screen exists to separate.
func TestAnUpgradeIsNotReportedAsDegraded(t *testing.T) {
	activity := &Activity{}
	front := frontDoor(t, "http://127.0.0.1:1", "http://127.0.0.1:1", func() Health {
		return Health{Children: []ChildSnapshot{
			{Name: PlatformChildName, State: ChildStarting, Unrecoverable: true},
		}}
	})
	front.Activity = activity

	// With nothing being done, the inference stands and says the worst.
	if got := front.recoveryState().Phase; got != PhaseDegraded {
		t.Fatalf("phase = %q with nothing in progress, want degraded", got)
	}

	activity.Report(PhaseUpgrading, "v0.4.0", "restarting platform (1 of 2)", 0.5)
	state := front.recoveryState()
	if state.Phase != PhaseUpgrading {
		t.Errorf("phase = %q during an upgrade, want upgrading", state.Phase)
	}
	if state.Progress != 0.5 {
		t.Errorf("progress = %v, want the reported fraction", state.Progress)
	}
	// The children still travel with it: an upgrade is the moment somebody most
	// wants to know which half is back.
	if len(state.Children) != 1 {
		t.Error("the children were dropped from a reported state")
	}
	// And the boot id, which is what joins a screenshot to a log (supervisor#5).
	if state.BootID == "" {
		t.Error("the boot id is missing from a reported state")
	}

	activity.Done()
	if got := front.recoveryState().Phase; got != PhaseDegraded {
		t.Errorf("phase = %q after the operation ended, want the inference back", got)
	}
}

// **A bar is actually drawn now, in every renderer.** This is the end of the
// chain the gap was in: something reports, the emitter branches on a measurable
// progress, and the renderers draw it.
func TestAReportedFetchDrawsAProgressBar(t *testing.T) {
	activity := &Activity{}
	front := frontDoor(t, "http://127.0.0.1:1", "http://127.0.0.1:1", func() Health { return Health{} })
	front.Activity = activity

	rec := httptest.NewRecorder()
	front.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, supervisorUIFragmentPath, nil))
	if strings.Contains(rec.Body.String(), `role="progressbar"`) {
		t.Fatal("a bar was drawn with nothing in progress — an unmeasurable phase must draw none")
	}

	activity.Report(PhaseProvisioning, "v0.4.0", "downloading mosaic-platform (1 of 2)", 0.25)
	rec = httptest.NewRecorder()
	front.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, supervisorUIFragmentPath, nil))
	body := rec.Body.String()
	if !strings.Contains(body, `role="progressbar"`) {
		t.Error("no bar was drawn for a measurable phase")
	}
	if !strings.Contains(body, "downloading mosaic-platform (1 of 2)") {
		t.Errorf("the detail did not reach the screen: %s", body)
	}
	if !strings.Contains(body, "Setting up for the first time") {
		t.Errorf("provisioning does not read as a first install: %s", body)
	}
}

// Counting in artefacts is what makes the bar advance truthfully when a server
// gives no Content-Length — which it need not, and which the download cap
// deliberately does not trust either.
func TestProgressAdvancesWithoutAContentLength(t *testing.T) {
	for _, tc := range []struct {
		name         string
		index, total int
		done, size   int64
		want         float64
	}{
		{"first artefact, no size claimed", 0, 2, 5000, 0, 0},
		{"second artefact, no size claimed", 1, 2, 5000, 0, 0.5},
		{"halfway through the first", 0, 2, 50, 100, 0.25},
		{"a size claim smaller than the bytes", 0, 2, 300, 100, 0.5},
		{"nothing to fetch", 0, 0, 0, 0, -1},
	} {
		if got := fetchProgress(tc.index, tc.total, tc.done, tc.size); got != tc.want {
			t.Errorf("%s: progress = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A nil Activity is a fetch nobody is watching — a test, or a path with no
// front door. Reporting into nothing must be a no-op rather than a panic in the
// process whose job is to stay up.
func TestReportingIntoNothingIsSafe(t *testing.T) {
	var activity *Activity
	activity.Report(PhaseUpgrading, "v1", "x", 0.5)
	activity.Done()
	if _, ok := activity.Current(); ok {
		t.Error("a nil Activity reported something")
	}
}
