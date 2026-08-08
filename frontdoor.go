// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Route prefixes that belong to the Platform. Everything not named here goes
// to the Shell, which is the right default: the Shell owns the URL space a
// person navigates, and the Platform owns a small, enumerable set of machine
// endpoints.
const (
	// connectPrefix covers both Connect services at once —
	// /mosaic.auth.v1.AuthService/ and /mosaic.session.v1.SessionService/ —
	// because protobuf's fully-qualified names all begin "mosaic.". Matching
	// the package prefix rather than the two service paths means a third
	// service does not need a change here to be reachable.
	connectPrefix = "/mosaic."
	artworkPath   = "/artwork"
	playbackPath  = "/playback/"

	// supervisorHealthPath is the Supervisor's own probe. It is under a
	// reserved prefix so it cannot collide with a Shell route, and it is the
	// one path the front door answers itself.
	supervisorHealthPath = "/supervisor/healthz"
)

// FrontDoor is the single public entry point (ADR 0005). It terminates TLS,
// routes, and answers for itself when an upstream is not there.
//
// It is a projection surface and nothing else: it holds no state about a
// session, reads no database, and never rewrites a body. The Platform's
// health/handoff listener is deliberately *not* routed — that is the private
// channel between these two processes and publishing it would put Generation
// and migration state on the public port.
type FrontDoor struct {
	platform *httputil.ReverseProxy
	shell    *httputil.ReverseProxy
	cfg      Config
	started  time.Time
	// health reports what the Supervisor knows about its children. It is a
	// function so the process manager can own the state without the front
	// door depending on it.
	health func() Health
}

// Health is what the Supervisor knows about itself and its children.
type Health struct {
	Status   string          `json:"status"`
	Service  string          `json:"service"`
	BootID   string          `json:"bootId"`
	Uptime   int64           `json:"uptimeSeconds"`
	Children []ChildSnapshot `json:"children"`
}

// NewFrontDoor builds the router. health may be nil when nothing is
// supervising children, in which case the probe reports the Supervisor alone.
func NewFrontDoor(cfg Config, health func() Health) (*FrontDoor, error) {
	platformURL, err := url.Parse(cfg.PlatformURL)
	if err != nil {
		return nil, fmt.Errorf("platform URL: %w", err)
	}
	shellURL, err := url.Parse(cfg.ShellURL)
	if err != nil {
		return nil, fmt.Errorf("shell URL: %w", err)
	}

	fd := &FrontDoor{cfg: cfg, started: time.Now(), health: health}
	fd.platform = fd.proxyTo(platformURL, fd.platformUnavailable)
	fd.shell = fd.proxyTo(shellURL, fd.shellUnavailable)
	return fd, nil
}

// proxyTo builds a reverse proxy with the settings that matter here.
func (f *FrontDoor) proxyTo(target *url.URL, onError func(http.ResponseWriter, *http.Request, error)) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Immediate flush. The session transport has a server-push lane
	// (ADR 0041), and a proxy that buffers turns "unprompted push" into
	// "arrives when the buffer fills" — which looks like the push lane not
	// working rather than like a proxy setting.
	proxy.FlushInterval = -1

	inner := proxy.Director
	proxy.Director = func(r *http.Request) {
		inner(r)
		// The upstreams are behind TLS termination and must be able to tell.
		// Without this the Platform cannot build a correct absolute URL for
		// anything it signs or redirects to.
		r.Header.Set("X-Forwarded-Proto", forwardedProto(r))
		if r.Host != "" {
			r.Header.Set("X-Forwarded-Host", r.Host)
		}
	}

	proxy.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: upstreamDialTimeout}).DialContext,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		// No response header timeout: the push lane holds a response open
		// indefinitely by design, and a timeout here would sever it on a
		// schedule.
	}
	proxy.ErrorHandler = onError
	return proxy
}

func forwardedProto(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func (f *FrontDoor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == supervisorHealthPath:
		f.serveHealth(w, r)
	case isPlatformPath(r.URL.Path):
		f.platform.ServeHTTP(w, r)
	default:
		f.shell.ServeHTTP(w, r)
	}
}

// isPlatformPath is the complete enumeration of what goes to the Platform.
func isPlatformPath(p string) bool {
	if p == artworkPath {
		return true
	}
	if len(p) >= len(playbackPath) && p[:len(playbackPath)] == playbackPath {
		return true
	}
	return len(p) >= len(connectPrefix) && p[:len(connectPrefix)] == connectPrefix
}

// serveHealth answers for the Supervisor. It reports its children's states but
// its own status stays "ok" while it is answering: the Supervisor being up is
// exactly the condition this endpoint exists to report, and folding a child's
// failure into it would make an orchestrator restart the one process that is
// working.
func (f *FrontDoor) serveHealth(w http.ResponseWriter, r *http.Request) {
	health := Health{
		Status:  "ok",
		Service: "mosaic-supervisor",
		BootID:  f.cfg.BootID,
		Uptime:  int64(time.Since(f.started).Seconds()),
	}
	if f.health != nil {
		reported := f.health()
		health.Children = reported.Children
	}
	body, err := json.Marshal(health)
	if err != nil {
		http.Error(w, "health encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// platformUnavailable answers when the Platform is not reachable.
//
// It returns a status the *Shell* can interpret rather than a page, because
// the Shell is still loaded and already renders the offline state — that state
// is ADR 0031's stated exception and it is the richest available presentation
// layer in ADR 0005's ladder. Replacing a working client-side screen with a
// server-rendered error page would degrade further than the situation calls
// for.
func (f *FrontDoor) platformUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Connect reads this header for its own error mapping; "unavailable" is
	// the category the Platform itself would use, so a client sees one
	// vocabulary whether the failure came from the Platform or from in front
	// of it.
	w.Header().Set("Connect-Accept-Encoding", "identity")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    "unavailable",
		"message": "the Platform is not reachable from the Supervisor",
	})
}

// shellUnavailable answers when the Shell is not reachable.
//
// This is the bottom rung the Supervisor can still answer on: ADR 0005's
// embedded renderer, which exists for browser bootstrap and Shell failure.
// It is deliberately one static document with no scripting and no styling
// system — it must work when everything that would make it prettier is the
// thing that is broken.
//
// It is NOT Recovery SDUI. ADR 0005 says the Supervisor emits Recovery SDUI
// and the Shell renders it; neither the emitter nor the renderer is built, and
// this page is not a substitute for either. It says so, rather than looking
// like the feature.
func (f *FrontDoor) shellUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(bootstrapPage))
}

// bootstrapPage is intentionally tiny and dependency-free.
const bootstrapPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Mosaic</title></head>
<body style="font:16px/1.6 system-ui,sans-serif;max-width:34rem;margin:20vh auto;padding:0 1.5rem">
<h1 style="font-size:1.25rem">Mosaic is starting</h1>
<p>The interface is not running yet. The Supervisor is up and will serve it as soon as it is.</p>
<p><a href="/">Try again</a></p>
</body></html>
`
