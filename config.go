// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

// Package supervisor is Mosaic's host-level process manager and single front
// door (ADR 0004, ADR 0005).
//
// What it is responsible for has shrunk a long way from those records:
// extension modules are the Platform's throughout (ADR 0079) and per-install
// builds were deleted in favour of a CI-built binary (ADR 0063), so the Build
// Pipeline, Module resolution and Generation *building* are not here and are
// not coming. What is left is process lifecycle, the front door, and
// activating an artefact somebody else built.
package supervisor

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Environment variable names, read in one place so the configuration surface
// is greppable.
const (
	listenAddrEnv  = "MOSAIC_SUPERVISOR_ADDR"
	certFileEnv    = "MOSAIC_SUPERVISOR_TLS_CERT"
	keyFileEnv     = "MOSAIC_SUPERVISOR_TLS_KEY"
	platformURLEnv = "MOSAIC_SUPERVISOR_PLATFORM_URL"
	shellURLEnv    = "MOSAIC_SUPERVISOR_SHELL_URL"
	bootIDEnv      = "MOSAIC_BOOT_ID"
)

const (
	defaultListenAddr = ":8443"
	// The two processes the Supervisor fronts, on loopback. They are not
	// published to the host in a deployed install: the whole point of a
	// single front door is that these are the only way in and it is not one
	// of them.
	defaultPlatformURL = "http://127.0.0.1:8081"
	defaultShellURL    = "http://127.0.0.1:8090"
)

// Config is what an operator can set.
type Config struct {
	// ListenAddr is the one public port. There is deliberately no second
	// listener: ADR 0005 makes this the only public HTTP entry point, and a
	// second one is how the Platform ends up reachable around it.
	ListenAddr string
	// CertFile and KeyFile point at an operator-supplied certificate. Both
	// empty means a self-signed pair is generated for this boot — usable on
	// a LAN, and honest about what it is.
	CertFile string
	KeyFile  string
	// PlatformURL and ShellURL are the upstreams.
	PlatformURL string
	ShellURL    string
	// BootID names one start of this process. The Supervisor is the process
	// that *mints* it and hands it to its children (ADR 0060), so unlike the
	// Platform and the Shell it adopts an inbound one only when something is
	// supervising the Supervisor.
	BootID string
}

// LoadConfig reads configuration from the environment, validating rather than
// coercing: an unusable upstream is an error at startup, not a 502 later that
// points at the wrong layer.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:  strings.TrimSpace(getenv(listenAddrEnv)),
		CertFile:    strings.TrimSpace(getenv(certFileEnv)),
		KeyFile:     strings.TrimSpace(getenv(keyFileEnv)),
		PlatformURL: strings.TrimSpace(getenv(platformURLEnv)),
		ShellURL:    strings.TrimSpace(getenv(shellURLEnv)),
		BootID:      strings.TrimSpace(getenv(bootIDEnv)),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.PlatformURL == "" {
		cfg.PlatformURL = defaultPlatformURL
	}
	if cfg.ShellURL == "" {
		cfg.ShellURL = defaultShellURL
	}
	if cfg.BootID == "" {
		cfg.BootID = NewID()
	}

	// One of the pair without the other is a misconfiguration that would
	// otherwise fall back to self-signed and look like it worked, which is
	// the worst outcome: a deployment quietly serving a certificate nobody
	// meant to serve.
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return Config{}, fmt.Errorf("%s and %s must be set together", certFileEnv, keyFileEnv)
	}
	for name, raw := range map[string]string{platformURLEnv: cfg.PlatformURL, shellURLEnv: cfg.ShellURL} {
		if err := validateUpstream(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	cfg.PlatformURL = strings.TrimRight(cfg.PlatformURL, "/")
	cfg.ShellURL = strings.TrimRight(cfg.ShellURL, "/")
	return cfg, nil
}

// LoadConfigFromEnv is LoadConfig against the process environment.
func LoadConfigFromEnv() (Config, error) { return LoadConfig(os.Getenv) }

// TLSConfigured reports whether an operator supplied a certificate.
func (c Config) TLSConfigured() bool { return c.CertFile != "" && c.KeyFile != "" }

func validateUpstream(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("must be an http:// or https:// URL, got %q", raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("must include a host, got %q", raw)
	}
	return nil
}

// Timeouts used by the front door. Collected so the reasoning is in one place
// rather than scattered as literals.
const (
	// upstreamDialTimeout is short: both upstreams are on loopback, so a slow
	// dial means "not listening" rather than "far away".
	upstreamDialTimeout = 2 * time.Second
	// readinessInterval is how often a child is probed once it is running.
	readinessInterval = 2 * time.Second
	// readinessTimeout bounds one probe.
	readinessTimeout = 3 * time.Second
)
