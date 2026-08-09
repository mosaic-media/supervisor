// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake release host, serving what the three release workflows publish: the
// binaries, a SHA256SUMS over them and a detached signature.
type fakeRelease struct {
	server *httptest.Server
	keys   *Keyring
	files  map[string][]byte
}

func newFakeRelease(t *testing.T, files map[string]string) *fakeRelease {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := &Keyring{}
	if err := keys.Trust("test-release", pub); err != nil {
		t.Fatal(err)
	}

	served := map[string][]byte{}
	var sums strings.Builder
	for name, body := range files {
		served[name] = []byte(body)
		sum := sha256.Sum256([]byte(body))
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	served[checksumsName] = []byte(sums.String())
	served[signatureName] = ed25519.Sign(priv, []byte(sums.String()))

	r := &fakeRelease{keys: keys, files: served}
	r.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, ok := r.files[strings.TrimPrefix(req.URL.Path, "/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(r.server.Close)
	return r
}

// fetcher wires the fake host to a fresh generations root, with the fake's TLS
// client so the self-signed test certificate is accepted.
func (r *fakeRelease) fetcher(t *testing.T) (*Fetcher, *Generations) {
	t.Helper()
	g := generations(t)
	return &Fetcher{Generations: g, Keys: r.keys, Client: r.server.Client()}, g
}

func (r *fakeRelease) release() Release {
	return Release{
		Version: "v0.1.0",
		BaseURL: r.server.URL,
		Artefacts: []Artefact{
			{Name: "mosaic-platform-linux-amd64", As: "mosaic-platform", Executable: true},
			{Name: "mosaic-shell-linux-amd64", As: "mosaic-shell", Executable: true},
		},
	}
}

// The whole path in one test: two artefacts downloaded, both verified, the
// Generation completed and the signer named.
func TestFetchVerifiesAndCompletesAGeneration(t *testing.T) {
	r := newFakeRelease(t, map[string]string{
		"mosaic-platform-linux-amd64": "platform bytes",
		"mosaic-shell-linux-amd64":    "shell bytes",
	})
	f, g := r.fetcher(t)

	keyID, err := f.Fetch(context.Background(), r.release())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if keyID != "test-release" {
		t.Errorf("keyID = %q", keyID)
	}
	if !g.Complete("v0.1.0") {
		t.Fatal("the generation was not completed")
	}

	// Stored under the name a child spec execs, not the release's target name.
	body, err := os.ReadFile(filepath.Join(g.Dir("v0.1.0"), "mosaic-platform"))
	if err != nil {
		t.Fatalf("reading the fetched binary: %v", err)
	}
	if string(body) != "platform bytes" {
		t.Errorf("fetched %q", body)
	}
}

// **The load-bearing test.** One tampered artefact must discard the whole
// staging directory, not just itself. Half a Generation is the most dangerous
// state available: every file in it is individually genuine, so nothing looks
// wrong, and the completeness marker is the only thing that would have caught
// it.
func TestOneBadArtefactDiscardsTheWholeGeneration(t *testing.T) {
	r := newFakeRelease(t, map[string]string{
		"mosaic-platform-linux-amd64": "platform bytes",
		"mosaic-shell-linux-amd64":    "shell bytes",
	})
	// The shell is served as something other than what was signed for.
	r.files["mosaic-shell-linux-amd64"] = []byte("swapped after signing")
	f, g := r.fetcher(t)

	if _, err := f.Fetch(context.Background(), r.release()); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Fetch: %v, want ErrDigestMismatch", err)
	}
	if g.Complete("v0.1.0") {
		t.Fatal("a generation with a bad artefact was completed")
	}
	if _, err := os.Stat(g.Dir("v0.1.0")); !os.IsNotExist(err) {
		t.Error("the staging directory survived a failed fetch, leaving a partly-genuine generation on disk")
	}
	// And it is not activatable, which is the property that actually protects
	// the install if the cleanup above ever fails.
	if err := g.Activate("v0.1.0"); err == nil {
		t.Error("a failed fetch left an activatable generation")
	}
}

// A build with no key refuses before downloading anything, rather than spending
// a hundred megabytes discovering it cannot check them.
func TestFetchWithNoKeyRefusesBeforeDownloading(t *testing.T) {
	r := newFakeRelease(t, map[string]string{"mosaic-platform-linux-amd64": "platform bytes"})
	f, g := r.fetcher(t)
	f.Keys = &Keyring{}

	if _, err := f.Fetch(context.Background(), r.release()); !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("Fetch: %v, want ErrNoTrustedKey", err)
	}
	if _, err := os.Stat(g.Dir("v0.1.0")); !os.IsNotExist(err) {
		t.Error("a keyless fetch staged a directory")
	}
}

// A release that publishes no signature is not a release this will install.
func TestAReleaseWithNoSignatureIsRefused(t *testing.T) {
	r := newFakeRelease(t, map[string]string{"mosaic-platform-linux-amd64": "platform bytes"})
	delete(r.files, signatureName)
	f, _ := r.fetcher(t)

	if _, err := f.Fetch(context.Background(), r.release()); err == nil {
		t.Fatal("a release with no signature was installed")
	}
}

// An artefact the release does not have is a catalogue naming a version that
// was never built for this target — and the 404 is reported rather than turning
// into an empty file that then fails a digest check for a confusing reason.
func TestAMissingArtefactIsReportedAsMissing(t *testing.T) {
	r := newFakeRelease(t, map[string]string{"mosaic-platform-linux-amd64": "platform bytes"})
	f, _ := r.fetcher(t)

	rel := r.release()
	rel.Artefacts = []Artefact{{Name: "mosaic-platform-plan9-mips", As: "mosaic-platform"}}
	_, err := f.Fetch(context.Background(), rel)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Fetch: %v, want the status", err)
	}
}

