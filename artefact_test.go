// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A signed release, as CI would produce one: a binary, a SHA256SUMS covering
// it, and a detached signature over that file.
type signedRelease struct {
	dir       string
	name      string
	path      string
	checksums []byte
	signature []byte
	keys      *Keyring
	priv      ed25519.PrivateKey
}

func newSignedRelease(t *testing.T, contents string) signedRelease {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	keys := &Keyring{}
	if err := keys.Trust("test-release", pub); err != nil {
		t.Fatalf("Trust: %v", err)
	}

	dir := t.TempDir()
	name := "mosaic-platform-linux-amd64"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sum := sha256.Sum256([]byte(contents))
	// Two spaces, as sha256sum writes it, and a second entry so "the one we
	// downloaded" is genuinely being selected from a set rather than being the
	// only line present.
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n" +
		strings.Repeat("0", 64) + "  mosaic-platform-darwin-arm64\n")

	return signedRelease{
		dir: dir, name: name, path: path,
		checksums: checksums,
		signature: ed25519.Sign(priv, checksums),
		keys:      keys, priv: priv,
	}
}

// The whole point, in one test: a genuine artefact verifies and says which key
// vouched for it.
func TestAGenuineArtefactVerifies(t *testing.T) {
	r := newSignedRelease(t, "the platform binary")

	keyID, err := VerifyArtefact(r.path, r.name, r.checksums, r.signature, r.keys)
	if err != nil {
		t.Fatalf("VerifyArtefact: %v", err)
	}
	if keyID != "test-release" {
		t.Errorf("keyID = %q, want the key that signed it", keyID)
	}
}

// **The load-bearing refusal.** A file that is not in the signed set is
// unsigned, however genuine the signature over the set is. Without this an
// attacker who can add a file to a release directory has it executed on the
// strength of a valid signature over everything else.
func TestAnArtefactNotInTheChecksumsIsRefused(t *testing.T) {
	r := newSignedRelease(t, "the platform binary")

	smuggled := filepath.Join(r.dir, "mosaic-platform-linux-arm64")
	if err := os.WriteFile(smuggled, []byte("not mine"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := VerifyArtefact(smuggled, "mosaic-platform-linux-arm64", r.checksums, r.signature, r.keys)
	if !errors.Is(err, ErrNotInChecksums) {
		t.Fatalf("err = %v, want ErrNotInChecksums", err)
	}
}

// Altered bytes are refused even though the signature over the checksums is
// perfectly genuine — which is the case the digest exists for.
func TestAlteredBytesAreRefused(t *testing.T) {
	r := newSignedRelease(t, "the platform binary")
	if err := os.WriteFile(r.path, []byte("the platform binary, tampered"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := VerifyArtefact(r.path, r.name, r.checksums, r.signature, r.keys)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
}

// Altered checksums break the signature, which is the case the signature exists
// for: rewriting a digest to match tampered bytes must not be enough.
func TestAlteredChecksumsAreRefused(t *testing.T) {
	r := newSignedRelease(t, "the platform binary")

	tampered := append([]byte{}, r.checksums...)
	tampered[0] ^= 0xff

	_, err := VerifyArtefact(r.path, r.name, tampered, r.signature, r.keys)
	if !errors.Is(err, ErrUnsigned) {
		t.Fatalf("err = %v, want ErrUnsigned", err)
	}
}

// A signature from a key this build does not trust is not a signature.
func TestAnUntrustedSignerIsRefused(t *testing.T) {
	r := newSignedRelease(t, "the platform binary")
	_, other, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = VerifyArtefact(r.path, r.name, r.checksums, ed25519.Sign(other, r.checksums), r.keys)
	if !errors.Is(err, ErrUnsigned) {
		t.Fatalf("err = %v, want ErrUnsigned", err)
	}
}

// **"Cannot verify" and "did not verify" are different facts** and must not
// collapse into one message: an unconfigured build would otherwise report an
// attack, and an attacked one a misconfiguration.
func TestABuildWithNoKeyRefusesDistinctly(t *testing.T) {
	r := newSignedRelease(t, "the platform binary")

	for _, empty := range []*Keyring{nil, {}} {
		_, err := VerifyArtefact(r.path, r.name, r.checksums, r.signature, empty)
		if !errors.Is(err, ErrNoTrustedKey) {
			t.Errorf("err = %v, want ErrNoTrustedKey", err)
		}
		if errors.Is(err, ErrUnsigned) {
			t.Error("an unconfigured build reported the artefact as unsigned")
		}
	}
}

// Rotation by overlap (platform#76): a keyring trusting the old and new keys
// verifies a release signed by either, which is what makes a rotation cost
// nobody an outage.
func TestEitherKeyInAnOverlapVerifies(t *testing.T) {
	oldPub, oldPriv, _ := ed25519.GenerateKey(nil)
	newPub, newPriv, _ := ed25519.GenerateKey(nil)

	keys := &Keyring{}
	if err := keys.Trust("mosaic-release", oldPub); err != nil {
		t.Fatal(err)
	}
	if err := keys.Trust("mosaic-release-2", newPub); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		signer ed25519.PrivateKey
		want   string
	}{{oldPriv, "mosaic-release"}, {newPriv, "mosaic-release-2"}} {
		r := newSignedRelease(t, "the platform binary")
		got, err := VerifyArtefact(r.path, r.name, r.checksums, ed25519.Sign(tc.signer, r.checksums), keys)
		if err != nil {
			t.Fatalf("VerifyArtefact: %v", err)
		}
		if got != tc.want {
			t.Errorf("keyID = %q, want %q — the log is the only place an overlap is visible", got, tc.want)
		}
	}
}

// A malformed line is refused rather than skipped. A skipped line is an
// artefact that quietly left the signed set, which surfaces later as
// ErrNotInChecksums at a point where the cause is invisible.
func TestAMalformedChecksumsLineIsRefusedNotSkipped(t *testing.T) {
	for _, bad := range []string{
		"not-a-digest  mosaic-platform-linux-amd64\n",
		strings.Repeat("a", 64) + "\n",
		strings.Repeat("a", 63) + "  short-digest\n",
		"",
	} {
		if _, err := ParseChecksums([]byte(bad)); err == nil {
			t.Errorf("ParseChecksums(%q) was accepted", bad)
		}
	}
}

// The `*` sha256sum writes in binary mode on some platforms is stripped, so a
// release does not depend on which runner produced its checksums.
func TestTheBinaryModeMarkerIsAccepted(t *testing.T) {
	digest := strings.Repeat("a", 64)
	sums, err := ParseChecksums([]byte(digest + "  *mosaic-shell-windows-amd64.exe\n"))
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if _, ok := sums["mosaic-shell-windows-amd64.exe"]; !ok {
		t.Errorf("the name kept its marker: %v", sums)
	}
}

// One name with two different digests cannot be verified — whichever were
// picked would decide whether the artefact is accepted.
func TestOneNameWithTwoDigestsIsRefused(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, err := ParseChecksums([]byte(a + "  dup\n" + b + "  dup\n")); err == nil {
		t.Error("two digests for one name were accepted")
	}
	// The same digest twice is harmless and must not be refused.
	if _, err := ParseChecksums([]byte(a + "  dup\n" + a + "  dup\n")); err != nil {
		t.Errorf("an identical repeated line was refused: %v", err)
	}
}
