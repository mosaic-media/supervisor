// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/mosaic-media/contracts/gen/mosaic/auth/v1"
	"github.com/mosaic-media/contracts/gen/mosaic/auth/v1/authv1connect"
	sessionv1 "github.com/mosaic-media/contracts/gen/mosaic/session/v1"
	"github.com/mosaic-media/contracts/gen/mosaic/session/v1/sessionv1connect"
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
// return something the client can interpret rather than replacing a working
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
	if !strings.Contains(string(body), "Mosaic") {
		t.Errorf("want the recovery page, got:\n%s", body)
	}

	// It must still say what is happening with scripting off, and that property
	// is not to be relaxed as the page gains scripts.
	//
	// The state is server-rendered into the page body rather than into
	// <noscript>, which is what makes it work: a browser with scripting off
	// shows that content from the ordinary DOM, and the meta refresh inside
	// <noscript> is what keeps it current. htmx then replaces the same element
	// when scripting is on.
	page := string(body)
	if !strings.Contains(page, "Starting") {
		t.Errorf("the page does not report the state on first paint:\n%s", page)
	}
	// The property, tested directly: remove every script element and the state
	// must survive. Cutting at the first `<script` would test nothing useful —
	// the vendored tags are in the head, above content that does not depend on
	// them.
	if withoutScripts := stripScripts(page); !strings.Contains(withoutScripts, "Starting") {
		t.Errorf("the state is only reachable after a script runs — with scripting off "+
			"the page would show one empty frame forever:\n%s", withoutScripts)
	}
	if !strings.Contains(between(page, "<noscript>", "</noscript>"), "http-equiv=\"refresh\"") {
		t.Error("no refresh for a browser with scripting off — the whole content is a state that changes")
	}
}

// stripScripts removes every script element, so what is left is what a browser
// with scripting off would show.
func stripScripts(page string) string {
	var b strings.Builder
	for rest := page; ; {
		open := strings.Index(rest, "<script")
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:open])
		after := rest[open:]
		if end := strings.Index(after, "</script>"); end >= 0 {
			rest = after[end+len("</script>"):]
			continue
		}
		// A self-closing or unterminated tag: drop to the end of it.
		if end := strings.Index(after, ">"); end >= 0 {
			rest = after[end+1:]
			continue
		}
		return b.String()
	}
}

// between returns what sits between two markers, for reading one block out of a
// page without a parser.
func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
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

// The Supervisor answers the Platform's own routes while the Platform is down
// (supervisor#7), so a client has one SDUI source and no rule for choosing
// between two.
//
// It is checked through a real Connect client against a real server rather than
// by hand-shaping a request, because the thing being asserted is that a client
// which knows nothing about the Supervisor gets an answer it can use.
func TestTheSupervisorAnswersThePlatformsRoutesWhenItIsDown(t *testing.T) {
	front := frontDoor(t, "http://127.0.0.1:1", "http://127.0.0.1:1", func() Health {
		return Health{Children: []ChildSnapshot{{Name: PlatformChildName, State: ChildStarting}}}
	})
	server := httptest.NewServer(front)
	t.Cleanup(server.Close)

	client := authv1connect.NewAuthServiceClient(server.Client(), server.URL)
	res, err := client.Bootstrap(context.Background(), connect.NewRequest(&authv1.BootstrapRequest{}))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.Msg.UiNode.GetType() != "Box" {
		t.Errorf("root node is %q, want the Box primitive", res.Msg.UiNode.GetType())
	}
	if !strings.Contains(treeText(res.Msg.UiNode), "boot-1") {
		t.Error("the boot id is not on the screen")
	}
	// No skin and no definitions, and both absences are correct: the skin is
	// Platform-delivered and the tree is primitives only. A client draws it
	// with what it shipped with.
	if len(res.Msg.Tokens) != 0 || len(res.Msg.Definitions) != 0 {
		t.Error("the Supervisor answered with a skin or a definition library, neither of which it has")
	}
}

