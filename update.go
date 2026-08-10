// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Discovering a release and upgrading to it.
//
// This is the piece that composes the two that already existed and did nothing
// on their own: [Fetcher.Fetch] brings a Generation down and verifies it, and
// [Activator.Activate] puts it into service and reverts if it does not serve.
// Neither had a caller, and nothing knew a release existed.
//
// **The catalogue is signed, and that is not the same signature as the
// artefacts'.** SHA256SUMS is signed so the bytes cannot be swapped; the index
// is signed so the *choice of version* cannot be. Without the second, a host
// that serves the release can pin an install to an old, genuinely-signed
// release forever — a rollback attack, where every signature checks out and the
// install never receives the fix it is waiting for. Home Assistant's updater
// fetches its version document over plain HTTPS with no signature at all, which
// is the specific thing the roadmap says not to copy.
//
// **Nothing here runs on a schedule.** Check and Upgrade are called; there is
// no poll. A periodic check is a small addition on top and it needs a decision
// this does not make — how often, and whether an install upgrades itself
// unattended — which is a product question rather than a mechanism.

// ReleaseIndexSchema identifies the document, so a future format change is a
// refusal rather than a misparse.
const ReleaseIndexSchema = "mosaic.release.index/v1"

// checkTimeout bounds a catalogue read. Far shorter than a download's, because
// a catalogue is a few hundred bytes.
const checkTimeout = 30 * time.Second

// ErrUnknownRelease is the refusal when a version is asked for that the signed
// catalogue does not offer. An install must not fetch from a URL nobody signed
// for, which is what taking a caller's URL would amount to.
var ErrUnknownRelease = errors.New("supervisor: the signed release catalogue does not offer that version")

// ErrUpToDate reports that the latest release is already active. It is an error
// value rather than a silent success so a caller can tell "nothing to do" from
// "upgraded", which are different things to say to somebody watching.
var ErrUpToDate = errors.New("supervisor: the latest release is already active")

// ReleaseEntry is one release the catalogue offers.
type ReleaseEntry struct {
	Version string `json:"version"`
	// URL is the directory holding this release's artefacts, its SHA256SUMS and
	// the signature over them. It is declared per release rather than computed
	// from a template for the reason the module registry declares its own:
	// where a release hosts its bytes is the release's to say, and a template
	// is a second place for the answer to be wrong.
	URL string `json:"url"`
}

// ReleaseIndex is the signed catalogue of what an install may upgrade to.
type ReleaseIndex struct {
	Schema   string         `json:"schema"`
	Releases []ReleaseEntry `json:"releases"`
}

// Latest is the highest version the catalogue offers that this build can order.
//
// **It is computed rather than read from a `latest` field**, and the difference
// is a real one: a field is a second statement of the same fact, and the two
// disagreeing is a catalogue that offers one version and names another. An
// entry this build cannot parse is skipped rather than failing the whole
// catalogue — a future release using a versioning scheme this binary predates
// must not stop it seeing the ones it does understand.
func (i ReleaseIndex) Latest() (ReleaseEntry, bool) {
	var best ReleaseEntry
	for _, e := range i.Releases {
		if _, err := parseVersion(e.Version); err != nil {
			continue
		}
		if best.Version == "" {
			best = e
			continue
		}
		if up, err := newer(best.Version, e.Version); err == nil && up {
			best = e
		}
	}
	return best, best.Version != ""
}

// Find returns the entry for a version.
func (i ReleaseIndex) Find(version string) (ReleaseEntry, bool) {
	for _, e := range i.Releases {
		if e.Version == version {
			return e, true
		}
	}
	return ReleaseEntry{}, false
}

// Updater checks the catalogue and upgrades to what it offers.
type Updater struct {
	// IndexURL is the directory holding index.json and index.json.sig.
	IndexURL  string
	Fetcher   *Fetcher
	Activator *Activator
	Keys      *Keyring
	Tel       *Telemetry
}

// Available is what a check found.
type Available struct {
	// Active is the Generation running now, empty on a fresh install.
	Active string
	// Latest is the highest version the catalogue offers.
	Latest ReleaseEntry
	// Upgrade is whether Latest is newer than Active.
	Upgrade bool
	// SignedBy names the key that vouched for the catalogue, which is the only
	// place an overlap window (platform#76) is visible from outside.
	SignedBy string
}

// Check fetches and verifies the catalogue and reports what is on offer.
//
// It changes nothing. Separating the question from the act is what lets a
// surface show "v0.4.0 is available" without an install having decided
// anything, and it is why an unreachable catalogue costs a check rather than an
// upgrade.
func (u *Updater) Check(ctx context.Context) (Available, error) {
	index, keyID, err := u.catalogue(ctx)
	if err != nil {
		return Available{}, err
	}
	latest, ok := index.Latest()
	if !ok {
		return Available{}, errors.New("supervisor: the release catalogue offers no version this build can order")
	}

	active, _ := u.Activator.Generations.Active()
	up, err := newer(active, latest.Version)
	if err != nil {
		return Available{}, err
	}
	return Available{Active: active, Latest: latest, Upgrade: up, SignedBy: keyID}, nil
}

