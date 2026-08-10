// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Verifying a downloaded release artefact before it is executed.
//
// **What is signed is the checksums file, not each binary.** A release publishes
// one `SHA256SUMS` covering every target and one detached signature over it, so
// a Supervisor that downloaded one binary verifies a signature over a small text
// file and then a digest over the bytes it holds. That is the same shape the
// module registry uses — sign the aggregate, let the digests inside authenticate
// the parts (platform#40) — and it means adding a target costs no extra signature.
//
// **It is a separate step from downloading on purpose.** Verification takes
// bytes already on disk, so it can be tested without a network and cannot be
// accidentally skipped by a download path that returned early. The order it
// enforces is the one that matters: nothing executes before this returns nil.

// ErrNoTrustedKey is the refusal when the build has no release key.
//
// It is distinct from a bad signature and the distinction is the whole point.
// "This artefact is not signed by Mosaic" and "this Supervisor cannot tell" are
// different facts about different problems, and collapsing them would have an
// unconfigured build report an attack and an attacked one report a
// misconfiguration.
var ErrNoTrustedKey = errors.New("supervisor: this build trusts no release signing key, " +
	"so it cannot verify a downloaded artefact (platform#76: the release key is decided and not yet generated)")

// ErrUnsigned is the refusal when the signature does not verify against any
// trusted key — the artefact was not signed by Mosaic, or was signed by a key
// this build does not trust, or the checksums were altered after signing.
var ErrUnsigned = errors.New("supervisor: the release checksums are not signed by a trusted key")

// ErrNotInChecksums is the refusal when the artefact is not named in the signed
// checksums.
//
// **This is the one that is easy to get wrong and matters most.** A file that is
// not in the signed set is unsigned, however genuine the signature over the set
// is — so an attacker who can add a file to a release directory must not be able
// to have it executed by virtue of a valid signature over everything else.
var ErrNotInChecksums = errors.New("supervisor: the artefact is not named in the signed checksums, so it is unsigned")

// ErrDigestMismatch is the refusal when the bytes on disk are not the bytes that
// were signed for.
var ErrDigestMismatch = errors.New("supervisor: the artefact's digest does not match the signed checksums")

// Checksums maps an artefact's file name to its hex-encoded SHA-256.
type Checksums map[string]string

// ParseChecksums reads the `sha256sum` output format a release publishes: one
// entry per line, a hex digest, whitespace, then the file name.
//
// It refuses a malformed line rather than skipping it. A skipped line is an
// artefact that silently leaves the signed set, which turns into
// [ErrNotInChecksums] later at a point where the cause is no longer visible —
// and being strict here costs nothing, because the file is produced by
// `sha256sum` and not by a person.
//
// The leading `*` GNU coreutils writes for a binary-mode digest is accepted and
// stripped, because `sha256sum` emits it on some platforms and not others and a
// release must not depend on which runner built it.
func ParseChecksums(b []byte) (Checksums, error) {
	sums := Checksums{}
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		digest, name, found := strings.Cut(line, " ")
		if !found {
			return nil, fmt.Errorf("supervisor: checksums line %d is not `<digest>  <name>`: %q", n+1, line)
		}
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		if name == "" {
			return nil, fmt.Errorf("supervisor: checksums line %d names no file", n+1)
		}
		raw, err := hex.DecodeString(digest)
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("supervisor: checksums line %d has no SHA-256 digest: %q", n+1, digest)
		}
		if existing, clash := sums[name]; clash && existing != strings.ToLower(digest) {
			// Two digests for one name is not a file that can be verified: one
			// of them would be picked, and which one decides whether the
			// artefact is accepted.
			return nil, fmt.Errorf("supervisor: checksums name %q twice, with different digests", name)
		}
		sums[name] = strings.ToLower(digest)
	}
	if len(sums) == 0 {
		return nil, errors.New("supervisor: the checksums file is empty")
	}
	return sums, nil
}

// VerifyArtefact decides whether the file at path may be executed, and reports
// which key vouched for it.
//
// The order is the one that makes the refusals meaningful: is there a key at
// all, is the checksums file signed by it, is this artefact in the signed set,
// and only then are the bytes on disk the bytes that were signed for. Reading
// the file — the expensive step — is last, so an unsigned release costs a
// signature check rather than a hash of a hundred megabytes.
//
// keyID is returned for the log. Which key vouched for a release is the only way
// an overlap window is visible from the outside (platform#76), and a development
// key is meant to be conspicuous in a way a fingerprint in a boot line makes it.
func VerifyArtefact(path, name string, checksums, signature []byte, keys *Keyring) (keyID string, err error) {
	if keys == nil || keys.Empty() {
		return "", ErrNoTrustedKey
	}
	keyID, ok := keys.Verify(checksums, signature)
	if !ok {
		return "", ErrUnsigned
	}

	sums, err := ParseChecksums(checksums)
	if err != nil {
		return "", err
	}
	want, listed := sums[name]
	if !listed {
		return "", fmt.Errorf("%w: %s", ErrNotInChecksums, name)
	}

	got, err := fileDigest(path)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("%w: %s is %s, signed for %s", ErrDigestMismatch, name, got, want)
	}
	return keyID, nil
}

// fileDigest is the SHA-256 of a file on disk, streamed rather than read whole:
// a Platform binary is tens of megabytes and the Supervisor is the process that
// has to stay up.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("supervisor: reading the downloaded artefact: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("supervisor: hashing the downloaded artefact: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