// And it stops the moment the Platform is serving. The switch is the front
// door's alone; nothing on the client participates in it.
func TestTheSupervisorStandsAsideWhenThePlatformIsServing(t *testing.T) {
	platform, shell := upstreams(t)
	front := frontDoor(t, platform.URL, shell.URL, func() Health {
		return Health{Children: []ChildSnapshot{{Name: PlatformChildName, State: ChildReady}}}
	})
	rec := httptest.NewRecorder()
	front.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mosaic.auth.v1.AuthService/Bootstrap", nil))
	if got := rec.Header().Get("X-Upstream"); got != "platform" {
		t.Errorf("a serving Platform was answered by %q instead of being proxied", got)
	}
}

// The handover: the push lane ends when the Platform is back, and the client's
// ordinary reconnect does the rest. Nothing polls and nobody is told to refresh.
func TestThePushLaneEndsWhenThePlatformReturns(t *testing.T) {
	children := &atomic.Value{}
	children.Store([]ChildSnapshot{{Name: PlatformChildName, State: ChildStarting}})
	front := frontDoor(t, "http://127.0.0.1:1", "http://127.0.0.1:1", func() Health {
		return Health{Children: children.Load().([]ChildSnapshot)}
	})
	server := httptest.NewServer(front)
	t.Cleanup(server.Close)

	client := sessionv1connect.NewSessionServiceClient(server.Client(), server.URL)
	stream, err := client.Subscribe(context.Background(), connect.NewRequest(&sessionv1.SubscribeRequest{}))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	if !stream.Receive() {
		t.Fatalf("no shell was pushed: %v", stream.Err())
	}
	shellMsg, ok := stream.Msg().Body.(*sessionv1.ServerMessage_Shell)
	if !ok {
		t.Fatalf("first message is %T, want a shell update", stream.Msg().Body)
	}
	if !strings.Contains(treeText(shellMsg.Shell.UiNode), "Mosaic") {
		t.Error("the pushed shell is not the Supervisor's screen")
	}
	// The resume cursor is the Platform's to define. A number minted here would
	// be presented to it as a position in a stream it never sent.
	if stream.Msg().Seq != 0 {
		t.Errorf("seq = %d — a cursor from this stream would be replayed against the Platform's",
			stream.Msg().Seq)
	}

	children.Store([]ChildSnapshot{{Name: PlatformChildName, State: ChildReady}})
	deadline := time.Now().Add(10 * time.Second)
	for stream.Receive() {
		if time.Now().After(deadline) {
			t.Fatal("the stream did not end when the Platform came back")
		}
	}
	if err := stream.Err(); err != nil {
		t.Errorf("the stream ended with an error rather than cleanly: %v", err)
	}
}

// A credential is never answered by something that cannot check it, and the
// refusal is Unavailable rather than Unauthenticated — a client answers the
// latter by discarding its refresh chain (platform#58), which would sign somebody
// out because their server restarted.
func TestCredentialCallsAreRefusedAsUnavailable(t *testing.T) {
	front := frontDoor(t, "http://127.0.0.1:1", "http://127.0.0.1:1", func() Health {
		return Health{Children: []ChildSnapshot{{Name: PlatformChildName, State: ChildStarting}}}
	})
	server := httptest.NewServer(front)
	t.Cleanup(server.Close)

	client := authv1connect.NewAuthServiceClient(server.Client(), server.URL)
	for name, call := range map[string]func() error{
		"SignIn": func() error {
			_, err := client.SignIn(context.Background(), connect.NewRequest(&authv1.SignInRequest{}))
			return err
		},
		"Refresh": func() error {
			_, err := client.Refresh(context.Background(), connect.NewRequest(&authv1.RefreshRequest{}))
			return err
		},
		"SignOut": func() error {
			_, err := client.SignOut(context.Background(), connect.NewRequest(&authv1.SignOutRequest{}))
			return err
		},
	} {
		err := call()
		if err == nil {
			t.Errorf("%s succeeded against a Supervisor that cannot check a credential", name)
			continue
		}
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Errorf("%s failed with %v, want unavailable — anything else risks a spurious sign-out", name, got)
		}
	}
}

