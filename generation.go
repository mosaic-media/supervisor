// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Generations on disk.
//
// A Generation is "Platform, Shell and admitted Modules" — but since platform#38
// moved the build to CI and platform#51 took extension modules out of the binary,
// what the Supervisor actually holds is a directory of verified binaries and a
// version naming them. It builds nothing; it selects.
//
// The layout is deliberately boring, because this is the state that has to be
// readable by a person at 2am when the thing that reads it will not start:
//
//	<root>/generations/<version>/          the binaries
//	<root>/generations/<version>/.complete  written last, after everything verified
//	<root>/generations.json                 {"active": "...", "previous": "..."}
//
// The .complete marker is the load-bearing file. A download interrupted halfway
// leaves a directory of plausible-looking binaries, and the only difference
// between that and a good Generation is whether every artefact verified. The
// marker is written after the last one does, and [Generations.Activate] refuses
// a version without it — so a partial download cannot be activated by a
// Supervisor that restarted in the middle of one.

// ErrIncomplete is the refusal when a Generation was never finished: staged,
// partly downloaded, and not verified through to the end.
var ErrIncomplete = errors.New("supervisor: that generation was never completed, so it is not activatable")

// ErrNoPrevious is the refusal when a rollback is asked for and there is
// nothing to roll back to — the first Generation, or one whose predecessor has
// been pruned.
var ErrNoPrevious = errors.New("supervisor: there is no previous generation to roll back to")

// pointer is the on-disk record of which Generation is live.
//
// Two fields and no history. "What is running" and "what was running before it"
// are the only two a rollback needs, and a longer list would be a retention
// policy nobody asked for — the directories are still there and still named by
// version if somebody wants an older one.
type pointer struct {
	Active   string `json:"active"`
	Previous string `json:"previous"`
}

// Generations is the set of Generations this install holds, and which one is
// live.
type Generations struct {
	root string
}

// OpenGenerations prepares the directories under root, creating them if this is
// a first boot.
//
// 0700 throughout: these are executables the Supervisor will run as itself, so
// anything that can write here can choose what runs. That is the same reasoning
// as the runtime directory's permissions and it matters more, because a socket
// is reachable and a binary is executed.
func OpenGenerations(root string) (*Generations, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("supervisor: the generations root is required")
	}
	g := &Generations{root: root}
	if err := os.MkdirAll(g.versionsDir(), 0o700); err != nil {
		return nil, fmt.Errorf("supervisor: preparing %s: %w", g.versionsDir(), err)
	}
	return g, nil
}

func (g *Generations) versionsDir() string { return filepath.Join(g.root, "generations") }
func (g *Generations) pointerPath() string { return filepath.Join(g.root, "generations.json") }
func (g *Generations) markerPath(v string) string {
	return filepath.Join(g.Dir(v), ".complete")
}

// Dir is where a version's binaries live. It is a pure path function, so it
// answers for a version that does not exist yet — which is what [Generations.Stage]
// needs.
func (g *Generations) Dir(version string) string {
	return filepath.Join(g.versionsDir(), version)
}

// Stage prepares an empty directory for a version being downloaded, discarding
// anything already there under that name.
//
// Discarding is the right default and the surprising one. A re-staged version is
// a retry, and a retry that merged into a previous attempt's leavings would
// produce a directory whose contents came from two downloads — each file
// individually verified, and the set never checked as a whole. Starting empty
// costs a re-download and removes that.
func (g *Generations) Stage(version string) (dir string, err error) {
	if err := validVersion(version); err != nil {
		return "", err
	}
	dir = g.Dir(version)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("supervisor: clearing %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("supervisor: preparing %s: %w", dir, err)
	}
	return dir, nil
}

// Commit marks a staged version complete. It must be called only once every
// artefact has been downloaded and verified, never before — the marker is the
// only thing that distinguishes a finished Generation from an interrupted one.
func (g *Generations) Commit(version string) error {
	if err := validVersion(version); err != nil {
		return err
	}
	if _, err := os.Stat(g.Dir(version)); err != nil {
		return fmt.Errorf("supervisor: cannot complete %s: %w", version, err)
	}
	return os.WriteFile(g.markerPath(version), []byte(version+"\n"), 0o600)
}

// Complete reports whether a version finished downloading and verifying.
func (g *Generations) Complete(version string) bool {
	_, err := os.Stat(g.markerPath(version))
	return err == nil
}

// Discard removes a Generation's directory. Refused for the active one, because
// deleting what is currently running is not an operation with a good outcome.
func (g *Generations) Discard(version string) error {
	if err := validVersion(version); err != nil {
		return err
	}
	if active, ok := g.Active(); ok && active == version {
		return fmt.Errorf("supervisor: %s is active and cannot be discarded", version)
	}
	return os.RemoveAll(g.Dir(version))
}