// **HTTPS only, and no downgrade on a redirect.** The signature protects the
// bytes, but plain HTTP lets anything on the path see and change what is
// fetched before it is checked — and a redirect is how that happens without
// anybody choosing it.
func TestPlainHTTPIsRefused(t *testing.T) {
	f := &Fetcher{Generations: generations(t), Keys: &Keyring{}}
	_, err := releaseURL("http://example.invalid/release", checksumsName)
	if !errors.Is(err, ErrInsecureRelease) {
		t.Errorf("releaseURL over http: %v, want ErrInsecureRelease", err)
	}

	// And through the redirect policy, which is the path a real download takes.
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/asset", nil)
	if err := refuseInsecureRedirect(req, nil); !errors.Is(err, ErrInsecureRelease) {
		t.Errorf("a redirect to http was allowed: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://example.invalid/asset", nil)
	if err := refuseInsecureRedirect(req, nil); err != nil {
		t.Errorf("a redirect to https was refused: %v", err)
	}
	_ = f
}

// An artefact name comes from a catalogue — remote input — and is both appended
// to a URL and used as a file name.
func TestAnArtefactNameCannotEscapeTheRelease(t *testing.T) {
	for _, bad := range []string{"../SHA256SUMS", "a/b", `a\b`, ""} {
		if _, err := releaseURL("https://example.invalid/release", bad); err == nil {
			t.Errorf("releaseURL(%q) was accepted", bad)
		}
	}
}

// A release naming nothing would complete a Generation holding nothing, which
// would then be activatable.
func TestAReleaseWithNoArtefactsIsRefused(t *testing.T) {
	r := newFakeRelease(t, map[string]string{"mosaic-platform-linux-amd64": "x"})
	f, g := r.fetcher(t)

	rel := r.release()
	rel.Artefacts = nil
	if _, err := f.Fetch(context.Background(), rel); err == nil {
		t.Fatal("a release with no artefacts was accepted")
	}
	if g.Complete("v0.1.0") {
		t.Error("an empty generation was completed")
	}
}

// The fetched binaries are executable and the metadata is not — the Supervisor
// execs these, so the mode is part of what it is delivering.
func TestFetchedBinariesAreExecutable(t *testing.T) {
	r := newFakeRelease(t, map[string]string{
		"mosaic-platform-linux-amd64": "platform bytes",
		"mosaic-shell-linux-amd64":    "shell bytes",
	})
	f, g := r.fetcher(t)
	if _, err := f.Fetch(context.Background(), r.release()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(g.Dir("v0.1.0"), "mosaic-platform"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode %v — the Supervisor has to exec this", info.Mode().Perm())
	}
}
