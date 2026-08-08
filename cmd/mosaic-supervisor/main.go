// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

// Command mosaic-supervisor is Mosaic's host-level manager and single front
// door (ADR 0004, ADR 0005).
//
// It runs the Platform and the Shell as child processes, mints the boot id
// they both adopt (ADR 0060), and is the only public HTTP entry point: TLS on
// one port, the Shell at the root, and the Platform's Connect services,
// /artwork and /playback behind it.
//
// It does not touch extension modules — those are the Platform's throughout
// (ADR 0079) — and it does not build anything, since per-install builds were
// deleted in favour of a CI-built binary (ADR 0063).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mosaic-media/supervisor"
)

// Commands for the children. Empty means externally managed — the Supervisor
// fronts and reports on the process but does not own its lifecycle, which is
// the shape the dev stack uses, where compose owns them.
const (
	platformCommandEnv = "MOSAIC_SUPERVISOR_PLATFORM_COMMAND"
	shellCommandEnv    = "MOSAIC_SUPERVISOR_SHELL_COMMAND"
	// The children's working directories. The Platform needs one: it resolves
	// its extension install directory relative to the working directory
	// (ADR 0081), so started from the Supervisor's own directory it would
	// find none of the modules a user installed.
	platformDirEnv = "MOSAIC_SUPERVISOR_PLATFORM_DIR"
	shellDirEnv    = "MOSAIC_SUPERVISOR_SHELL_DIR"
)

// Where the children are told to bind. The Supervisor decides this rather than
// each child being configured separately, because the two halves have to agree
// and a Supervisor dialling a socket its child was never told to create is a
// failure with no good error message.
const (
	platformAddrEnv        = "MOSAIC_API_ADDR"
	platformHandoffAddrEnv = "MOSAIC_HEALTH_ADDR"
	shellAddrEnv           = "MOSAIC_SHELL_ADDR"
)

// platformServingPath is the client-facing path probed to prove the API
// listener is bound and routing. It names a real Connect method because the
// point is to exercise the mux a client uses, and the same `/mosaic.` prefix
// the front door routes on.
const platformServingPath = "/mosaic.auth.v1.AuthService/Bootstrap"

