// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// The upgrade loop (platform#77).
//
// The machinery to upgrade — fetch, verify, activate, gate on the surface a
// client actually reaches, revert keeping the evidence — is [Updater]'s. This is
// the half that makes it reachable, and it invents no channel to do it.
//
// The offer goes out on the spool and the request comes back on the handoff.
// Both already exist and both already carry exactly this shape of message: the
// spool is how the Supervisor tells the Platform what it learned (platform#74), and
// the handoff is how the Supervisor asks the Platform what is owed — it already
// reports a configuration escalation waiting for a process that can perform one.
//
// Nothing here acknowledges anything. A request names a version, and it is
// satisfied when the Platform is running that version — which the Platform
// decides for itself from the Generation id it was started with. That is why the
// channel stays one-directional: success is observable rather than reported, and
// an acknowledgement written by a process an upgrade is about to replace would be
// a claim made by the wrong process at the wrong time.

// upgradeCheckInterval is how often the catalogue is consulted.
//
// Six hours rather than minutes: a release is a human-scale event, the answer
// changes at most daily, and this is the first thing in this process that
// reaches outward on its own — so the rate is set by what is useful rather than
// by what is possible. A check costs one signed catalogue fetch and changes
// nothing.
const upgradeCheckInterval = 6 * time.Hour

// requestPollInterval is how often the Platform is asked whether somebody has
// pressed the control. Short, because it is a loopback request on a socket and
// the person who pressed it is watching.
const requestPollInterval = 10 * time.Second

// UpgradeRequestPath is where the Platform reports a pending upgrade request, on
// the private handoff listener beside `/config` — deliberately not on the public
// port, which is what keeps Generation state off it (platform#75).
const UpgradeRequestPath = "/upgrade"

// UpgradeRequest is what the handoff answers.
//
// It is flat, with no schema version, like the spool at the other end and for
// the same reason: it is written by one binary and read by another that may have
// been upgraded independently, so the reader treats it as untrusted input and
// ignores what it does not understand.
type UpgradeRequest struct {
	// Pending says somebody has asked for an upgrade that has not happened.
	Pending bool `json:"pending"`
	// Version is what they asked for. It is always a named version and never
	// "latest": the Platform does not hold the catalogue and must not guess at
	// what the newest release is, so it asks for the one it was offered.
	Version string `json:"version,omitempty"`
	// RequestedAt is when, for the record.
	RequestedAt time.Time `json:"requestedAt,omitzero"`
}

// UpgradeWatch checks the catalogue and carries out what a person asked for.
type UpgradeWatch struct {
	// Updater is the machinery. Nil is an install with no release source, where
	// there is nothing to check and nothing to offer.
	Updater *Updater
	// Handoff is the Platform's private listener, where a request appears.
	Handoff Endpoint
	// Spool is where an offer and a failure are written for the Platform to
	// adopt (platform#74).
	Spool *Spool
	Tel   *Telemetry
	// CheckInterval and PollInterval default to the constants above. They exist
	// so a test does not wait six hours.
	CheckInterval time.Duration
	PollInterval  time.Duration
}

// Run watches until ctx is cancelled. It blocks.
//
// The two rhythms are deliberately different and deliberately independent: the
// catalogue is a network call answering a question that changes daily, and the
// request is a loopback read answering one that changes the moment somebody
// presses a control. Sharing one timer would either make the catalogue check
// chatty or make the control feel broken.
func (w *UpgradeWatch) Run(ctx context.Context) {
	if w == nil || w.Updater == nil {
		// No release source: nothing to check, nothing to offer, and a request
		// nobody could carry out. Saying so at boot would be an alarming line
		// about a deployment that is working as intended.
		return
	}

	check := time.NewTicker(orDefault(w.CheckInterval, upgradeCheckInterval))
	defer check.Stop()
	poll := time.NewTicker(orDefault(w.PollInterval, requestPollInterval))
	defer poll.Stop()

	// The first check happens now rather than an interval from now, so an
	// install that has been off for a week says what is available on the boot
	// somebody is watching.
	w.checkCatalogue(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			w.checkCatalogue(ctx)
		case <-poll.C:
			w.carryOutRequest(ctx)
		}
	}
}

