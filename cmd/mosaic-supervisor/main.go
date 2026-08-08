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
	// platformHandoffEnv points at the Platform's handoff listener. It is a
	// separate setting from the API URL because the two are different
	// listeners on different ports, and only the API is ever proxied.
	platformHandoffEnv = "MOSAIC_SUPERVISOR_PLATFORM_HANDOFF_URL"
)

// defaultPlatformHandoff is the Platform's MOSAIC_HEALTH_ADDR default.
const defaultPlatformHandoff = "http://127.0.0.1:8080"

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

	platformHandoffURL := strings.TrimRight(os.Getenv(platformHandoffEnv), "/")
	if platformHandoffURL == "" {
		platformHandoffURL = defaultPlatformHandoff
	}

	manager := supervisor.NewManager(cfg.BootID, log.Printf)
	if err := manager.Add(supervisor.ChildSpec{
		Name:    "platform",
		Command: fields(os.Getenv(platformCommandEnv)),
		// The Platform's own handoff listener, which is the private channel
		// between these two processes and is deliberately not routed through
		// the front door.
		//
		// /readyz, not /healthz: liveness says the process is answering, and
		// the Platform answers that while it is still running migrations. The
		// front door must not send a client to a Platform that cannot serve
		// it yet, so readiness is the question worth asking.
		ReadinessURL: platformHandoffURL + "/readyz",
	}); err != nil {
		return err
	}
	if err := manager.Add(supervisor.ChildSpec{
		Name:         "shell",
		Command:      fields(os.Getenv(shellCommandEnv)),
		ReadinessURL: cfg.ShellURL + "/healthz",
	}); err != nil {
		return err
	}

	go manager.Run(ctx)

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
		cfg.ListenAddr, cfg.BootID, cfg.PlatformURL, cfg.ShellURL)

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
		log.Printf("mosaic-supervisor: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

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