func main() {
	if err := run(); err != nil {
		log.Printf("mosaic-supervisor: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := supervisor.LoadConfigFromEnv()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The children bind their sockets in here, so it has to exist before
	// either is started (ADR 0120).
	if err := supervisor.PrepareRuntimeDir(cfg.RuntimeDir); err != nil {
		return err
	}
	if cfg.Platform.IsUnix() {
		log.Printf("mosaic-supervisor: children on sockets under %s", cfg.RuntimeDir)
	} else {
		// A warning rather than a note: this is the configuration that gives
		// the Platform a second door, and it should not pass unremarked.
		log.Printf("mosaic-supervisor: WARNING the Platform is on TCP at %s rather than a socket, "+
			"so it can be reached without passing through this process", cfg.Platform.Address())
	}

	// Registration order is stop order — the Platform first, the interface
	// last. Adding a third child means deciding where in that sequence it
	// belongs.
	manager := supervisor.NewManager(cfg.BootID, log.Printf)
	if err := manager.Add(supervisor.ChildSpec{
		Name:       "platform",
		Command:    fields(os.Getenv(platformCommandEnv)),
		WorkingDir: os.Getenv(platformDirEnv),
		// Told where to listen, rather than configured independently: the two
		// halves have to agree, and a Supervisor dialling a socket its child
		// was never asked to create fails with no useful error.
		Env: []string{
			platformAddrEnv + "=" + cfg.Platform.ListenSpec(),
			platformHandoffAddrEnv + "=" + cfg.PlatformHandoff.ListenSpec(),
		},
		// The Platform's own handoff listener, which is the private channel
		// between these two processes and is deliberately not routed through
		// the front door.
		//
		// /readyz, not /healthz: liveness says the process is answering, and
		// the Platform answers that while it is still running migrations. The
		// front door must not send a client to a Platform that cannot serve
		// it yet, so readiness is the question worth asking.
		Readiness: supervisor.NewProbe(cfg.PlatformHandoff, "/readyz"),
		// And the surface a client actually arrives at, which is a different
		// listener on a different socket — `/readyz` is the Platform's opinion
		// of itself and cannot report that the client-facing listener failed
		// to bind or that its mux is unrouted.
		//
		// A GET at a Connect method is refused with 405 before the handler
		// runs, which is why this path is safe to poll: it invokes no RPC, so
		// it neither does the work Bootstrap does nor spends the pre-auth
		// rate-limit budget it shares with every real client (ADR 0101).
		Serving: supervisor.NewProbe(cfg.Platform, platformServingPath),
		// Longer than the default. The Platform may be mid-transaction or
		// draining a playback session, and a SIGKILL there is the unclean
		// stop that costs a recovery on the next boot.
		StopGrace: 45 * time.Second,
	}); err != nil {
		return err
	}
	if err := manager.Add(supervisor.ChildSpec{
		Name:       "shell",
		Command:    fields(os.Getenv(shellCommandEnv)),
		WorkingDir: os.Getenv(shellDirEnv),
		Env:        []string{shellAddrEnv + "=" + cfg.Shell.ListenSpec()},
		// The Shell's health endpoint is on the same listener it serves from,
		// so one probe answers both questions and a serving probe would be
		// the same request twice.
		Readiness: supervisor.NewProbe(cfg.Shell, "/healthz"),
		// It serves static files out of its own binary. There is nothing in
		// flight to finish, so waiting longer would only lengthen every
		// shutdown for no gain.
		StopGrace: 5 * time.Second,
	}); err != nil {
		return err
	}

	// Run owns the children until ctx is cancelled and then stops them in
	// order. The shutdown path below **waits for this to finish** — without
	// that wait the process would exit the moment the front door closed,
	// leaving every child to be killed by whatever is above the Supervisor
	// instead of stopped by it, which is precisely the job it exists to do.
	managerDone := make(chan struct{})
	go func() {
		defer close(managerDone)
		manager.Run(ctx)
	}()

	frontDoor, err := supervisor.NewFrontDoor(cfg, manager.Snapshot)
	if err != nil {
		return err
	}

	certificate, generated, err := supervisor.TLSCertificate(cfg)
	if err != nil {
		return err
	}
	if generated {
		// Said at every boot on purpose. A self-signed certificate is a
		// stopgap, and an install that has quietly been on one for a year is
		// the failure this warning exists to prevent.
		log.Printf("mosaic-supervisor: WARNING serving a self-signed certificate generated for this boot; " +
			"set MOSAIC_SUPERVISOR_TLS_CERT and MOSAIC_SUPERVISOR_TLS_KEY for a real one")
	}

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: frontDoor,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: the session transport's push lane holds a
		// response open by design, and a timeout here would sever it on a
		// schedule.
		IdleTimeout: 120 * time.Second,
	}

	log.Printf("mosaic-supervisor listening on %s (boot: %s, platform: %s, shell: %s)",
		cfg.ListenAddr, cfg.BootID, cfg.Platform.Address(), cfg.Shell.Address())

	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	log.Printf("mosaic-supervisor: shutting down")

	// The children go first, in registration order — the Platform, then the
	// Shell — and **the front door stays open while they do**. That ordering
	// is only worth anything if something is still answering to show it:
	// closing the front door first would make every rung of ADR 0005's ladder
	// invisible, since a client would get a refused connection either way.
	//
	// So a shutdown walks the ladder down rather than falling off it. The
	// Platform stops and the Shell, still up, renders its offline state; the
	// Shell stops and the holding page answers; only then does the door
	// close. It is also what ADR 0033's live handover needs, where the
	// Platform is replaced under a Shell that never went away.
	<-managerDone

	// The door last. Draining is bounded because a held-open push lane
	// (ADR 0041) never completes on its own, so waiting for it would mean
	// never shutting down.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), frontDoorDrain)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("mosaic-supervisor: front door did not drain cleanly: %v", err)
	}

	log.Printf("mosaic-supervisor: stopped")
	return nil
}

// frontDoorDrain bounds how long the door takes to close once both children
// are already gone. Everything still connected at that point is being served
// the holding page, so this is short.
const frontDoorDrain = 10 * time.Second

// fields splits a command string on whitespace. This is deliberately not a
// shell: a command needing quoting or a pipe belongs in a script the
// Supervisor starts, not in an environment variable it has to parse.
func fields(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return strings.Fields(raw)
}