// checkCatalogue asks what is available and records an offer.
//
// An unreachable catalogue costs a check rather than an upgrade, and it is
// recorded at debug rather than warn: a household box with no internet for an
// afternoon is the ordinary case, and a warning every six hours about it is how
// a log stops being read.
func (w *UpgradeWatch) checkCatalogue(ctx context.Context) {
	found, err := w.Updater.Check(ctx)
	if err != nil {
		w.Tel.Event(LevelDebug, componentGeneration, "could not check the release catalogue", Err(err))
		return
	}
	if !found.Upgrade {
		return
	}
	w.Tel.Info(componentGeneration, "a newer version is available",
		String("version", found.Latest.Version),
		String("active", found.Active),
		String("signed_by", found.SignedBy))
	// The offer reaches a person as a row on the register, which is the surface
	// that already exists for "something needs your attention" (supervisor#12). The
	// register folds repeats into one Issue with a count, so checking every six
	// hours does not produce a row every six hours.
	w.Spool.Record(FindingUpgradeAvailable, ContextGeneration, found.Latest.Version, found.SignedBy)
}

// carryOutRequest reads the handoff and upgrades if somebody asked.
func (w *UpgradeWatch) carryOutRequest(ctx context.Context) {
	request, ok := w.readRequest(ctx)
	if !ok || !request.Pending || request.Version == "" {
		return
	}

	w.Tel.Info(componentGeneration, "upgrading because it was asked for",
		String("version", request.Version))

	// UpgradeTo rather than Upgrade: the version is the one the person was
	// offered, and its URL comes from the signed catalogue rather than from the
	// request — so no request can point an install at bytes nobody signed for,
	// however it arrived.
	if err := w.Updater.UpgradeTo(ctx, request.Version); err != nil {
		w.Tel.Error(componentGeneration, "the requested upgrade did not happen",
			String("version", request.Version), Err(err))
		// Recorded for the Platform to adopt. The request stops being pending on
		// its own — the Platform that comes back is the old one and its
		// Generation does not match what was asked for — so what this adds is
		// the reason, which nothing else would carry.
		w.Spool.Record(FindingUpgradeFailed, ContextGeneration, request.Version, errText(err))
		return
	}
	// Deliberately not reported as done. The Platform decides that for itself by
	// comparing the Generation it is running against the one that was requested,
	// which is a statement about what is rather than about what happened — and
	// the process that would have reported it has just been replaced.
}

// readRequest asks the handoff. A Platform that is down, starting, or too old to
// serve this path answers nothing, and nothing is the same as no request.
func (w *UpgradeWatch) readRequest(ctx context.Context) (UpgradeRequest, bool) {
	client := w.Handoff.Client(readinessTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.Handoff.URL(UpgradeRequestPath), nil)
	if err != nil {
		return UpgradeRequest{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return UpgradeRequest{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return UpgradeRequest{}, false
	}
	// Bounded, because this is a response from another process and a reader
	// with no limit is a way to be handed a gigabyte.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return UpgradeRequest{}, false
	}
	var request UpgradeRequest
	if err := json.Unmarshal(body, &request); err != nil {
		// Untrusted input: a shape this build does not understand is ignored
		// rather than reported, which is the correct failure for a handoff
		// served by a newer Platform.
		return UpgradeRequest{}, false
	}
	return request, true
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

// ErrNoUpgradeRequested is returned by CarryOutOnce when nothing is waiting. It
// exists for a caller that wants to drive one pass explicitly — a test, and the
// shape a future "check now" control would take.
var ErrNoUpgradeRequested = errors.New("supervisor: no upgrade has been requested")

// CarryOutOnce runs one request pass and reports what it did, for a caller
// driving the loop rather than waiting on it.
func (w *UpgradeWatch) CarryOutOnce(ctx context.Context) (version string, err error) {
	request, ok := w.readRequest(ctx)
	if !ok || !request.Pending || request.Version == "" {
		return "", ErrNoUpgradeRequested
	}
	return request.Version, w.Updater.UpgradeTo(ctx, request.Version)
}
