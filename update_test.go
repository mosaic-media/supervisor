// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A fake Mosaic release host: a signed catalogue at /, and a signed release per
// version under /<version>/.
type releaseHost struct {
	server  *httptest.Server
	keys    *Keyring
	priv    ed25519.PrivateKey
	files   map[string][]byte
	ranLog  string
	serving *atomic.Bool
}

func newReleaseHost(t *testing.T, versions ...string) *releaseHost {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := &Keyring{}
	if err := keys.Trust("mosaic-release-dev", pub); err != nil {
		t.Fatal(err)
	}

	h := &releaseHost{keys: keys, priv: priv, files: map[string][]byte{}, ranLog: filepath.Join(t.TempDir(), "ran.log")}
	h.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, ok := h.files[strings.TrimPrefix(req.URL.Path, "/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(h.server.Close)

	serving := &atomic.Bool{}
	serving.Store(true)
	h.serving = serving

	for _, v := range versions {
		h.publish(t, v)
	}
	h.catalogue(t, versions...)
	return h
}

// publish adds a release: the artefact this host's build would take, a
// SHA256SUMS over it, and a signature.
func (h *releaseHost) publish(t *testing.T, version string) {
	t.Helper()
	name := ReleaseArtefactName("mosaic-platform")
	script := "#!/bin/sh\necho \"$0\" >> " + h.ranLog + "\necho 'platform " + version + " starting'\nexec sleep 300\n"

	sum := sha256.Sum256([]byte(script))
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"

	h.files[version+"/"+name] = []byte(script)
	h.files[version+"/"+checksumsName] = []byte(sums)
	h.files[version+"/"+signatureName] = ed25519.Sign(h.priv, []byte(sums))
}

// catalogue publishes the signed index naming the given versions.
func (h *releaseHost) catalogue(t *testing.T, versions ...string) {
	t.Helper()
	index := ReleaseIndex{Schema: ReleaseIndexSchema}
	for _, v := range versions {
		index.Releases = append(index.Releases, ReleaseEntry{Version: v, URL: h.server.URL + "/" + v})
	}
	body, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	h.files["index.json"] = body
	h.files["index.json.sig"] = ed25519.Sign(h.priv, body)
}

// updater wires the host to a fresh install with one child.
func (h *releaseHost) updater(t *testing.T) (*Updater, *Generations, *Manager) {
	t.Helper()
	g := generations(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !h.serving.Load() {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m := NewManager("boot-1", func(string, ...any) {})
	// A placeholder command: a fresh install has no Generation, so the first
	// upgrade is what gives the child something real to run.
	if err := m.Add(ChildSpec{
		Name:      "platform",
		Command:   []string{"sleep", "300"},
		Serving:   probeFor(t, srv.URL),
		StopGrace: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	waitUntil(t, "the placeholder child to serve", func() bool {
		return snapshotOf(m, "platform").State == ChildReady
	})

	act := &Activator{
		Generations:  g,
		Manager:      m,
		Targets:      []ActivationTarget{{Child: "platform", Binary: "mosaic-platform"}},
		ReadyTimeout: 3 * time.Second,
		Now:          func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) },
		Log:          func(string, ...any) {},
	}
	return &Updater{
		IndexURL:  h.server.URL,
		Fetcher:   &Fetcher{Generations: g, Keys: h.keys, Client: h.server.Client()},
		Activator: act,
		Keys:      h.keys,
		Log:       func(string, ...any) {},
	}, g, m
}

// The whole path, composed: check a signed catalogue, fetch the release it
// names, verify it, activate it, and end up running it.
func TestUpgradeFetchesVerifiesAndActivatesTheLatest(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0", "v0.2.0")
	u, g, m := h.updater(t)

	found, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if found.Latest.Version != "v0.2.0" || !found.Upgrade {
		t.Fatalf("Check = %+v", found)
	}
	if found.SignedBy != "mosaic-release-dev" {
		t.Errorf("catalogue signed by %q", found.SignedBy)
	}

	version, err := u.Upgrade(context.Background())
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if version != "v0.2.0" {
		t.Errorf("upgraded to %q", version)
	}
	if v, _ := g.Active(); v != "v0.2.0" {
		t.Errorf("active = %q", v)
	}
	argv, _ := m.CommandOf("platform")
	if !strings.Contains(argv[0], filepath.Join("v0.2.0", "mosaic-platform")) {
		t.Errorf("running %q", argv[0])
	}
	ran, _ := os.ReadFile(h.ranLog)
	if !strings.Contains(string(ran), "v0.2.0") {
		t.Errorf("the new generation's binary was never started: %s", ran)
	}
}

// **The load-bearing test.** The artefacts' signature stops the bytes being
// swapped; the catalogue's stops the *choice of version* being. Without the
// second, a host can pin an install to an old, genuinely-signed release forever
// and every signature still checks out.
func TestAnUnsignedCatalogueIsRefused(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	u, _, _ := h.updater(t)

	// Re-signed by somebody else — every artefact signature below it is still
	// perfectly genuine.
	_, other, _ := ed25519.GenerateKey(nil)
	h.files["index.json.sig"] = ed25519.Sign(other, h.files["index.json"])

	if _, err := u.Check(context.Background()); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("Check: %v, want ErrUnsigned", err)
	}
	if _, err := u.Upgrade(context.Background()); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("Upgrade: %v, want ErrUnsigned", err)
	}
}

// A tampered catalogue — an entry pointing somewhere else — breaks the
// signature, which is the case the catalogue signature exists for.
func TestATamperedCatalogueIsRefused(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	u, _, _ := h.updater(t)

	h.files["index.json"] = []byte(strings.Replace(string(h.files["index.json"]),
		"v0.1.0", "v9.9.9", 1))

	if _, err := u.Check(context.Background()); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("Check: %v, want ErrUnsigned", err)
	}
}

// An install already on the newest release says so rather than reinstalling it.
func TestUpgradeWhenCurrentSaysSo(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	u, _, _ := h.updater(t)

	if _, err := u.Upgrade(context.Background()); err != nil {
		t.Fatalf("first Upgrade: %v", err)
	}
	version, err := u.Upgrade(context.Background())
	if !errors.Is(err, ErrUpToDate) {
		t.Fatalf("second Upgrade: %v, want ErrUpToDate", err)
	}
	if version != "v0.1.0" {
		t.Errorf("reported %q as active", version)
	}
}

// **Upgrade never moves backwards.** A signed catalogue that offers only old
// versions is still a signed catalogue, so refusing the downgrade is the half
// of the rollback-attack defence the signature cannot provide.
func TestUpgradeRefusesToMoveBackwards(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0", "v0.2.0")
	u, g, _ := h.updater(t)

	if _, err := u.Upgrade(context.Background()); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if v, _ := g.Active(); v != "v0.2.0" {
		t.Fatalf("active = %q", v)
	}

	// The host now offers only the older release — a rollback attack, or a
	// release pulled.
	h.catalogue(t, "v0.1.0")
	found, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if found.Upgrade {
		t.Error("an older release was reported as an upgrade")
	}
	if _, err := u.Upgrade(context.Background()); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("Upgrade: %v, want ErrUpToDate", err)
	}
	if v, _ := g.Active(); v != "v0.2.0" {
		t.Errorf("active = %q — an install moved backwards on its own", v)
	}
}