// The phase is inferred from the health the front door already has, so a child
// that has passed its ceiling reads as degraded rather than as still starting —
// the two things a person does different things about.
func TestTheRecoveryUIReportsDegradedFromTheHealthReport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		children []ChildSnapshot
		want     string
	}{
		{"starting", []ChildSnapshot{{Name: PlatformChildName, State: ChildStarting}}, "Starting"},
		{"degraded", []ChildSnapshot{{Name: PlatformChildName, State: ChildStarting, Unrecoverable: true}}, "not running"},
		{"ready", []ChildSnapshot{{Name: PlatformChildName, State: ChildReady}}, "Mosaic is running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			children := tc.children
			front := frontDoor(t, "http://127.0.0.1:1", "http://127.0.0.1:1",
				func() Health { return Health{Children: children} })
			rec := httptest.NewRecorder()
			front.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, supervisorUIFragmentPath, nil))
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body does not say %q: %s", tc.want, rec.Body.String())
			}
		})
	}
}

// The stream carries a signal rather than content, and says so once the Shell
// is serving so the page can get out of the way.
func TestTheEventStreamSignalsChangeAndReadiness(t *testing.T) {
	state := &atomic.Value{}
	state.Store([]ChildSnapshot{{Name: "platform", State: ChildStarting}})
	front := frontDoor(t, "http://127.0.0.1:1", "http://127.0.0.1:1", func() Health {
		return Health{Children: state.Load().([]ChildSnapshot)}
	})

	srv := httptest.NewServer(front)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+supervisorUIEventsPath, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	// Named so a buffering proxy in front of a homelab does not hold every
	// event until the stream closes — a live page turned dead with no error.
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	events := bufio.NewReader(resp.Body)
	read := func(what string) string {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			line, err := events.ReadString('\n')
			if err != nil {
				t.Fatalf("reading %s: %v", what, err)
			}
			if strings.HasPrefix(line, "event: ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
		}
		t.Fatalf("timed out waiting for %s", what)
		return ""
	}

	// The first render is a change from nothing, so the stream opens by saying
	// so — a client that connected mid-state must not wait for the next one.
	if got := read("the initial state event"); got != "state" {
		t.Fatalf("first event = %q, want state", got)
	}

	state.Store([]ChildSnapshot{{Name: "platform", State: ChildReady}})
	// Ready ends the stream, having said so: the page reloads and the front
	// door hands it the Shell.
	for {
		if got := read("the ready event"); got == "ready" {
			break
		}
	}
	// The stream ends after saying so. Drained to EOF rather than reading one
	// line, because `event: ready` is followed by its own data line — reading
	// one more would succeed on a stream that had closed correctly.
	closed := make(chan error, 1)
	go func() {
		for {
			if _, err := events.ReadString('\n'); err != nil {
				closed <- err
				return
			}
		}
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Error("the stream stayed open after ready — a connection held for a client that has gone")
	}
}

// The vendored assets come out of the binary, so the page has nothing to fetch
// from anywhere else.
func TestTheVendoredAssetsAreServedFromTheBinary(t *testing.T) {
	front := frontDoor(t, "http://127.0.0.1:1", "http://127.0.0.1:1", nil)
	for _, name := range []string{"htmx.min.js", "sse.js"} {
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, recoveryAssetPrefix+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", name, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty", name)
		}
	}
	// And nothing else is reachable through that prefix.
	rec := httptest.NewRecorder()
	front.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, recoveryAssetPrefix+"../frontdoor.go", nil))
	if rec.Code == http.StatusOK {
		t.Error("the asset path served something outside the vendored set")
	}
}
