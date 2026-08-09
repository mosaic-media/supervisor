// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Downloading a release, and refusing to keep one that does not verify.
//
// The order is download → verify → complete, and the *shape* of it is what
// matters: an artefact is written into a staged Generation, verified there, and
// the Generation is marked complete only once every artefact has passed. A
// failure anywhere discards the whole staging directory, so there is no state in
// which some of a Generation is trusted.
//
// **The Supervisor is not the Platform and its egress rules are its own.** It
// fetches from one place — a release host it was told about — over HTTPS only,
// with no redirect to anything else, a size cap and a deadline. It has no
// netguard, no proxy and no deny list, and it does not need them: it makes
// exactly these requests and nothing feeds it a URL from a user.

// ErrInsecureRelease is the refusal when a release URL, or something it
// redirects to, is not HTTPS.
//
// GitHub is an untrusted host (ADR 0080) and the signature is what protects the
// bytes — but plain HTTP would let anything on the path see and change what is
// fetched *before* the signature is checked, and a downgrade on a redirect is
// the classic way that happens without anybody choosing it.
var ErrInsecureRelease = errors.New("supervisor: a release must be fetched over HTTPS")

// ErrReleaseTooLarge is the refusal when a download exceeds its cap. The
// Supervisor is the process that must stay up, and a response with no end is
// otherwise a disk filled by something it was told to trust the size of.
var ErrReleaseTooLarge = errors.New("supervisor: the download exceeded its size limit")

const (
	// maxArtefactBytes caps one binary. The Platform's linux/amd64 binary is
	// tens of megabytes; 512MiB is far above anything real and far below a disk.
	maxArtefactBytes = 512 << 20
	// maxMetadataBytes caps SHA256SUMS and its signature, which are a few
	// hundred bytes and a fixed 64.
	maxMetadataBytes = 1 << 20
	// checksumsName and signatureName are what a release publishes beside its
	// binaries. Spelled once, because the three release workflows produce them
	// and this consumes them.
	checksumsName = "SHA256SUMS"
	signatureName = "SHA256SUMS.sig"
)

// Artefact is one file to take from a release: the name it has there, and the
// name to store it under.
//
// The two differ because a release names its files by target
// (`mosaic-platform-linux-amd64`) and a Generation holds one host's, under the
// name the child spec execs (`mosaic-platform`). Doing the mapping here keeps
// the target selection in one place instead of in every path that builds a
// command line.
type Artefact struct {
	Name string
	As   string
	// Executable marks a file the Supervisor will exec, which is every binary
	// and nothing else. It is explicit rather than assumed so that a future
	// non-executable asset does not arrive chmod 0755 by default.
	Executable bool
}

// Release names a version and where its files are.
type Release struct {
	Version string
	// BaseURL is the directory the artefacts, SHA256SUMS and SHA256SUMS.sig sit
	// under — a GitHub release's download prefix in the shipped case.
	BaseURL   string
	Artefacts []Artefact
}

// Fetcher downloads and verifies a release into a Generation.
type Fetcher struct {
	Generations *Generations
	Keys        *Keyring
	// Client is the HTTP client. Nil takes a default with a deadline and a
	// redirect policy that refuses a downgrade to HTTP.
	Client *http.Client
	// Tel is where progress and outcome go (ADR 0060). The same telemetry the
	// Manager holds, so a fetch and the child start that follows it land in
	// one file under one boot id.
	Tel *Telemetry
	// Activity is where a download reports itself for the screen somebody is
	// watching, if anything is watching. Nil is a fetch nobody is looking at —
	// a test, or a path with no front door — and reporting into nothing is the
	// correct behaviour there rather than a missing wire.
	Activity *Activity
	// Phase is what this fetch *is*, which the Fetcher cannot work out and its
	// caller can: bringing down the first Generation an install has ever had is
	// provisioning, and replacing one is upgrading. They read differently to
	// somebody watching — one is "setting up", the other is "this was working a
	// minute ago" — so the distinction is worth a field. Empty takes
	// PhaseProvisioning, which is the safer default: describing an upgrade as a
	// first install is odd, and describing a first install as an upgrade is
	// alarming.
	Phase Phase
}

