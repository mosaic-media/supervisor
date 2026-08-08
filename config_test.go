// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestDefaultsAreUsable(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("want %q, got %q", defaultListenAddr, cfg.ListenAddr)
	}
	if cfg.PlatformURL != defaultPlatformURL || cfg.ShellURL != defaultShellURL {
		t.Errorf("unexpected upstreams: %+v", cfg)
	}
	// The Supervisor is the process that mints the boot id its children adopt.
	if cfg.BootID == "" {
		t.Error("want a minted boot id")
	}
	if cfg.TLSConfigured() {
		t.Error("no certificate was configured, so TLSConfigured must be false")
	}
}

// Half a certificate pair must fail rather than silently fall back to
// self-signed, which would be a deployment serving a certificate nobody meant
// to serve.
func TestHalfACertificatePairIsRefused(t *testing.T) {
	for _, pairs := range []map[string]string{
		{certFileEnv: "/tmp/cert.pem"},
		{keyFileEnv: "/tmp/key.pem"},
	} {
		if _, err := LoadConfig(env(pairs)); err == nil {
			t.Errorf("%v was accepted", pairs)
		}
	}
}

func TestUpstreamsAreValidatedAtStartup(t *testing.T) {
	for _, raw := range []string{"127.0.0.1:8081", "/platform", "ftp://x", "http://"} {
		if _, err := LoadConfig(env(map[string]string{platformURLEnv: raw})); err == nil {
			t.Errorf("%q was accepted; an unusable upstream must fail at startup, not as a 502 later", raw)
		}
	}
}

func TestUpstreamTrailingSlashIsTrimmed(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{shellURLEnv: "http://127.0.0.1:9000/"}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// A readiness URL is built by concatenation; a trailing slash yields
	// "//healthz".
	if cfg.ShellURL != "http://127.0.0.1:9000" {
		t.Errorf("want the trailing slash trimmed, got %q", cfg.ShellURL)
	}
}

// A LAN install is reached by IP far more often than by name, and a
// certificate covering only "localhost" fails on the one address that matters.
func TestSelfSignedCoversLoopbackAndTheHostsAddresses(t *testing.T) {
	cert, generated, err := TLSCertificate(Config{})
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}
	if !generated {
		t.Fatal("want a generated certificate when none is configured")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	if err := parsed.VerifyHostname("localhost"); err != nil {
		t.Errorf("does not cover localhost: %v", err)
	}
	if err := parsed.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("does not cover 127.0.0.1: %v", err)
	}
	// Short-lived on purpose: a stopgap nobody has to decide to replace is
	// one an install sits on indefinitely.
	if life := time.Until(parsed.NotAfter); life > 180*24*time.Hour {
		t.Errorf("self-signed certificate lives %v — too long for a stopgap", life)
	}
	if !parsed.IsCA {
		t.Error("a self-signed leaf must be its own issuer to be importable as a trust anchor")
	}
}

// It has to actually serve a handshake, not merely parse.
func TestSelfSignedCertificateCompletesAHandshake(t *testing.T) {
	cert, _, err := TLSCertificate(Config{})
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()

	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	pool.AddCert(leaf)

	_, port, _ := net.SplitHostPort(listener.Addr().String())
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", port), &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("handshake against the generated certificate failed: %v", err)
	}
	conn.Close()
}
