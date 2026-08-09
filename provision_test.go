// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The composition that gave the update machinery a caller. What is worth
// testing is the decisions it makes on boot, because each of them is a way an
// install can come up wrong and none of them fails loudly.

// provisioner builds one over a manager holding the given children. A managed
// child with no command is a first boot waiting for its binary; one with a
// command is a deployment that supplied its own.
func provisioner(t *testing.T, cfg Config, children ...ChildSpec) *Provisioner {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	manager := NewManager("boot-1", t.Logf)
	for _, spec := range children {
		if err := manager.Add(spec); err != nil {
			t.Fatalf("Add(%s): %v", spec.Name, err)
		}
	}
	p, err := OpenProvisioner(cfg, manager, &Activity{}, OpenSpool(t.TempDir(), t.Logf), t.Logf)
	if err != nil {
		t.Fatalf("OpenProvisioner: %v", err)
	}
	return p
}

// awaiting is a child the Supervisor owns with nothing yet to run.
func awaiting(name string) ChildSpec { return ChildSpec{Name: name, Managed: true} }

// **An install with a Generation boots onto it and is left alone.** Fetching on
// every boot would turn a restart into an upgrade — a machine that came back on
// a different version because it was power-cycled, which is the behaviour
// nobody asks for and everybody notices.
func TestAnExistingGenerationIsNotReplacedOnBoot(t *testing.T) {
	root := t.TempDir()
	p := provisioner(t, Config{StateDir: root, ReleaseURL: ""})

	dir, err := p.Generations.Stage("v0.3.0")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	writeExecutables(t, dir)
	if err := p.Generations.Commit("v0.3.0"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := p.Generations.Activate("v0.3.0"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if err := p.EnsureGeneration(context.Background()); err != nil {
		t.Errorf("an install with a generation refused to boot: %v", err)
	}
	if version, _ := p.Generations.Active(); version != "v0.3.0" {
		t.Errorf("active generation is %q — a boot changed it", version)
	}
}

// Nothing on disk and nowhere to fetch from is reported rather than waited
// through. The two look identical from outside — a box showing "starting"
// forever — and only one of them is something a person can act on.
func TestNothingOnDiskAndNoCatalogueIsAnError(t *testing.T) {
	p := provisioner(t, Config{ReleaseURL: ""}, awaiting(PlatformChildName))
	err := p.EnsureGeneration(context.Background())
	if !errors.Is(err, ErrNoReleaseSource) {
		t.Fatalf("err = %v, want ErrNoReleaseSource", err)
	}
	// It names who is stuck. "Something is misconfigured" is not actionable;
	// "the platform has nothing to run" is.
	if !strings.Contains(err.Error(), PlatformChildName) {
		t.Errorf("the error does not name the child that is waiting: %v", err)
	}
}

// **And says nothing when nothing needs one.** This is the ordinary shape of a
// deployment that runs its own processes, and it is where the first version got
// it wrong: it reported a missing release catalogue on every start of the dev
// stack, where both children were supplied and running perfectly. An alarming
// line printed when nothing is wrong is how a log stops being read.
func TestAnUnusedCatalogueIsNotReportedAsMissing(t *testing.T) {
	p := provisioner(t, Config{ReleaseURL: ""},
		ChildSpec{Name: PlatformChildName, Command: []string{"/bin/true"}, Managed: true},
		ChildSpec{Name: ShellChildName, Command: []string{"/bin/true"}, Managed: true},
	)
	if err := p.EnsureGeneration(context.Background()); err != nil {
		t.Errorf("a deployment running its own binaries was told off about a catalogue "+
			"it does not use: %v", err)
	}
}

// An externally managed child is not waiting for anything either — the
// Supervisor fronts it and something else runs it (ADR 0121's DIY path).
func TestAnExternallyManagedChildIsNotWaitingForABinary(t *testing.T) {
	p := provisioner(t, Config{ReleaseURL: ""}, ChildSpec{Name: PlatformChildName})
	if err := p.EnsureGeneration(context.Background()); err != nil {
		t.Errorf("a fronted-but-not-owned child was treated as one waiting for a binary: %v", err)
	}
}

// **A build with no trusted key still starts.** This is the shipped state
// today — no release key exists yet (ADR 0122) — and an install with binaries
// on disk works fine without one. Refusing to construct would refuse to boot.
func TestABuildWithNoReleaseKeyStillStarts(t *testing.T) {
	p := provisioner(t, Config{ReleaseURL: "https://example.invalid/releases"})
	if p == nil {
		t.Fatal("a build with no release key produced no provisioner at all")
	}
	if p.Updater != nil {
		t.Error("an updater was built with no key to verify anything with — " +
			"it would fail at the moment somebody needed it rather than at boot")
	}
}

// **The environment wins over the Generation.** Both cases that set it — a dev
// stack pointing at binaries it built, and a deployment managing its own
// processes — are deliberate acts by somebody who knows what they want run.
func TestTheActiveGenerationSuppliesTheChildCommands(t *testing.T) {
	root := t.TempDir()
	p := provisioner(t, Config{StateDir: root})

	// Nothing active yet: a first boot has no command to give, and the
	// Activator sets one when a Generation arrives.
	if argv := p.CommandFor(PlatformChildName); argv != nil {
		t.Errorf("CommandFor before any generation = %v, want nothing to run", argv)
	}

	dir, err := p.Generations.Stage("v0.4.0")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	writeExecutables(t, dir)
	if err := p.Generations.Commit("v0.4.0"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := p.Generations.Activate("v0.4.0"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	argv := p.CommandFor(PlatformChildName)
	if len(argv) == 0 || argv[0] != filepath.Join(dir, "mosaic-platform") {
		t.Errorf("CommandFor(platform) = %v, want the binary in the active generation", argv)
	}
	if argv := p.CommandFor("nothing-named-this"); argv != nil {
		t.Errorf("CommandFor(unknown) = %v, want nothing", argv)
	}
}

// The name a target is fetched under is derived, never listed twice. A second
// list is what this test replaced: it agreed with itself and disagreed with the
// release, which a fetch reports as a 404 and nothing else reports at all.
func TestEveryTargetIsFetchedUnderItsReleaseName(t *testing.T) {
	for _, target := range ProvisionTargets {
		name := ReleaseArtefactName(target.Binary)
		if name == target.Binary {
			t.Errorf("%s is fetched under its own name — the per-host suffix is missing, "+
				"so a release built for three platforms would offer one file for all of them",
				target.Binary)
		}
		if !strings.HasPrefix(name, target.Binary) {
			t.Errorf("%s is fetched as %q, which does not name it", target.Binary, name)
		}
	}
}

func writeExecutables(t *testing.T, dir string) {
	t.Helper()
	for _, target := range ProvisionTargets {
		path := filepath.Join(dir, target.Binary)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
}
