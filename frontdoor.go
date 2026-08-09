// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/mosaic-media/contracts/gen/mosaic/auth/v1/authv1connect"
	"github.com/mosaic-media/contracts/gen/mosaic/session/v1/sessionv1connect"
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
	// The embedded renderer's own two endpoints and its vendored assets.
	//
	// **These are the recovery page's, and nobody else's** (ADR 0123). There
	// was a `/supervisor/ui` beside them serving the same tree as JSON, on the
	// reasoning that a client should have to ask a different question to get a
	// Supervisor answer — so that it could never draw one believing it was
	// Mosaic. That reasoning was wrong in the way that matters: it made every
	// client responsible for knowing about two sources and for choosing between
	// them, which is a rule that has to be reimplemented, correctly, in every
	// client anyone ever writes. The Supervisor now answers the Platform's own
	// routes and says who is speaking in the payload, which costs one event
	// type and no client code at all.
	//
	// The fragment is that same tree rendered to HTML; the event stream carries
	// a signal that it changed, never the content.
	supervisorUIFragmentPath = "/supervisor/ui/fragment"
	supervisorUIEventsPath   = "/supervisor/ui/events"
	recoveryAssetPrefix      = "/supervisor/static/"
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
	// absent answers the Platform's own client surface while the Platform is
	// not serving (ADR 0123). It is the whole of "the Supervisor takes over":
	// one address, and the front door choosing who answers it.
	absent http.Handler
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
	fd := &FrontDoor{cfg: cfg, started: time.Now(), health: health}

	platform, err := fd.proxyTo(cfg.Platform, fd.platformUnavailable)
	if err != nil {
		return nil, fmt.Errorf("platform upstream: %w", err)
	}
	shell, err := fd.proxyTo(cfg.Shell, fd.shellUnavailable)
	if err != nil {
		return nil, fmt.Errorf("shell upstream: %w", err)
	}
	fd.platform, fd.shell = platform, shell

	// The Supervisor's stand-in for the Platform's client surface. Mounted on
	// the generated service paths, so the two services are reached at exactly
	// the addresses the Platform serves them at and a client cannot tell it is
	// being answered from somewhere else — which is the point (ADR 0123).
	mux := http.NewServeMux()
	authPath, authHandler := authv1connect.NewAuthServiceHandler(&authAbsent{state: fd.recoveryState})
	mux.Handle(authPath, authHandler)
	sessionPath, sessionHandler := sessionv1connect.NewSessionServiceHandler(&sessionAbsent{state: fd.recoveryState})
	mux.Handle(sessionPath, sessionHandler)
	fd.absent = mux

	return fd, nil
}

// platformServing reports whether the Platform is up enough to be proxied to.
//
// **Read from the health report rather than discovered by dialling.** The
// alternative — proxy and fall back when the dial fails — cannot work for the
// push lane: by the time a stream's dial has failed the request body is gone and
// there is nothing left to hand to a second handler. The cost is a lag of one
// health probe when the Platform dies, during which a client is told
// `unavailable` by the proxy's error handler and retries; that is the behaviour
// it had before this existed, so the lag degrades to the old path rather than to
// a new failure.
func (f *FrontDoor) platformServing() bool {
	if f.health == nil {
		// Nothing is supervising children, so there is no Platform whose
		// absence this could stand in for. Proxy and let the dial decide.
		return true
	}
	for _, c := range f.health().Children {
		if c.Name == PlatformChildName {
			return c.State == ChildReady
		}
	}
	// A Platform that is not in the report is not one this Supervisor started.
	// Same reasoning as above: proxy, and let the dial answer.
	return true
}

// proxyTo builds a reverse proxy with the settings that matter here.
func (f *FrontDoor) proxyTo(up Endpoint, onError func(http.ResponseWriter, *http.Request, error)) (*httputil.ReverseProxy, error) {
	target, err := up.ProxyTarget()
	if err != nil {
		return nil, err
	}
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

	// The dialer, not the URL, is what decides where this goes: for a socket
	// the URL's host is a placeholder and only this reaches the real address.
	proxy.Transport = up.Transport()
	proxy.ErrorHandler = onError
	return proxy, nil
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
	case r.URL.Path == supervisorUIFragmentPath:
		f.serveUIFragment(w, r)
	case r.URL.Path == supervisorUIEventsPath:
		f.serveUIEvents(w, r)
	case strings.HasPrefix(r.URL.Path, recoveryAssetPrefix):
		f.serveRecoveryAsset(w, r)
	case isPlatformPath(r.URL.Path):
		// **The switch, and the whole of what a client has to know about it:
		// nothing.** A client calls the one address it always calls. While the
		// Platform is serving this proxies; while it is not, the Supervisor
		// answers the same two services with its own state (ADR 0123), and
		// stops the moment the Platform is back.
		//
		// Only the client surface. Artwork and playback are bytes the
		// Supervisor does not have and must not pretend to — they keep the
		// proxy and its `unavailable`, which is the honest answer for a byte stream
		// that cannot be served.
		if isClientSurfacePath(r.URL.Path) && !f.platformServing() {
			f.absent.ServeHTTP(w, r)
			return
		}
		f.platform.ServeHTTP(w, r)
	default:
		f.shell.ServeHTTP(w, r)
	}
}

// isClientSurfacePath is the part of the Platform's routing the Supervisor can
// stand in for: the two Connect services a client draws itself from.
func isClientSurfacePath(p string) bool {
	return strings.HasPrefix(p, connectPrefix)
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
// It serves the embedded renderer, which fetches the Supervisor's Recovery
// SDUI and draws it — ADR 0005's third rung, where the second (the Shell
// rendering the same tree) is still unbuilt. What it draws is the same
// envelope a native client would render in its own skin; what it lacks is the
// design tokens, which are Platform-delivered and absent by definition here.
func (f *FrontDoor) shellUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(f.recoveryHTML()))
}

// recoveryPage is the embedded renderer (ADR 0005's third rung), served in
// place of the Shell when there is no Shell.
//
// **It is a page, not a message.** What it replaced was a static "Mosaic is
// starting" with a Try again link — accurate, and unable to say anything that
// changed, so a first boot downloading two binaries and a box that would never
// come up looked identical for as long as somebody was prepared to wait. This
// fetches /supervisor/ui and draws it, which is the same tree a native client
// renders in its own skin.
//
// **No framework and no build step**, deliberately: it draws when there is no
// Shell, so every dependency it had would be one more thing that must work on
// the worst day this install has.
//
//go:embed recoveryui/index.html
var recoveryPage string
