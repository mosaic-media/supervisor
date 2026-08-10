// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The Supervisor's findings, spooled to a file (platform#74, supervisor#5).
//
// **The findings it most needs to record are the ones it makes while the
// Platform is not there to be written to** — a Platform that will not start, a
// Generation that was rolled back. So the Supervisor writes them to a file and
// the Platform adopts them when it comes up, rather than the Supervisor calling
// a service that is by definition unavailable at the moment that matters.
//
// **The dependency does not invert.** The Platform never reads this to
// function, only to adopt what is in it; a Supervisor with no Platform keeps
// recording, and a Platform with no Supervisor is unaffected.
//
// **Everything here is best-effort and nothing is fatal.** A Supervisor that
// failed to start a child, and then failed to start *because* it could not
// write down that it had failed to start a child, would be the machinery
// defeating the thing it is for.

// spoolName is the file, in the state directory beside the Generations —
// somewhere that survives a reboot, unlike the runtime directory, because a
// finding made during a boot that then failed is exactly the one worth keeping.
const spoolName = "findings.jsonl"

// SpoolEnv is how the Platform is told where to read them. The Supervisor sets
// it on the child, like the socket addresses: the two halves have to agree, and
// a path configured independently at both ends is a pair that silently differs.
const SpoolEnv = "MOSAIC_SUPERVISOR_FINDINGS"

// SpooledFinding is one line of the file.
//
// **A flat record with no schema version.** It is written by one binary and
// read by another that may have been upgraded independently, so the reader
// treats it as untrusted input and skips what it cannot parse (platform#74) — a
// version field would only let it fail more precisely at something it is
// already required to survive.
type SpooledFinding struct {
	Type      string    `json:"type"`
	Context   string    `json:"context"`
	Reference string    `json:"reference,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

// The finding types the Supervisor can record. They are strings here rather
// than a shared package because the Platform owns the vocabulary (platform#74) and
// the Supervisor may not import it — the boundary is two modules wide and this
// is not one of them. A type the Platform does not know is skipped on adoption,
// which is the correct failure for a spool written by a newer binary.
const (
	FindingChildUnrecoverable   = "child_unrecoverable"
	FindingGenerationRolledBack = "generation_rolled_back"
	FindingProvisionFailed      = "provision_failed"
	// The offer and its failure (platform#77). An available version is a finding
	// rather than a notification channel of its own, because the register is
	// already the surface for "something needs your attention" and it already
	// folds repeats into one Issue with a count — so a check every six hours
	// does not produce a row every six hours.
	FindingUpgradeAvailable = "upgrade_available"
	FindingUpgradeFailed    = "upgrade_failed"

	ContextChild      = "child"
	ContextGeneration = "generation"
	ContextHost       = "host"
)

// Spool appends findings to a file for the Platform to adopt.
type Spool struct {
	path string
	mu   sync.Mutex
	tel  *Telemetry
}

// OpenSpool prepares one in the state directory. A Spool with an empty path is
// a no-op, which is what a test or a Supervisor with nowhere to write gets.
func OpenSpool(stateDir string, tel *Telemetry) *Spool {
	if stateDir == "" {
		return &Spool{tel: tel}
	}
	return &Spool{path: filepath.Join(stateDir, spoolName), tel: tel}
}

// Path is where findings are written, for handing to the child.
func (s *Spool) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Record appends one finding.
//
// **Deduplication is the Platform's, not this file's.** The register's identity
// rule already folds repeated detections of one situation into a single Issue
// with a count, so a spool that tried to be clever would be a second, weaker
// implementation of a rule that already exists — and this one cannot read what
// it has written without parsing the whole file on every append.
func (s *Spool) Record(findingType, findingContext, reference, detail string) {
	if s == nil || s.path == "" {
		return
	}
	line, err := json.Marshal(SpooledFinding{
		Type: findingType, Context: findingContext,
		Reference: reference, Detail: detail, At: time.Now().UTC(),
	})
	if err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.logf("could not record a finding: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		s.logf("could not record a finding: %v", err)
	}
}

func (s *Spool) logf(format string, args ...any) {
	if s == nil {
		return
	}
	s.tel.Printf(format, args...)
}
