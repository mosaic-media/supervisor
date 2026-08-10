// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

// Package supervisor is Mosaic's host-level process manager and single front
// door (supervisor#1, supervisor#2).
//
// What it is responsible for has shrunk a long way from those records:
// extension modules are the Platform's throughout (platform#49) and per-install
// builds were deleted in favour of a CI-built binary (platform#38), so the Build
// Pipeline, Module resolution and Generation *building* are not here and are
// not coming. What is left is process lifecycle, the front door, and
// activating an artefact somebody else built.
package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Environment variable names, read in one place so the configuration surface
// is greppable.
const (
	listenAddrEnv = "MOSAIC_SUPERVISOR_ADDR"
	certFileEnv   = "MOSAIC_SUPERVISOR_TLS_CERT"
	keyFileEnv    = "MOSAIC_SUPERVISOR_TLS_KEY"
	runtimeDirEnv = "MOSAIC_RUNTIME_DIR"
	// The upstreams. Each accepts a unix:// or an http:// URL, and each
	// defaults to a socket under the runtime directory.
	platformURLEnv     = "MOSAIC_SUPERVISOR_PLATFORM_URL"
	platformHandoffEnv = "MOSAIC_SUPERVISOR_PLATFORM_HANDOFF_URL"
	shellURLEnv        = "MOSAIC_SUPERVISOR_SHELL_URL"
	bootIDEnv          = "MOSAIC_BOOT_ID"
	// Where Generations are kept, and where a release is fetched from.
	stateDirEnv   = "MOSAIC_SUPERVISOR_STATE_DIR"
	releaseURLEnv = "MOSAIC_SUPERVISOR_RELEASE_URL"
	// How much the Supervisor records (supervisor#5). Levels are the only filtering
	// there is: no sampling, no per-component rules, nothing that could quietly
	// discard the one record that mattered.
	logLevelEnv = "MOSAIC_SUPERVISOR_LOG_LEVEL"
	// Where to send records as well as the file. Empty is the default and
	// means nowhere — see Config.OTLPEndpoint.
	otlpEndpointEnv = "MOSAIC_SUPERVISOR_OTLP_ENDPOINT"
)

const (
	defaultListenAddr = ":8443"
	// defaultRuntimeDir holds the children's sockets. /run is the conventional
	// home for an artefact that should not survive a reboot, and a socket left
	// behind by a killed process is exactly that.
	defaultRuntimeDir = "/run/mosaic"
	// defaultStateDir holds the Generations and the pointer at the live one.
	// /var/lib rather than /run, and the difference is the whole point: this
	// must survive a reboot or every restart would be a first boot.
	defaultStateDir = "/var/lib/mosaic"
)

// Socket file names within the runtime directory.
//
// The Platform's two surfaces are separate sockets because they are separate
// audiences: collapsing them would publish the handoff channel — Generation
// and migration state, deliberately outside the policy gate — to anything that
// could reach the client API (platform#75).
const (
	PlatformSocketName        = "platform.sock"
	PlatformHandoffSocketName = "platform-handoff.sock"
	ShellSocketName           = "shell.sock"
)