// Fetch downloads every artefact of a release, verifies each against the signed
// checksums, and completes the Generation only if all of them pass. It reports
// which key vouched for the release.
//
// **A failure discards the whole staging directory.** Half a Generation is the
// state that would be most dangerous to keep: its files are individually
// genuine, so nothing about them looks wrong, and the only thing that would have
// caught it is the completeness marker this refuses to write.
func (f *Fetcher) Fetch(ctx context.Context, rel Release) (keyID string, err error) {
	if f.Generations == nil {
		return "", errors.New("supervisor: a fetcher needs somewhere to stage a generation")
	}
	if len(rel.Artefacts) == 0 {
		return "", errors.New("supervisor: a release with no artefacts would complete a generation holding nothing")
	}
	// Refused before anything is downloaded, so a build with no key does not
	// spend a hundred megabytes discovering it cannot check them.
	if f.Keys == nil || f.Keys.Empty() {
		return "", ErrNoTrustedKey
	}

	dir, err := f.Generations.Stage(rel.Version)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			// Best-effort: the staging directory is not completable, so leaving
			// it behind is untidy rather than dangerous, and reporting the
			// cleanup's failure over the real error would bury the useful one.
			_ = os.RemoveAll(dir)
		}
	}()

	checksums, err := f.get(ctx, rel.BaseURL, checksumsName, maxMetadataBytes)
	if err != nil {
		return "", err
	}
	signature, err := f.get(ctx, rel.BaseURL, signatureName, maxMetadataBytes)
	if err != nil {
		return "", err
	}

	// Reported for the whole fetch rather than per artefact, and cleared however
	// it ends: a failed download left showing "downloading" is the stuck
	// progress bar every installer has and nobody believes.
	defer f.Activity.Done()

	for i, a := range rel.Artefacts {
		as := a.As
		if as == "" {
			as = a.Name
		}
		dest := filepath.Join(dir, as)
		f.log("fetching %s from %s", a.Name, rel.Version)
		f.report(rel.Version, a.Name, i, len(rel.Artefacts), 0, 0)
		if err := f.download(ctx, rel.BaseURL, a.Name, dest, a.Executable, func(done, size int64) {
			f.report(rel.Version, a.Name, i, len(rel.Artefacts), done, size)
		}); err != nil {
			return "", err
		}
		id, err := VerifyArtefact(dest, a.Name, checksums, signature, f.Keys)
		if err != nil {
			return "", err
		}
		keyID = id
	}

	if err := f.Generations.Commit(rel.Version); err != nil {
		return "", err
	}
	f.Tel.Info(componentGeneration, "fetched and verified",
		String("version", rel.Version), String("signed_by", keyID))
	return keyID, nil
}

// download streams one file to dest.
//
// Streaming rather than reading whole: a Platform binary is tens of megabytes
// and this runs in the process that must not be the one that runs out of
// memory. The cap is enforced on the way through rather than from
// Content-Length, which is a claim by the server.
func (f *Fetcher) download(ctx context.Context, base, name, dest string, executable bool, onProgress func(done, size int64)) error {
	body, size, err := f.open(ctx, base, name)
	if err != nil {
		return err
	}
	defer body.Close()

	mode := os.FileMode(0o600)
	if executable {
		mode = 0o700
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("supervisor: writing %s: %w", dest, err)
	}
	defer out.Close()

	// Counted through a reporting reader rather than after the fact, because
	// the number worth having is the one during the minutes this takes.
	src := io.Reader(io.LimitReader(body, maxArtefactBytes+1))
	if onProgress != nil {
		src = &progressReader{r: src, size: size, report: onProgress}
	}
	written, err := io.Copy(out, src)
	if err != nil {
		return fmt.Errorf("supervisor: downloading %s: %w", name, err)
	}
	if written > maxArtefactBytes {
		return fmt.Errorf("%w: %s", ErrReleaseTooLarge, name)
	}
	return out.Close()
}

