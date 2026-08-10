// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What matters here is that recording never becomes a reason not to run: the
// spool exists for the boot where something has already gone wrong.

func TestAFindingSurvivesTheProcessThatMadeIt(t *testing.T) {
	dir := t.TempDir()
	spool := OpenSpool(dir, nil)

	spool.Record(FindingChildUnrecoverable, ContextChild, "platform", "exit status 1")
	spool.Record(FindingGenerationRolledBack, ContextGeneration, "v0.4.0", "did not come up")

	body, err := os.ReadFile(filepath.Join(dir, spoolName))
	if err != nil {
		t.Fatalf("reading the spool: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want one per finding: %q", len(lines), body)
	}
	// One JSON object per line, so a reader can skip a bad one without losing
	// the rest — which is the whole shape of the format.
	for i, line := range lines {
		var f SpooledFinding
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Errorf("line %d is not readable on its own: %v", i, err)
		}
		if f.At.IsZero() {
			t.Errorf("line %d carries no time, so the Platform cannot say when it started", i)
		}
	}
}

// **Nothing here is fatal, and nothing panics.** A Supervisor that failed to
// start a child, and then failed *because* it could not write that down, would
// be the machinery defeating the thing it is for.
func TestRecordingIntoNowhereIsSafe(t *testing.T) {
	var absent *Spool
	absent.Record(FindingProvisionFailed, ContextHost, "", "no catalogue")
	if absent.Path() != "" {
		t.Error("a nil spool claims a path")
	}

	// Configured with nowhere to write.
	unset := OpenSpool("", nil)
	unset.Record(FindingProvisionFailed, ContextHost, "", "no catalogue")

	// Configured at a path that cannot be written.
	unwritable := OpenSpool(filepath.Join(t.TempDir(), "no", "such", "dir"), nil)
	unwritable.Record(FindingProvisionFailed, ContextHost, "", "no catalogue")
}

// The finding types this Supervisor writes are ones the Platform's closed
// vocabulary knows. It cannot import that vocabulary — the boundary is two
// modules wide and the Platform is not one of them — so this is the seam where
// the two are stated to agree, and it is checked against the register's own
// documented set rather than against nothing.
func TestTheSpooledTypesAreTheOnesThePlatformKnows(t *testing.T) {
	// Mirrors domain.KnownIssueTypes in the Platform (platform#74). A type added
	// here and not there is adopted and skipped, silently.
	known := map[string]bool{
		"extension_unavailable":  true,
		"child_unrecoverable":    true,
		"generation_rolled_back": true,
		"provision_failed":       true,
	}
	for _, written := range []string{
		FindingChildUnrecoverable, FindingGenerationRolledBack, FindingProvisionFailed,
	} {
		if !known[written] {
			t.Errorf("the Supervisor records %q and the Platform's register has no such type, "+
				"so the finding is adopted and dropped with nothing reported", written)
		}
	}
}