// Upgrade takes the latest release the catalogue offers, if it is newer than
// what is running.
//
// **It refuses to move backwards**, which is the half of the rollback-attack
// defence the signature cannot provide on its own: a signed catalogue that
// offers only old versions is still a signed catalogue. Going back deliberately
// is [Activator.Rollback] or [Updater.UpgradeTo], both of which are somebody
// choosing rather than an install deciding.
func (u *Updater) Upgrade(ctx context.Context) (version string, err error) {
	found, err := u.Check(ctx)
	if err != nil {
		return "", err
	}
	if !found.Upgrade {
		return found.Active, ErrUpToDate
	}
	return found.Latest.Version, u.install(ctx, found.Latest)
}

// UpgradeTo installs a named version from the catalogue, newer or not.
//
// The version must be one the signed catalogue offers: the URL comes from the
// entry rather than from the caller, so no caller can point an install at bytes
// nobody signed for.
func (u *Updater) UpgradeTo(ctx context.Context, version string) error {
	index, _, err := u.catalogue(ctx)
	if err != nil {
		return err
	}
	entry, ok := index.Find(version)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownRelease, version)
	}
	return u.install(ctx, entry)
}

// install is fetch-then-activate, which is the whole composition and the reason
// this type exists.
//
// A Generation already on disk and complete is not downloaded again — the
// common case being an activation that failed and is being retried, where
// re-fetching a hundred megabytes to arrive at the same bytes is time spent for
// nothing. It is safe because completeness means every artefact verified
// against the signed checksums; an incomplete one is re-staged from scratch by
// Fetch.
func (u *Updater) install(ctx context.Context, entry ReleaseEntry) error {
	if u.Fetcher == nil || u.Activator == nil {
		return errors.New("supervisor: an updater needs a fetcher and an activator")
	}
	if u.Activator.Generations.Complete(entry.Version) {
		u.log("generation %s is already on disk and verified", entry.Version)
	} else {
		u.log("fetching generation %s from %s", entry.Version, entry.URL)
		keyID, err := u.Fetcher.Fetch(ctx, Release{
			Version:   entry.Version,
			BaseURL:   entry.URL,
			Artefacts: u.artefacts(),
		})
		if err != nil {
			return err
		}
		u.log("generation %s downloaded and verified, signed by %s", entry.Version, keyID)
	}
	return u.Activator.Activate(ctx, entry.Version)
}

// artefacts is what this install needs from a release, derived from the
// activation targets so the two cannot disagree about which binaries a
// Generation must hold.
func (u *Updater) artefacts() []Artefact {
	out := make([]Artefact, 0, len(u.Activator.Targets))
	for _, t := range u.Activator.Targets {
		out = append(out, Artefact{
			// The release names its files by target and a Generation holds one
			// host's under the name the child spec execs. ReleaseArtefactName
			// is the single place that mapping lives.
			Name:       ReleaseArtefactName(t.Binary),
			As:         t.Binary,
			Executable: true,
		})
	}
	return out
}

// catalogue fetches index.json and its signature and verifies one against the
// other.
func (u *Updater) catalogue(ctx context.Context) (ReleaseIndex, string, error) {
	if u.Fetcher == nil {
		return ReleaseIndex{}, "", errors.New("supervisor: an updater needs a fetcher")
	}
	if u.Keys == nil || u.Keys.Empty() {
		return ReleaseIndex{}, "", ErrNoTrustedKey
	}

	// Bounded separately from a download: a catalogue is a few hundred bytes,
	// and a check that hangs for a download's timeout makes a surface look
	// broken rather than slow.
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	body, err := u.Fetcher.get(ctx, u.IndexURL, "index.json", maxMetadataBytes)
	if err != nil {
		return ReleaseIndex{}, "", err
	}
	sig, err := u.Fetcher.get(ctx, u.IndexURL, "index.json.sig", maxMetadataBytes)
	if err != nil {
		return ReleaseIndex{}, "", err
	}
	keyID, ok := u.Keys.Verify(body, sig)
	if !ok {
		// Same words as an unsigned artefact, because it is the same failure:
		// something this build will not trust is claiming to be Mosaic's.
		return ReleaseIndex{}, "", fmt.Errorf("%w (the release catalogue)", ErrUnsigned)
	}

	var index ReleaseIndex
	if err := json.Unmarshal(body, &index); err != nil {
		return ReleaseIndex{}, "", fmt.Errorf("supervisor: the release catalogue is not readable: %w", err)
	}
	if index.Schema != ReleaseIndexSchema {
		return ReleaseIndex{}, "", fmt.Errorf(
			"supervisor: the release catalogue is %q, and this build reads %q",
			index.Schema, ReleaseIndexSchema)
	}
	return index, keyID, nil
}

func (u *Updater) log(format string, args ...any) {
	u.Tel.Printf(format, args...)
}

// ReleaseArtefactName maps a binary's installed name to the name a release
// publishes it under for this host.
//
// The release matrix produces `mosaic-platform-linux-amd64`; a Generation holds
// `mosaic-platform`, because that is what the child spec execs and what a
// person reading the directory expects. Doing the mapping in one exported
// function keeps the target suffix out of every caller — and makes the one
// place that has to change when a new target is added findable.
func ReleaseArtefactName(binary string) string {
	name := fmt.Sprintf("%s-%s-%s", binary, hostOS, hostArch)
	if hostOS == "windows" {
		name += ".exe"
	}
	return name
}
