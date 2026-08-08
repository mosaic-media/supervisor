// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tlsStateStub marks a request as having arrived over TLS. Its contents do not
// matter — the front door only asks whether it is nil.
var tlsStateStub = tls.ConnectionState{}

// upstreams stands up a fake Platform and Shell, each echoing which one it is
// and the path it was asked for.
func upstreams(t *testing.T) (platform, shell *httptest.Server) {
	t.Helper()
	mark := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Upstream", name)
			w.Header().Set("X-Seen-Forwarded-Proto", r.Header.Get("X-Forwarded-Proto"))
			_, _ = io.WriteString(w, name+" "+r.URL.Path)
		}))
	}
	platform, shell = mark("platform"), mark("shell")
	t.Cleanup(platform.Close)
	t.Cleanup(shell.Close)
	return platform, shell
}

func frontDoor(t *testing.T, platformURL, shellURL string, health func() Health) *FrontDoor {
	t.Helper()
	fd, err := NewFrontDoor(Config{
		Platform: mustEndpoint(t, platformURL),
		Shell:    mustEndpoint(t, shellURL),
		BootID:   "boot-1",
	}, health)
	if err != nil {
		t.Fatalf("NewFrontDoor: %v", err)
	}
	return fd
}

func mustEndpoint(t *testing.T, raw string) Endpoint {
	t.Helper()
	e, err := ParseEndpoint(raw)
	if err != nil {
		t.Fatalf("ParseEndpoint(%q): %v", raw, err)
	}
	return e
}

func route(t *testing.T, fd *FrontDoor, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	fd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

// The complete enumeration of what reaches the Platform, and the default that
// everything else is the Shell's.
func TestRouting(t *testing.T) {
	platform, shell := upstreams(t)
	fd := frontDoor(t, platform.URL, shell.URL, nil)

	for path, want := range map[string]string{
		"/mosaic.auth.v1.AuthService/Bootstrap":  "platform",
		"/mosaic.session.v1.SessionService/Open": "platform",
		"/artwork":                               "platform",
		"/playback/abc/segment-1.ts":             "platform",

		"/":                "shell",
		"/library":         "shell",
		"/settings/people": "shell",
		"/assets/index.js": "shell",
	} {
		if got := route(t, fd, path).Header.Get("X-Upstream"); got != want {
			t.Errorf("%s went to %q, want %q", path, got, want)
		}
	}
}

// The Platform's health/handoff listener carries Generation, migration and
// config-activation state. It is the private channel between these two
// processes and must not become reachable from the public port just because it
// is called "/health".
func TestThePlatformsHandoffListenerIsNotPublished(t *testing.T) {
	platform, shell := upstreams(t)
	fd := frontDoor(t, platform.URL, shell.URL, nil)

	// The real set, as registered in internal/transport/health/handoff.go.
	// Written from that file rather than invented, because a made-up path
	// proves only that the router ignores made-up paths.
	for _, path := range []string{"/metadata", "/readyz", "/healthz", "/migrations", "/config"} {
		if got := route(t, fd, path).Header.Get("X-Upstream"); got == "platform" {
			t.Errorf("%s reached the Platform through the front door", path)
		}
	}
}

// The upstreams sit behind TLS termination and cannot tell without being told.
func TestUpstreamsAreToldTheySitBehindTLS(t *testing.T) {
	platform, shell := upstreams(t)
	fd := frontDoor(t, platform.URL, shell.URL, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mosaic.auth.v1.AuthService/Bootstrap", nil)
	req.TLS = &tlsStateStub
	fd.ServeHTTP(rec, req)

	if got := rec.Result().Header.Get("X-Seen-Forwarded-Proto"); got != "https" {
		t.Errorf("upstream saw X-Forwarded-Proto %q, want https", got)
	}
}

// When the Platform is down the Shell is still loaded and already renders the
// offline state, which is the richest available layer. The front door must
// return something the *client* can interpret rather than replacing a working
// screen with a server-rendered error page.
func TestPlatformDownAnswersTheClientNotThePage(t *testing.T) {
	_, shell := upstreams(t)
	// A port nothing listens on.
	fd := frontDoor(t, "http://127.0.0.1:1", shell.URL, nil)

	resp := route(t, fd, "/mosaic.auth.v1.AuthService/Bootstrap")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("want JSON for a client, got %q", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["code"] != "unavailable" {
		t.Errorf("want the Platform's own error vocabulary, got %q", body["code"])
	}
}

// When the Shell is down there is no client left to render anything, so this
// is the bottom rung the Supervisor can still answer on.
func TestShellDownServesTheBootstrapPage(t *testing.T) {
	platform, _ := upstreams(t)
	fd := frontDoor(t, platform.URL, "http://127.0.0.1:1", nil)

	resp := route(t, fd, "/library")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Mosaic is starting") {
		t.Errorf("want the bootstrap page, got:\n%s", body)
	}
	// It must not depend on anything that could be the broken thing.
	if strings.Contains(string(body), "<script") {
		t.Error("the bootstrap page must not depend on scripting")
	}
}

// The Supervisor being up is exactly what this endpoint reports, so a failed
// child must not turn it red — an orchestrator reading it would restart the
// one process that is working.
func TestHealthReportsChildrenWithoutGoingRed(t *testing.T) {
	platform, shell := upstreams(t)
	fd := frontDoor(t, platform.URL, shell.URL, func() Health {
		return Health{Children: []ChildSnapshot{
			{Name: "platform", State: ChildFailed, Restarts: 3, LastErr: "exit status 1"},
			{Name: "shell", State: ChildReady},
		}}
	})

	resp := route(t, fd, supervisorHealthPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var health Health
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if health.Status != "ok" {
		t.Errorf("the Supervisor reported %q while answering", health.Status)
	}
	if health.Service != "mosaic-supervisor" || health.BootID != "boot-1" {
		t.Errorf("unexpected identity: %+v", health)
	}
	if len(health.Children) != 2 || health.Children[0].State != ChildFailed {
		t.Errorf("children not reported faithfully: %+v", health.Children)
	}
}

// The health path is under a reserved prefix so a Shell route cannot collide
// with it — and so it does not get proxied away.
func TestHealthPathIsNotProxied(t *testing.T) {
	platform, shell := upstreams(t)
	fd := frontDoor(t, platform.URL, shell.URL, nil)

	if got := route(t, fd, supervisorHealthPath).Header.Get("X-Upstream"); got != "" {
		t.Errorf("the Supervisor's own probe was proxied to %q", got)
	}
}