// Going back deliberately is allowed, and is somebody choosing rather than an
// install deciding.
func TestUpgradeToTakesANamedOlderVersion(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0", "v0.2.0")
	u, g, _ := h.updater(t)

	if _, err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := u.UpgradeTo(context.Background(), "v0.1.0"); err != nil {
		t.Fatalf("UpgradeTo: %v", err)
	}
	if v, _ := g.Active(); v != "v0.1.0" {
		t.Errorf("active = %q", v)
	}
}

// A version the signed catalogue does not offer is refused — the URL comes from
// the entry, so no caller can point an install at bytes nobody signed for.
func TestUpgradeToAnUncataloguedVersionIsRefused(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	u, _, _ := h.updater(t)

	if err := u.UpgradeTo(context.Background(), "v9.9.9"); !errors.Is(err, ErrUnknownRelease) {
		t.Fatalf("UpgradeTo: %v, want ErrUnknownRelease", err)
	}
}

// An upgrade whose Generation does not serve reverts, and the install stays on
// what it was running — the composition inheriting activation's failure branch
// rather than restating it.
func TestAnUpgradeThatDoesNotServeLeavesTheInstallAlone(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0", "v0.2.0")
	u, g, m := h.updater(t)

	if _, err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.catalogue(t, "v0.1.0", "v0.2.0", "v0.3.0")
	h.publish(t, "v0.3.0")
	h.catalogue(t, "v0.1.0", "v0.2.0", "v0.3.0")
	h.serving.Store(false)

	if _, err := u.Upgrade(context.Background()); !errors.Is(err, ErrActivationFailed) {
		t.Fatalf("Upgrade: %v, want ErrActivationFailed", err)
	}
	if v, _ := g.Active(); v != "v0.2.0" {
		t.Errorf("active = %q — a generation that never served became live", v)
	}
	argv, _ := m.CommandOf("platform")
	if !strings.Contains(argv[0], "v0.2.0") {
		t.Errorf("running %q after the revert", argv[0])
	}
	// The failed Generation is on disk and verified; what stops it being live
	// is the pointer, not its absence.
	if !g.Complete("v0.3.0") {
		t.Error("the fetched generation was discarded, so a retry would re-download it")
	}
}