// Config is what an operator can set.
type Config struct {
	// ListenAddr is the one public port. There is deliberately no second
	// listener: supervisor#2 makes this the only public HTTP entry point, and a
	// second one is how the Platform ends up reachable around it.
	ListenAddr string
	// CertFile and KeyFile point at an operator-supplied certificate. Both
	// empty means a self-signed pair is generated for this boot — usable on
	// a LAN, and honest about what it is.
	CertFile string
	KeyFile  string
	// RuntimeDir holds the children's sockets. The Supervisor creates it,
	// because it is the process that starts before the others.
	RuntimeDir string
	// Platform, PlatformHandoff and Shell are the upstreams. Each is a Unix
	// socket in the shipped shape and may be TCP for a development stack that
	// runs the children as separate containers (platform#75).
	Platform        Endpoint
	PlatformHandoff Endpoint
	Shell           Endpoint
	// BootID names one start of this process. The Supervisor is the process
	// that *mints* it and hands it to its children (supervisor#5), so unlike the
	// Platform and the Shell it adopts an inbound one only when something is
	// supervising the Supervisor.
	BootID string
	// StateDir holds the Generations and the pointer at the live one. It
	// survives a reboot; the runtime directory deliberately does not.
	StateDir string
	// ReleaseURL is the directory holding the signed release catalogue —
	// index.json and index.json.sig.
	//
	// **There is no default, and that is a gap rather than a policy.** The
	// module registry's URL is compiled into the Platform because there is one
	// and it is published; the equivalent for Platform *releases* does not
	// exist yet — a Generation needs binaries from two repositories, and
	// nothing aggregates and signs them the way the registry does for modules.
	// Inventing a URL here would be a plausible-looking constant pointing at
	// nothing. Empty means an install boots on whatever it already has and says
	// so, which is the honest behaviour until there is somewhere to point.
	ReleaseURL string
	// LogLevel is the floor for the Supervisor's own records (supervisor#5). Info
	// by default, and an unrecognised name resolves to info rather than being
	// rejected: a typo in a log level must not silence the one process still
	// able to explain why nothing else started.
	LogLevel Level
	// OTLPEndpoint is an OpenTelemetry collector to send records to, as well as
	// the file — never instead of it (sdk#8).
	//
	// **Empty by default, and that is the decision rather than an omission.**
	// [supervisor#5](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0060-the-supervisor-observes-independently.md)
	// refused an exporter that needs a running collector, because the Supervisor
	// may assume nothing else is up — and that reasoning stands. What sdk#8
	// changed is that OTLP became an *option an operator takes*, so an install
	// with a collector gets its lifecycle in the same place as everything else
	// while an install without one is unaffected and unconfigured.
	//
	// The file is written either way. A collector that is down, unreachable or
	// misconfigured costs records in the collector and none on disk, which is
	// the only acceptable failure direction here.
	OTLPEndpoint string
}

// LoadConfig reads configuration from the environment, validating rather than
// coercing: an unusable upstream is an error at startup, not a 502 later that
// points at the wrong layer.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:   strings.TrimSpace(getenv(listenAddrEnv)),
		CertFile:     strings.TrimSpace(getenv(certFileEnv)),
		KeyFile:      strings.TrimSpace(getenv(keyFileEnv)),
		RuntimeDir:   strings.TrimSpace(getenv(runtimeDirEnv)),
		BootID:       strings.TrimSpace(getenv(bootIDEnv)),
		StateDir:     strings.TrimSpace(getenv(stateDirEnv)),
		ReleaseURL:   strings.TrimSpace(getenv(releaseURLEnv)),
		LogLevel:     ParseLevel(getenv(logLevelEnv)),
		OTLPEndpoint: strings.TrimSpace(getenv(otlpEndpointEnv)),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = defaultRuntimeDir
	}
	if cfg.StateDir == "" {
		cfg.StateDir = defaultStateDir
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

	// Each upstream defaults to a socket in the runtime directory and can be
	// overridden with a TCP URL. The default is the shipped shape, so a
	// deployment that has not thought about it gets the one with no second
	// door rather than the one that is easiest to reach.
	upstreams := []struct {
		env      string
		fallback string
		into     *Endpoint
	}{
		{platformURLEnv, socketURL(cfg.RuntimeDir, PlatformSocketName), &cfg.Platform},
		{platformHandoffEnv, socketURL(cfg.RuntimeDir, PlatformHandoffSocketName), &cfg.PlatformHandoff},
		{shellURLEnv, socketURL(cfg.RuntimeDir, ShellSocketName), &cfg.Shell},
	}
	for _, u := range upstreams {
		raw := strings.TrimSpace(getenv(u.env))
		if raw == "" {
			raw = u.fallback
		}
		endpoint, err := ParseEndpoint(strings.TrimRight(raw, "/"))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", u.env, err)
		}
		*u.into = endpoint
	}
	return cfg, nil
}

// socketURL names a socket inside the runtime directory.
func socketURL(dir, name string) string {
	return "unix://" + filepath.Join(dir, name)
}

// LoadConfigFromEnv is LoadConfig against the process environment.
func LoadConfigFromEnv() (Config, error) { return LoadConfig(os.Getenv) }

// TLSConfigured reports whether an operator supplied a certificate.
func (c Config) TLSConfigured() bool { return c.CertFile != "" && c.KeyFile != "" }

// PrepareRuntimeDir creates the directory the children's sockets live in.
//
// The Supervisor owns it because it is the process that starts before the
// others. `0700` is the access control platform#75 relies on: a socket's own mode
// governs connecting to it, but a directory nobody else may traverse is what
// stops another user on the box enumerating what is there.
func PrepareRuntimeDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the runtime directory %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and one created by
	// an earlier version — or by a distribution package — may be wider.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("securing the runtime directory %s: %w", dir, err)
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
