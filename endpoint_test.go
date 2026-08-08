// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveOnSocket stands up an HTTP server on a real Unix socket, which is the
// only way to prove this works: parsing a `unix://` URL says nothing about
// whether a request ever reaches the other end.
func serveOnSocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	// Short, because a Unix socket path has a hard length limit around 100
	// bytes and a long TempDir plus a long name silently exceeds it.
	dir, err := os.MkdirTemp("", "mos")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return path
}

func TestARequestReachesAUnixSocket(t *testing.T) {
	path := serveOnSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached "+r.URL.Path)
	}))

	endpoint, err := ParseEndpoint("unix://" + path)
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	if !endpoint.IsUnix() {
		t.Fatal("a unix:// URL did not produce a socket endpoint")
	}

	resp, err := endpoint.Client(readinessTimeout).Get(endpoint.URL("/readyz"))
	if err != nil {
		t.Fatalf("request over the socket: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "reached /readyz" {
		t.Errorf("got %q", body)
	}
}

// The front door has to proxy over a socket, which is the whole point: the
// upstream URL carries a placeholder host and only the dialer knows the path.
func TestTheFrontDoorProxiesOverSockets(t *testing.T) {
	platform := serveOnSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "platform")
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	shell := serveOnSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream", "shell")
	}))

	fd, err := NewFrontDoor(Config{
		Platform: mustEndpoint(t, "unix://"+platform),
		Shell:    mustEndpoint(t, "unix://"+shell),
		BootID:   "boot-1",
	}, nil)
	if err != nil {
		t.Fatalf("NewFrontDoor: %v", err)
	}

	for path, want := range map[string]string{
		"/mosaic.auth.v1.AuthService/Bootstrap": "platform",
		"/library":                              "shell",
	} {
		rec := httptest.NewRecorder()
		fd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Result().Header.Get("X-Upstream"); got != want {
			t.Errorf("%s reached %q, want %q", path, got, want)
		}
	}
}

// A socket's own mode is the access control ADR 0120 rests on, so the
// placeholder host must never leak into somewhere it would be taken for a real
// one — it is what a connection pool keys on and what appears in an error.
func TestTheSocketPlaceholderHostIsUnresolvable(t *testing.T) {
	endpoint, err := ParseEndpoint("unix:///run/mosaic/platform.sock")
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	// .invalid is reserved by RFC 2606 and can never resolve, so a dialer that
	// ignored the endpoint and used the URL's host would fail loudly rather
	// than reach something real.
	if !strings.HasSuffix(endpoint.URL("/x"), ".invalid/x") {
		t.Errorf("placeholder host is not an unresolvable name: %s", endpoint.URL("/x"))
	}
}

// A path is only usable if both halves agree on it, and the child is told what
// to bind by the same value the Supervisor dials.
func TestTheChildIsToldExactlyWhatTheSupervisorDials(t *testing.T) {
	endpoint, err := ParseEndpoint("unix:///run/mosaic/platform.sock")
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	if got := endpoint.ListenSpec(); got != "/run/mosaic/platform.sock" {
		t.Errorf("the child would bind %q, which is not what is dialled", got)
	}

	tcp, err := ParseEndpoint("http://127.0.0.1:8081")
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	if got := tcp.ListenSpec(); got != "127.0.0.1:8081" {
		t.Errorf("a TCP child would bind %q", got)
	}
}