// A retry does not re-download a Generation already on disk and verified.
func TestARetryReusesAVerifiedGeneration(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	u, g, _ := h.updater(t)
	h.serving.Store(false)

	if _, err := u.Upgrade(context.Background()); !errors.Is(err, ErrActivationFailed) {
		t.Fatalf("Upgrade: %v", err)
	}
	if !g.Complete("v0.1.0") {
		t.Fatal("the generation was not kept")
	}

	// The host stops serving the artefact entirely; a retry that re-downloaded
	// would now fail with a 404 instead of activating.
	delete(h.files, "v0.1.0/"+ReleaseArtefactName("mosaic-platform"))
	h.serving.Store(true)

	if err := u.UpgradeTo(context.Background(), "v0.1.0"); err != nil {
		t.Fatalf("retry: %v — a verified generation on disk was re-downloaded", err)
	}
	if v, _ := g.Active(); v != "v0.1.0" {
		t.Errorf("active = %q", v)
	}
}

// A build with no key cannot read a catalogue, and says that rather than that
// the catalogue is bad.
func TestACatalogueNeedsAKey(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	u, _, _ := h.updater(t)
	u.Keys = &Keyring{}

	if _, err := u.Check(context.Background()); !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("Check: %v, want ErrNoTrustedKey", err)
	}
}

// A catalogue in a format this build does not read is refused rather than
// misparsed into an empty one, which would report "no releases" for a host that
// is offering plenty.
func TestACatalogueOfAnotherSchemaIsRefused(t *testing.T) {
	h := newReleaseHost(t, "v0.1.0")
	u, _, _ := h.updater(t)

	body, _ := json.Marshal(map[string]any{"schema": "mosaic.release.index/v2"})
	h.files["index.json"] = body
	h.files["index.json.sig"] = ed25519.Sign(h.priv, body)

	_, err := u.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "v2") {
		t.Fatalf("Check: %v, want a refusal naming the schema", err)
	}
}

// The latest is computed from the entries rather than read from a field, so a
// catalogue cannot offer one version and name another. An entry this build
// cannot order is skipped rather than failing the whole catalogue.
func TestLatestIsComputedAndSkipsWhatItCannotOrder(t *testing.T) {
	index := ReleaseIndex{Releases: []ReleaseEntry{
		{Version: "v0.2.0"}, {Version: "v0.10.0"}, {Version: "v0.9.0"},
		{Version: "v1.0.0-rc1"}, {Version: "nightly"},
	}}
	latest, ok := index.Latest()
	if !ok {
		t.Fatal("no latest")
	}
	if latest.Version != "v0.10.0" {
		t.Errorf("latest = %q — 0.10 must sort above 0.9, which string order gets wrong", latest.Version)
	}

	if _, ok := (ReleaseIndex{Releases: []ReleaseEntry{{Version: "nightly"}}}).Latest(); ok {
		t.Error("a catalogue this build cannot order reported a latest")
	}
}

// Version ordering, including the case string comparison gets wrong.
func TestVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		current, candidate string
		want               bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.1.0", false},
		{"v0.9.0", "v0.10.0", true}, // string order says otherwise
		{"v0.10.0", "v0.9.0", false},
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v1.0.0", false},
		{"", "v0.1.0", true}, // a fresh install: everything is newer than nothing
	} {
		got, err := newer(tc.current, tc.candidate)
		if err != nil {
			t.Errorf("newer(%q, %q): %v", tc.current, tc.candidate, err)
			continue
		}
		if got != tc.want {
			t.Errorf("newer(%q, %q) = %v", tc.current, tc.candidate, got)
		}
	}

	// Unorderable is an error, not a guess. A guess in the wrong direction is a
	// silent downgrade.
	for _, bad := range []string{"", "nightly", "v1.0", "v1.0.0.0", "v1.0.0-rc1", "v-1.0.0"} {
		if _, err := newer("v1.0.0", bad); err == nil {
			t.Errorf("newer(v1.0.0, %q) was ordered", bad)
		}
	}
}

// The release names its files by target and a Generation holds one host's under
// the name the child spec execs. The windows suffix is the detail that is wrong
// until somebody runs it there, so it is checked from here.
func TestReleaseArtefactNamesTheHostsBuild(t *testing.T) {
	defer func(o, a string) { hostOS, hostArch = o, a }(hostOS, hostArch)

	hostOS, hostArch = "linux", "arm64"
	if got := ReleaseArtefactName("mosaic-platform"); got != "mosaic-platform-linux-arm64" {
		t.Errorf("got %q", got)
	}
	hostOS, hostArch = "windows", "amd64"
	if got := ReleaseArtefactName("mosaic-shell"); got != "mosaic-shell-windows-amd64.exe" {
		t.Errorf("got %q", got)
	}
}
