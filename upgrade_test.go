// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handoffServing stands in for the Platform's private listener, answering
// `/upgrade` with whatever the test wants pending.
func handoffServing(t *testing.T, request UpgradeRequest) Endpoint {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != UpgradeRequestPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(request)
	}))
	t.Cleanup(srv.Close)
	endpoint, err := ParseEndpoint(srv.URL)
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	return endpoint
}

// spoolIn opens a Spool in a temporary directory and returns it with a reader
// for what it recorded.
func spoolIn(t *testing.T) (*Spool, func() []SpooledFinding) {
	t.Helper()
	dir := t.TempDir()
	spool := OpenSpool(dir, nil)
	return spool, func() []SpooledFinding {
		data, err := os.ReadFile(filepath.Join(dir, spoolName))
		if err != nil {
			return nil
		}
		var out []SpooledFinding
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var finding SpooledFinding
			if err := json.Unmarshal([]byte(line), &finding); err != nil {
				t.Fatalf("spooled line is not JSON: %v", err)
			}
			out = append(out, finding)
		}
		return out
	}
}

// The offer reaches a person as a finding rather than through a notification
// channel built for this one feature (supervisor#12, platform#77). The register is
// already the surface for "something needs your attention", and it already folds
// repeats into one Issue with a count.
func TestAnAvailableVersionIsSpooledAsAnOffer(t *testing.T) {
	// Installed on v0.1.0 for real, then the catalogue grows a newer one. The
	// first version has to be genuinely active: a fresh install with nothing
	// active considers everything an upgrade, which would let this pass
	// without checking the comparison it exists to check.
	h := newReleaseHost(t, "v0.1.0")
	updater, _, _ := h.updater(t)
	if _, err := updater.Upgrade(context.Background()); err != nil {
		t.Fatalf("installing the first generation: %v", err)
	}
	h.publish(t, "v0.2.0")
	h.catalogue(t, "v0.1.0", "v0.2.0")

	spool, recorded := spoolIn(t)
	watch := &UpgradeWatch{Updater: updater, Spool: spool}
	watch.checkCatalogue(context.Background())

	findings := recorded()
	if len(findings) != 1 {
		t.Fatalf("want one offer, got %d: %+v", len(findings), findings)
	}
	if findings[0].Type != FindingUpgradeAvailable {
		t.Errorf("type is %q, want %q", findings[0].Type, FindingUpgradeAvailable)
	}
	// The version is the reference, because that is what the person is being
	// offered and what the request will name back.
	if findings[0].Reference != "v0.2.0" {
		t.Errorf("reference is %q, want the offered version", findings[0].Reference)
	}
	if findings[0].Context != ContextGeneration {
		t.Errorf("context is %q", findings[0].Context)
	}
}

// Nothing newer is nothing to say. An install that is current must not
// accumulate a row telling it so.
func TestNothingIsSpooledWhenTheInstallIsCurrent(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	updater, _, _ := h.updater(t)
	if _, err := updater.Upgrade(context.Background()); err != nil {
		t.Fatalf("installing the first generation: %v", err)
	}

	spool, recorded := spoolIn(t)
	(&UpgradeWatch{Updater: updater, Spool: spool}).checkCatalogue(context.Background())

	if findings := recorded(); len(findings) != 0 {
		t.Fatalf("a current install recorded %d findings: %+v", len(findings), findings)
	}
}

// The request comes back on the handoff and is carried out. This is the half
// that was missing: the machinery worked and nothing could ask it to.
func TestARequestOnTheHandoffIsCarriedOut(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0", "v0.2.0")
	updater, generations, _ := h.updater(t)

	spool, recorded := spoolIn(t)
	watch := &UpgradeWatch{
		Updater: updater,
		Handoff: handoffServing(t, UpgradeRequest{
			Pending: true, Version: "v0.2.0", RequestedAt: time.Now(),
		}),
		Spool: spool,
	}
	watch.carryOutRequest(context.Background())

	active, ok := generations.Active()
	if !ok || active != "v0.2.0" {
		t.Fatalf("active generation is %q (%v), want v0.2.0", active, ok)
	}
	// Success is observable rather than reported, so nothing is spooled for it.
	if findings := recorded(); len(findings) != 0 {
		t.Fatalf("a successful upgrade recorded %d findings: %+v", len(findings), findings)
	}
}

// A request naming a version the signed catalogue does not offer is refused,
// and the refusal is recorded. The URL comes from the catalogue entry rather
// than from the request, so no request can point an install at bytes nobody
// signed for.
func TestARequestForAVersionTheCatalogueDoesNotOfferIsRefused(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	updater, generations, _ := h.updater(t)

	spool, recorded := spoolIn(t)
	watch := &UpgradeWatch{
		Updater: updater,
		Handoff: handoffServing(t, UpgradeRequest{Pending: true, Version: "v9.9.9"}),
		Spool:   spool,
	}
	watch.carryOutRequest(context.Background())

	if active, ok := generations.Active(); ok && active == "v9.9.9" {
		t.Fatal("a version the catalogue never offered was activated")
	}
	findings := recorded()
	if len(findings) != 1 || findings[0].Type != FindingUpgradeFailed {
		t.Fatalf("want one upgrade_failed finding, got %+v", findings)
	}
	if findings[0].Reference != "v9.9.9" {
		t.Errorf("reference is %q", findings[0].Reference)
	}
}

// Nothing pending is nothing to do, and a Platform that is down or too old to
// serve the path answers nothing — which is the same as no request.
func TestNoRequestDoesNothing(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0", "v0.2.0")
	updater, generations, _ := h.updater(t)
	before, _ := generations.Active()

	for _, request := range []UpgradeRequest{
		{Pending: false},
		{Pending: true}, // pending with no version names nothing to install
	} {
		watch := &UpgradeWatch{
			Updater: updater,
			Handoff: handoffServing(t, request),
			Spool:   OpenSpool("", nil),
		}
		watch.carryOutRequest(context.Background())
	}

	after, _ := generations.Active()
	if after != before {
		t.Fatalf("the active generation moved from %q to %q with nothing requested", before, after)
	}
}

// An install with no release source has nothing to check and could not carry
// out a request, so the loop returns rather than ticking forever. That is the
// ordinary shape of a deployment managing its own binaries.
func TestTheWatchDoesNothingWithoutAReleaseSource(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&UpgradeWatch{}).Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return for an install with no release source")
	}
}
