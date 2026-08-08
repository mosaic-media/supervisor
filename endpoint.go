// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// unixHost is the authority used in URLs that are really Unix sockets.
//
// A Unix socket has no host, but net/http insists on one to build a request:
// it goes into the Host header and the request line, and it is what a
// connection pool keys on. A fixed placeholder is correct because the dialer
// ignores it entirely — every connection for this endpoint goes to the same
// socket path — and a *recognisable* placeholder is what stops it reading as a
// hostname somebody failed to resolve when it turns up in an error message.
const unixHost = "mosaic.socket.invalid"

// Endpoint is a resolved upstream: how to reach it, and the URL to hand a
// proxy or a probe.
//
// It exists because ADR 0120 gives the same upstream two possible transports.
// Everything above it — the front door, the readiness probes — is written
// against this rather than against a URL, so neither has to know which of the
// two it got.
type Endpoint struct {
	// network and address are what a dialer needs: ("unix", "/run/…/x.sock")
	// or ("tcp", "127.0.0.1:8081").
	network string
	address string
	// scheme and host are what a URL needs.
	scheme string
	host   string
}

// ParseEndpoint reads an upstream address.
//
//	unix:///run/mosaic/platform.sock   a Unix socket (the shipped shape)
//	http://127.0.0.1:8081              TCP, for a split deployment or dev
//
// A `unix://` URL keeps the whole endpoint in one setting, so a deployment
// says what it is in one place rather than in an address plus a mode flag that
// can disagree with it.
func ParseEndpoint(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("not a URL: %w", err)
	}

	switch parsed.Scheme {
	case "unix":
		// url.Parse puts the path in Path for unix:///a/b — an absolute path
		// is required, because a relative socket path resolves against a
		// working directory that is not the same for the Supervisor and the
		// child that binds it.
		if parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/") {
			return Endpoint{}, fmt.Errorf("needs an absolute socket path, got %q", raw)
		}
		return Endpoint{network: "unix", address: parsed.Path, scheme: "http", host: unixHost}, nil
	case "http", "https":
		if parsed.Host == "" {
			return Endpoint{}, fmt.Errorf("must include a host, got %q", raw)
		}
		return Endpoint{
			network: "tcp",
			address: parsed.Host,
			scheme:  parsed.Scheme,
			host:    parsed.Host,
		}, nil
	default:
		return Endpoint{}, fmt.Errorf("must be a unix://, http:// or https:// URL, got %q", raw)
	}
}

// IsUnix reports whether this endpoint is a socket, for the boot line that
// says which shape an install is running.
func (e Endpoint) IsUnix() bool { return e.network == "unix" }

// Address is the socket path or host:port, for logging.
func (e Endpoint) Address() string { return e.address }

// URL builds an absolute URL for a path on this endpoint.
func (e Endpoint) URL(path string) string {
	return e.scheme + "://" + e.host + path
}

// ProxyTarget is the URL a reverse proxy is pointed at.
func (e Endpoint) ProxyTarget() (*url.URL, error) {
	return url.Parse(e.scheme + "://" + e.host)
}

// DialContext dials this endpoint, ignoring the address net/http computed from
// the URL — for a socket that address is the placeholder host, and for TCP it
// is the same thing this holds anyway.
func (e Endpoint) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	d := &net.Dialer{Timeout: upstreamDialTimeout}
	return d.DialContext(ctx, e.network, e.address)
}

// Transport builds an http.Transport that reaches this endpoint.
func (e Endpoint) Transport() *http.Transport {
	return &http.Transport{
		DialContext:           e.DialContext,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		// No response header timeout: the push lane holds a response open
		// indefinitely by design, and a timeout here would sever it on a
		// schedule.
	}
}

// Client builds an HTTP client for probing this endpoint.
func (e Endpoint) Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: e.Transport()}
}

// ListenSpec is what a child should be told to bind: a socket path, or a
// host:port. The child chooses its transport from the shape of it, which keeps
// one setting per listener rather than an address plus a mode flag that can
// contradict it (ADR 0120).
func (e Endpoint) ListenSpec() string { return e.address }

// Probe is one thing to ask of one endpoint.
//
// It pairs the URL with the client that can reach it, because those cannot be
// separated once a socket is involved: the URL's host is a placeholder and
// only the client's dialer knows the path. A child with two listeners has two
// probes, and they are not interchangeable.
type Probe struct {
	url    string
	client *http.Client
}

// NewProbe builds a probe for a path on an endpoint.
func NewProbe(e Endpoint, path string) *Probe {
	return &Probe{url: e.URL(path), client: e.Client(readinessTimeout)}
}

// Target is the URL being asked, for logging and tests.
func (p *Probe) Target() string { return p.url }
