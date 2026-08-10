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
	// Set to say something else runs this child and the Supervisor only fronts
	// and reports on it.
	//
	// **It is opt-out rather than inferred, and that was a defect.** Ownership
	// used to be worked out from "no command and nowhere to fetch one", which
	// reads as "not mine" — and in the supervised image, where there is no
	// something-else in the container at all, it meant a Supervisor sat at
	// "Starting" forever without ever saying it had nowhere to fetch from. The
	// ambiguous case now defaults to the honest reading: this is mine, and I
	// have nothing to run.
	platformExternalEnv = "MOSAIC_SUPERVISOR_PLATFORM_EXTERNAL"
	shellExternalEnv    = "MOSAIC_SUPERVISOR_SHELL_EXTERNAL"
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

// main is the one place still writing through the standard logger, and
// deliberately. Everything run reports goes through the telemetry it opens
// (ADR 0060) — but a failure that escapes run may be the failure to load the
// configuration that says where the log file goes, so the last word is on
// stderr, which needs nothing to have worked.
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

	// **Everything this process says goes to a file as well as the console**
	// (ADR 0060). It is the process that survives the failures worth
	// diagnosing — a Platform that will not start, a Generation that dies on
	// activation — and until now it said all of that to stdout and nowhere
	// else, so on a box where nobody is watching the console it said it to no
	// one. Opened first, so the failures below are in it.
	tel := supervisor.OpenTelemetry(cfg, os.Stderr)
	defer tel.Close()
	if path := tel.Path(); path != "" {
		tel.Info("", "recording to "+path)
	}

	// The children bind their sockets in here, so it has to exist before
	// either is started (ADR 0120).
	if err := supervisor.PrepareRuntimeDir(cfg.RuntimeDir); err != nil {
		return err
	}
	if cfg.Platform.IsUnix() {
		tel.Info("", "children on sockets under "+cfg.RuntimeDir)
	} else {
		// A warning rather than a note: this is the configuration that gives
		// the Platform a second door, and it should not pass unremarked.
		tel.Warn("", "the Platform is on TCP rather than a socket, "+
			"so it can be reached without passing through this process",
			supervisor.String("address", cfg.Platform.Address()))
	}

	// What the Supervisor is doing to itself, shared by the components that do
	// it and the front door that draws it.
	activity := &supervisor.Activity{}

	// Where the Supervisor writes what went wrong, for the Platform to adopt
	// when it is up (ADR 0119). A file rather than a call, because the findings
	// worth having most are the ones made while the Platform is not there.
	spool := supervisor.OpenSpool(cfg.StateDir, tel)

	// Registration order is stop order — the Platform first, the interface
	// last. Adding a third child means deciding where in that sequence it
	// belongs.
	manager := supervisor.NewManager(cfg.BootID, tel)
	manager.SetSpool(spool)

	// The Generations this install holds, and the machinery to acquire one.
	// Built before the children are registered because it is what decides what
	// they run.
	provisioner, err := supervisor.OpenProvisioner(cfg, manager, activity, spool, tel)
	if err != nil {
		return err
	}

	// Which Generation the children belong to, so a Platform can say which one
	// it is — the fact that settles an upgrade request (ADR 0129). Set before
	// any child starts, and again by an activation.
	if version, ok := provisioner.ActiveGeneration(); ok {
		manager.SetGenerationID(version)
	}

	if err := manager.Add(supervisor.ChildSpec{
		Name:       supervisor.PlatformChildName,
		Command:    childCommand(os.Getenv(platformCommandEnv), provisioner, supervisor.PlatformChildName),
		Managed:    !external(platformExternalEnv),
		WorkingDir: os.Getenv(platformDirEnv),
		// Told where to listen, rather than configured independently: the two
		// halves have to agree, and a Supervisor dialling a socket its child
		// was never asked to create fails with no useful error.
		Env: []string{
			platformAddrEnv + "=" + cfg.Platform.ListenSpec(),
			platformHandoffAddrEnv + "=" + cfg.PlatformHandoff.ListenSpec(),
			// Where to adopt the Supervisor's findings from (ADR 0119). Told
			// rather than configured at both ends, like the sockets above: two
			// halves that have to agree must not be able to disagree.
			supervisor.SpoolEnv + "=" + spool.Path(),
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
		Name:       supervisor.ShellChildName,
		Command:    childCommand(os.Getenv(shellCommandEnv), provisioner, supervisor.ShellChildName),
		Managed:    !external(shellExternalEnv),
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
	frontDoor.Activity = activity

	certificate, generated, err := supervisor.TLSCertificate(cfg)
	if err != nil {
		return err
	}
	if generated {
		// Said at every boot on purpose. A self-signed certificate is a
		// stopgap, and an install that has quietly been on one for a year is
		// the failure this warning exists to prevent.
		tel.Warn("", "serving a self-signed certificate generated for this boot; "+
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

	// No boot id among the fields: every record already carries it, and a
	// second copy is a second thing to keep in step.
	tel.Info("", "listening",
		supervisor.String("address", cfg.ListenAddr),
		supervisor.String("platform", cfg.Platform.Address()),
		supervisor.String("shell", cfg.Shell.Address()))

	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	// **Provisioning happens after the door is open, and that ordering is the
	// whole reason the recovery page exists.** A first boot fetches two
	// binaries over whatever connection the box has; somebody who opens the URL
	// during those minutes must see the install happening rather than a refused
	// connection. Doing this before serving would make the one screen written
	// for this moment unreachable in it.
	//
	// Its failure is logged and not returned. A Supervisor that cannot
	// provision must keep running, because the front door is what says why —
	// exiting would replace an explanation with a closed port.
	if err := provisioner.EnsureGeneration(ctx); err != nil {
		tel.Error("", err.Error())
		// **And on the screen, because a log is not a surface an install in
		// this state has.** Nothing is serving and nothing is going to, so the
		// recovery page is the only thing anybody can reach — and left to the
		// health inference it would say "Starting" forever, which is the one
		// answer that is both true and useless. Reported and never cleared: the
		// situation does not resolve on its own.
		activity.Report(supervisor.PhaseDegraded, "", err.Error(), -1)
	}

	// **The upgrade loop starts after provisioning, not before.** A first boot
	// is already fetching a Generation; a second fetcher asking the same
	// catalogue while it does would be two downloads and one confusing screen.
	// By here there is either a Generation or a recorded reason there is not.
	go (&supervisor.UpgradeWatch{
		Updater: provisioner.Update(),
		Handoff: cfg.PlatformHandoff,
		Spool:   spool,
		Tel:     tel,
	}).Run(ctx)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	tel.Info("", "shutting down")

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
	// (contracts#5) never completes on its own, so waiting for it would mean
	// never shutting down.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), frontDoorDrain)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		tel.Warn("", "the front door did not drain cleanly", supervisor.Err(err))
	}

	tel.Info("", "stopped")
	return nil
}

// frontDoorDrain bounds how long the door takes to close once both children
// are already gone. Everything still connected at that point is being served
// the holding page, so this is short.
const frontDoorDrain = 10 * time.Second

// childCommand decides what a child runs.
//
// **The environment wins over the Generation**, which is the right way round
// for the two cases that set it: a development stack pointing at binaries it
// built, and a deployment managing its own processes (ADR 0121's DIY path).
// Both are deliberate acts by somebody who knows what they want run, and
// neither should be quietly overridden by a Generation that happens to be on
// disk.
//
// Empty from both is a child the Supervisor fronts but does not own, which is
// also how a first boot starts: there is nothing to run until a Generation
// arrives, and the Activator sets the command when one does.
func childCommand(fromEnv string, p *supervisor.Provisioner, child string) []string {
	if argv := fields(fromEnv); len(argv) > 0 {
		return argv
	}
	return p.CommandFor(child)
}

// external reads the opt-out. Anything other than empty means the deployment
// runs this child itself.
func external(env string) bool { return strings.TrimSpace(os.Getenv(env)) != "" }

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