// Activate makes a completed version live, keeping the one it replaces as the
// rollback target.
//
// It changes a pointer and nothing else — no process is started or stopped here.
// Separating the two is what lets activation's failure branch put the pointer
// back before anything is restarted a second time, and it keeps this file
// testable without spawning anything.
func (g *Generations) Activate(version string) error {
	if err := validVersion(version); err != nil {
		return err
	}
	if !g.Complete(version) {
		return fmt.Errorf("%w: %s", ErrIncomplete, version)
	}
	p := g.read()
	if p.Active == version {
		return nil // already live; activating twice is not an error
	}
	// The version being replaced becomes the rollback target. An activation
	// from nothing (a first boot) leaves Previous empty, which is what
	// ErrNoPrevious then reports rather than rolling back to "".
	return g.write(pointer{Active: version, Previous: p.Active})
}

// Rollback returns to the previous Generation and reports which one that is.
//
// The two pointers swap rather than the previous one being dropped. A rollback
// that failed would otherwise have nowhere to go back to, and "the upgrade broke
// and so did the rollback" is exactly when somebody needs the option of the
// version they started from.
func (g *Generations) Rollback() (version string, err error) {
	p := g.read()
	if p.Previous == "" {
		return "", ErrNoPrevious
	}
	if !g.Complete(p.Previous) {
		// The directory has been removed or was never finished. Saying so beats
		// pointing at it and having the next start fail to exec.
		return "", fmt.Errorf("%w: %s is recorded but not complete", ErrNoPrevious, p.Previous)
	}
	if err := g.write(pointer{Active: p.Previous, Previous: p.Active}); err != nil {
		return "", err
	}
	return p.Previous, nil
}

// Active is the live Generation, and whether there is one. A fresh install has
// none, which is a state rather than an error: it is what a first boot has to
// download into.
func (g *Generations) Active() (string, bool) {
	v := g.read().Active
	return v, v != ""
}

// Previous is the rollback target, and whether there is one.
func (g *Generations) Previous() (string, bool) {
	v := g.read().Previous
	return v, v != ""
}

// List names every Generation on disk, newest-looking last, with whether each
// completed. It exists for the diagnostic surface: "what does this box hold"
// is the first question when an activation has gone wrong.
func (g *Generations) List() (versions []string, err error) {
	entries, err := os.ReadDir(g.versionsDir())
	if err != nil {
		return nil, fmt.Errorf("supervisor: reading %s: %w", g.versionsDir(), err)
	}
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	sort.Strings(versions)
	return versions, nil
}

// read returns the pointer, treating every unreadable state as "nothing is
// active".
//
// That is deliberate and it is the safe direction. A missing file is a first
// boot; a corrupt one is a disk that lost a write. Both mean the Supervisor
// cannot say what should be running, and answering "nothing" makes it download
// rather than exec a path it guessed at.
func (g *Generations) read() pointer {
	b, err := os.ReadFile(g.pointerPath())
	if err != nil {
		return pointer{}
	}
	var p pointer
	if err := json.Unmarshal(b, &p); err != nil {
		return pointer{}
	}
	return p
}

// write replaces the pointer atomically — a temporary file and a rename, so a
// crash mid-write leaves the old pointer rather than a truncated one. The
// failure this prevents is the worst available here: a Supervisor that cannot
// read its own pointer has no idea what to start.
func (g *Generations) write(p pointer) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(g.root, "generations-*.json")
	if err != nil {
		return fmt.Errorf("supervisor: writing the generation pointer: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename below succeeds

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("supervisor: writing the generation pointer: %w", err)
	}
	// Flushed before the rename, or the rename can land ahead of the bytes and
	// a power loss leaves an empty file where a valid pointer used to be.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("supervisor: flushing the generation pointer: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("supervisor: writing the generation pointer: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("supervisor: writing the generation pointer: %w", err)
	}
	return os.Rename(name, g.pointerPath())
}

// validVersion refuses anything that would escape the generations directory or
// name something other than a version.
//
// A version reaches this from a release catalogue, which is remote input, and it
// is used to build a path. ".." in it is a directory traversal that ends with the
// Supervisor executing a file somebody else chose, so the check is here rather
// than at each call site.
func validVersion(version string) error {
	switch {
	case strings.TrimSpace(version) == "":
		return errors.New("supervisor: a generation needs a version")
	case version != filepath.Base(version), version == "." || version == "..":
		return fmt.Errorf("supervisor: %q is not a usable generation name", version)
	case strings.ContainsAny(version, `/\`):
		return fmt.Errorf("supervisor: %q is not a usable generation name", version)
	case strings.HasPrefix(version, "."):
		// A leading dot would collide with the .complete marker's namespace and
		// hide the directory from an operator listing it.
		return fmt.Errorf("supervisor: %q is not a usable generation name", version)
	}
	return nil
}
