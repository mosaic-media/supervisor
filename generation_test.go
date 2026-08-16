// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func generations(t *testing.T) *Generations {
	t.Helper()
	g, err := OpenGenerations(t.TempDir())
	if err != nil {
		t.Fatalf("OpenGenerations: %v", err)
	}
	return g
}

// complete stages a version, puts a file in it and marks it done — a Generation
// as Fetch would leave one.
func complete(t *testing.T, g *Generations, version string) {
	t.Helper()
	dir, err := g.Stage(version)
	if err != nil {
		t.Fatalf("Stage(%s): %v", version, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mosaic-platform"), []byte(version), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := g.Commit(version); err != nil {
		t.Fatalf("Commit(%s): %v", version, err)
	}
}

// A fresh install has no active Generation, which is a state and not an error:
// it is what a first boot downloads into.
func TestAFreshInstallHasNoActiveGeneration(t *testing.T) {
	g := generations(t)
	if v, ok := g.Active(); ok {
		t.Errorf("a fresh install reports %q active", v)
	}
	if _, ok := g.Previous(); ok {
		t.Error("a fresh install reports a rollback target")
	}
	if _, err := g.Rollback(); !errors.Is(err, ErrNoPrevious) {
		t.Errorf("Rollback on a fresh install: %v, want ErrNoPrevious", err)
	}
}

// TestAnIncompleteGenerationCannotBeActivated pins the load-bearing refusal. A
// download interrupted halfway leaves a directory of plausible binaries, and the
// marker is the only thing that distinguishes it from a good Generation.
// Activating one would exec a partially-downloaded Platform.
func TestAnIncompleteGenerationCannotBeActivated(t *testing.T) {
	g := generations(t)
	if _, err := g.Stage("v0.1.0"); err != nil {
		t.Fatal(err)
	}
	// Staged, files written, and the process died before the last one verified.
	if err := os.WriteFile(filepath.Join(g.Dir("v0.1.0"), "mosaic-platform"), []byte("half"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := g.Activate("v0.1.0"); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Activate on an incomplete generation: %v, want ErrIncomplete", err)
	}
	if _, ok := g.Active(); ok {
		t.Error("an incomplete generation became active")
	}
}

// Activation records what it replaced, which is the whole of the rollback
// target.
func TestActivationKeepsThePredecessorAsTheRollbackTarget(t *testing.T) {
	g := generations(t)
	complete(t, g, "v0.1.0")
	complete(t, g, "v0.2.0")

	if err := g.Activate("v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Previous(); ok {
		t.Error("the first activation invented a rollback target")
	}

	if err := g.Activate("v0.2.0"); err != nil {
		t.Fatal(err)
	}
	if v, _ := g.Active(); v != "v0.2.0" {
		t.Errorf("active = %q", v)
	}
	if v, _ := g.Previous(); v != "v0.1.0" {
		t.Errorf("previous = %q", v)
	}
}

// A rollback swaps rather than drops. "The upgrade broke and so did the
// rollback" is exactly when somebody needs the option of going back to where
// they started, so the version rolled away from stays reachable.
func TestARollbackSwapsSoItCanBeUndone(t *testing.T) {
	g := generations(t)
	complete(t, g, "v0.1.0")
	complete(t, g, "v0.2.0")
	if err := g.Activate("v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := g.Activate("v0.2.0"); err != nil {
		t.Fatal(err)
	}

	back, err := g.Rollback()
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back != "v0.1.0" {
		t.Fatalf("rolled back to %q", back)
	}
	if v, _ := g.Active(); v != "v0.1.0" {
		t.Errorf("active = %q after rollback", v)
	}
	if v, _ := g.Previous(); v != "v0.2.0" {
		t.Errorf("previous = %q — the version rolled away from must stay reachable", v)
	}
}

// A recorded predecessor whose directory has gone is not a rollback target.
// Pointing at it would make the next start fail to exec, at a moment chosen by
// the failure that prompted the rollback.
func TestARollbackToAMissingGenerationIsRefused(t *testing.T) {
	g := generations(t)
	complete(t, g, "v0.1.0")
	complete(t, g, "v0.2.0")
	if err := g.Activate("v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := g.Activate("v0.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := g.Discard("v0.1.0"); err != nil {
		t.Fatal(err)
	}

	if _, err := g.Rollback(); !errors.Is(err, ErrNoPrevious) {
		t.Errorf("Rollback to a discarded generation: %v, want ErrNoPrevious", err)
	}
	if v, _ := g.Active(); v != "v0.2.0" {
		t.Errorf("a refused rollback changed the active generation to %q", v)
	}
}

// Restaging discards the previous attempt, so a retry cannot merge two
// downloads into one directory whose set was never checked as a whole.
func TestRestagingStartsEmpty(t *testing.T) {
	g := generations(t)
	dir, err := g.Stage("v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leftover"), []byte("from a failed attempt"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := g.Stage("v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "leftover")); !os.IsNotExist(err) {
		t.Error("a re-staged generation kept the previous attempt's files")
	}
	if g.Complete("v0.1.0") {
		t.Error("a re-staged generation is still marked complete")
	}
}

// The active Generation cannot be deleted out from under the processes running
// it.
func TestTheActiveGenerationCannotBeDiscarded(t *testing.T) {
	g := generations(t)
	complete(t, g, "v0.1.0")
	if err := g.Activate("v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := g.Discard("v0.1.0"); err == nil {
		t.Error("the active generation was discarded")
	}
}

// A version reaches this from a release catalogue — remote input — and is used
// to build a path. A traversal ends with the Supervisor executing a file
// somebody else chose.
func TestAVersionCannotEscapeTheGenerationsDirectory(t *testing.T) {
	g := generations(t)
	for _, bad := range []string{"..", ".", "../evil", "a/b", `a\b`, "", "   ", ".hidden"} {
		if _, err := g.Stage(bad); err == nil {
			t.Errorf("Stage(%q) was accepted", bad)
		}
		if err := g.Activate(bad); err == nil {
			t.Errorf("Activate(%q) was accepted", bad)
		}
		if err := g.Discard(bad); err == nil {
			t.Errorf("Discard(%q) was accepted", bad)
		}
	}
}

// An unreadable pointer reports "nothing active" rather than guessing, because
// a Supervisor that cannot say what should be running must download rather than
// exec a path it invented.
func TestACorruptPointerReadsAsNothingActive(t *testing.T) {
	g := generations(t)
	complete(t, g, "v0.1.0")
	if err := g.Activate("v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.pointerPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if v, ok := g.Active(); ok {
		t.Errorf("a corrupt pointer reported %q active", v)
	}
}

// Activating what is already active is a no-op rather than a self-rollback —
// otherwise a re-activation would record the live version as its own
// predecessor and destroy the rollback target.
func TestActivatingTheLiveGenerationIsANoOp(t *testing.T) {
	g := generations(t)
	complete(t, g, "v0.1.0")
	complete(t, g, "v0.2.0")
	if err := g.Activate("v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := g.Activate("v0.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := g.Activate("v0.2.0"); err != nil {
		t.Fatal(err)
	}

	if v, _ := g.Previous(); v != "v0.1.0" {
		t.Errorf("previous = %q — re-activating destroyed the rollback target", v)
	}
}

func TestListNamesWhatIsOnDisk(t *testing.T) {
	g := generations(t)
	complete(t, g, "v0.1.0")
	complete(t, g, "v0.2.0")

	got, err := g.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0] != "v0.1.0" || got[1] != "v0.2.0" {
		t.Errorf("List = %v", got)
	}
}