// get reads a small file whole — the checksums and the signature, which are
// bounded by their own nature and by the cap passed in.
func (f *Fetcher) get(ctx context.Context, base, name string, limit int64) ([]byte, error) {
	body, _, err := f.open(ctx, base, name)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("supervisor: downloading %s: %w", name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %s", ErrReleaseTooLarge, name)
	}
	return b, nil
}

// open issues the request and returns the body, refusing anything that is not a
// 200 over HTTPS.
// The reported size is the server's Content-Length, or zero when it does not
// give one. **It is a claim, and it is only ever used to draw a bar** — the
// size limit is enforced on the way through, on the bytes themselves, precisely
// because this header cannot be trusted. The worst a lie does here is draw the
// fraction badly for one artefact.
func (f *Fetcher) open(ctx context.Context, base, name string) (io.ReadCloser, int64, error) {
	target, err := releaseURL(base, name)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("supervisor: requesting %s: %w", name, err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("supervisor: fetching %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		// The status is named because the two common ones mean different
		// things: a 404 is a release that does not publish this artefact — a
		// catalogue naming a version that was never built — and a 5xx is the
		// host, which is worth retrying and the other is not.
		return nil, 0, fmt.Errorf("supervisor: fetching %s: %s", name, resp.Status)
	}
	size := resp.ContentLength
	if size < 0 {
		size = 0
	}
	return resp.Body, size, nil
}

// report tells the Activity where this fetch has got to.
func (f *Fetcher) report(version, name string, index, total int, done, size int64) {
	phase := f.Phase
	if phase == "" {
		phase = PhaseProvisioning
	}
	f.Activity.Report(phase, version, fetchDetail(name, index, total),
		fetchProgress(index, total, done, size))
}

// progressReader counts bytes on their way past.
//
// **It reports on a threshold rather than on every read**, because a copy makes
// thousands of them and each report takes a lock the front door also takes to
// render. A quarter of a megabyte is far finer than anything a person can see
// move and coarse enough to cost nothing.
type progressReader struct {
	r      io.Reader
	size   int64
	done   int64
	marked int64
	report func(done, size int64)
}

const progressStep = 256 << 10

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.done-p.marked >= progressStep {
		p.marked = p.done
		p.report(p.done, p.size)
	}
	return n, err
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{
		// Generous, because an arm64 NAS on a domestic connection is the case
		// this has to work on, and bounded, because a stalled download must not
		// hold an activation open forever.
		Timeout:       30 * time.Minute,
		CheckRedirect: refuseInsecureRedirect,
	}
}

func (f *Fetcher) log(format string, args ...any) {
	f.Tel.Printf(format, args...)
}

// refuseInsecureRedirect stops a release download being redirected off HTTPS.
//
// GitHub redirects release assets to a CDN, so redirects are ordinary here and
// cannot simply be refused — what must be refused is the *downgrade*, which is
// how a fetch stops being private without anybody choosing it.
func refuseInsecureRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%w: redirected to %s", ErrInsecureRelease, req.URL.Scheme)
	}
	if len(via) >= 10 {
		return errors.New("supervisor: too many redirects fetching a release")
	}
	return nil
}

// releaseURL joins a base and a file name, refusing anything but HTTPS and
// anything that would climb out of the release.
func releaseURL(base, name string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("supervisor: the release base URL is unusable: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("%w: %s", ErrInsecureRelease, base)
	}
	// The name comes from a release catalogue, which is remote input, and it is
	// appended to a URL and used as a file name. A `..` in it would fetch
	// something outside the release and write it outside the generation.
	if name == "" || name != path.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("supervisor: %q is not a usable artefact name", name)
	}
	u.Path = path.Join(u.Path, name)
	return u.String(), nil
}
