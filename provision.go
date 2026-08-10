// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Giving the update machinery a caller.
//
// [Fetcher.Fetch], [Activator.Activate] and [Updater.Upgrade] were built,
// tested and reachable from Go and from nowhere else — no code path in the
// binary constructed any of them. So an install could not provision itself, the
// `provisioning` and `upgrading` phases were never reported by anything, and the
// progress bar every renderer draws had nothing to draw.
//
// This is the composition that closes that: a Supervisor that boots onto the
// Generation it already has, and fetches one when it has none.
//
// **What it deliberately does not add is a schedule.** Nothing polls the
// catalogue and nothing upgrades an install that is already running. Whether an
// install updates itself unattended is a product decision, and the mechanism
// existing is not a reason to make it here.

// ProvisionTargets is what a Generation must contain to be worth activating.
//
// The binaries are named here rather than in the release, because what an
// install runs is a property of *this* Supervisor: a release that named its own
// binaries could name a different one and be obeyed, which is a supply-chain
// decision dressed up as a manifest field.
var ProvisionTargets = []ActivationTarget{
	{Child: PlatformChildName, Binary: "mosaic-platform"},
	{Child: ShellChildName, Binary: "mosaic-shell"},
}

// **There is no artefact list here.** The Updater derives one from the targets
// above through ReleaseArtefactName, which is the single place the mapping from
// a binary to its per-host release file name lives. A second list beside this
// one was written and deleted within the hour: it named the binaries without
// their `-linux-amd64` suffix, agreed with itself in a test, and would have
// been noticed only by a fetch 404ing — which is exactly how it was noticed.

// Provisioner owns the Generations an install holds and the machinery to
// acquire one.
type Provisioner struct {
	Generations *Generations
	Updater     *Updater
	Activity    *Activity
	// Manager is asked which children are waiting for a binary, which is what
	// decides whether an absent release catalogue is a problem or simply
	// unused.
	Manager *Manager
	// Spool is where a provisioning failure is recorded (platform#74). Nothing
	// else can report it: an install that cannot provision has no Platform to
	// tell, which is exactly why the spool is a file.
	Spool *Spool
	// Development is true when the release catalogue is trusted through a
	// development key rather than the shipped one, so the boot log can say so.
	Development bool
	Tel         *Telemetry
}

// ErrNoReleaseSource is the refusal when there is nothing on disk and nowhere
// configured to get anything from.
//
// **It is an error rather than a silent wait**, because the two look identical
// from outside — a box showing "starting" forever — and only one of them is
// something a person can act on.
var ErrNoReleaseSource = errors.New("supervisor: no generation on disk and no release catalogue configured")

// OpenProvisioner builds the whole chain from configuration, or reports why it
// cannot. A nil Provisioner with a nil error is a deployment that manages its
// own binaries — the DIY path, and the dev stack — which is supported rather
// than degraded.
func OpenProvisioner(cfg Config, manager *Manager, activity *Activity, spool *Spool, tel *Telemetry) (*Provisioner, error) {
	generations, err := OpenGenerations(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	p := &Provisioner{Generations: generations, Activity: activity, Manager: manager, Spool: spool, Tel: tel}

	if cfg.ReleaseURL == "" {
		// Nothing to fetch from. Still a Provisioner, because it can boot onto
		// a Generation that is already here — an install that was provisioned
		// once and has since had its catalogue URL removed must still start.
		return p, nil
	}

	keys, development, err := TrustedKeys()
	if err != nil {
		return nil, err
	}
	if keys.Empty() {
		// **A build with no trusted key cannot provision, and says so at boot
		// rather than at the moment somebody needs it.** This is the shipped
		// state today: no release key exists yet (platform#76), so a release
		// binary carries an empty one. Refusing here would mean refusing to
		// start, which is worse — an install with binaries on disk works fine.
		tel.Warn(componentGeneration, "no trusted release key is compiled in, "+
			"so this install cannot fetch or verify a release")
		return p, nil
	}
	p.Development = development

	fetcher := &Fetcher{Generations: generations, Keys: keys, Tel: tel, Activity: activity}
	activator := &Activator{
		Generations: generations,
		Manager:     manager,
		Targets:     ProvisionTargets,
		Tel:         tel,
		Activity:    activity,
		Spool:       spool,
	}
	p.Updater = &Updater{IndexURL: cfg.ReleaseURL, Fetcher: fetcher, Activator: activator, Keys: keys, Tel: tel}
	return p, nil
}

// CommandFor is what a child should run, given the Generation that is live.
//
// Empty when there is none, which the Manager reads as "externally managed" —
// the shape the dev stack uses, where something else owns the process and the
// Supervisor only fronts and reports on it.
func (p *Provisioner) CommandFor(child string) []string {
	if p == nil || p.Generations == nil {
		return nil
	}
	version, ok := p.Generations.Active()
	if !ok {
		return nil
	}
	for _, t := range ProvisionTargets {
		if t.Child == child {
			return append([]string{filepath.Join(p.Generations.Dir(version), t.Binary)}, t.Args...)
		}
	}
	return nil
}

// EnsureGeneration acquires one if this install has none.
//
// **Only when there is none.** An install that already has a Generation boots
// onto it and is left alone: fetching on every boot would make a restart into
// an upgrade, which is the behaviour nobody asks for and everybody notices —
// a machine that came back on a different version because it was power-cycled.
//
// It returns an error rather than failing the boot. A Supervisor that cannot
// provision must still run, because the front door is what tells somebody why
// it cannot; exiting would replace an explanation with a refused connection.
func (p *Provisioner) EnsureGeneration(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if version, ok := p.Generations.Active(); ok {
		p.log("running generation %s", version)
		return nil
	}
	// Nothing owned is waiting for a binary, so there is nothing to fetch and
	// nothing to report. This is the ordinary shape of a deployment that runs
	// its own processes, and saying anything here would be an alarming line
	// about a real misconfiguration printed when nothing is wrong.
	waiting := p.Manager.AwaitingCommand()
	if len(waiting) == 0 {
		return nil
	}
	if p.Updater == nil {
		return fmt.Errorf("%w (%s have nothing to run)", ErrNoReleaseSource, strings.Join(waiting, " and "))
	}
	if p.Development {
		// At every boot that uses it, like the Platform's development registry
		// (platform#55). A development key signing a development catalogue is a
		// working configuration, and one nobody should be on by accident.
		p.log("WARNING fetching the first generation against a development release key")
	}
	p.log("no generation on disk; fetching one")

	version, err := p.Updater.Upgrade(ctx)
	if err != nil {
		if errors.Is(err, ErrUpToDate) {
			// Reachable only if a Generation appeared between the check above
			// and here, which is not a failure.
			return nil
		}
		p.Spool.Record(FindingProvisionFailed, ContextHost, "", errText(err))
		return err
	}
	p.log("provisioned generation %s", version)
	return nil
}

// ActiveGeneration is the version running now, if any. It is what the children
// are told they belong to (platform#77).
func (p *Provisioner) ActiveGeneration() (string, bool) {
	if p == nil || p.Generations == nil {
		return "", false
	}
	return p.Generations.Active()
}

// Update is the machinery an upgrade loop drives, nil when this install has no
// release source — which is a deployment that manages its own binaries, where
// there is nothing to check and nothing that could carry out a request.
func (p *Provisioner) Update() *Updater {
	if p == nil {
		return nil
	}
	return p.Updater
}

func (p *Provisioner) log(format string, args ...any) {
	if p == nil {
		return
	}
	p.Tel.Printf(format, args...)
}
