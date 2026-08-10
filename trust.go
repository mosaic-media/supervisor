// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// The Supervisor's side of platform#40's trust model: it downloads the Platform
// and the Shell, so it is the process that must decide whether the bytes it is
// about to execute are Mosaic's.
//
// **This is a different key from the one the Platform holds**
// ([platform#76](https://github.com/mosaic-media/platform/blob/main/docs/adr/0076-the-signing-key-hierarchy.md)).
// The Platform embeds `mosaic-official` and verifies the extension-module index
// with it — a key CI exercises on every module release, whose compromise serves
// a malicious module into a separate process with controlled egress. This one
// vouches for the Platform binary itself, whose compromise is bounded by
// nothing, so it is signed with `mosaic-release` and the two do not meet.
//
// ed25519 throughout, matching the Platform: small keys, small signatures, in
// the standard library, and no parameter choices to get wrong.

// releasePublicKey is Mosaic's release signing key, and it does not exist yet.
//
// platform#76 decided the key hierarchy; generating the key is custody work that
// has to happen off CI and off any agent's machine, so there is nothing to
// embed. The variable is declared rather than the file faked because **an
// absent key must fail closed and say so** — see [TrustedKeys]. When the key
// exists this becomes a `//go:embed mosaic-release.pub` over 32 raw bytes and
// nothing else here changes.
//
// A development build supplies one from the environment instead; that is the
// whole of `trust_dev.go`, and a shipped binary does not contain the code that
// reads it.
var releasePublicKey []byte

// DevReleaseKeyEnv names the variable a *development build* reads to supply a
// release-signing key (platform#55's pattern, applied to release artefacts rather
// than to the module index).
//
// **This build ignores it unless it was built with the `mosaicdev` tag.** The
// name is declared here — always compiled — so that statement has somewhere to
// live and something to test; `devReleaseKey` is the only reader, and its
// untagged half reads no environment at all.
const DevReleaseKeyEnv = "MOSAIC_DEV_RELEASE_KEY"

// trustedKey is one key the Supervisor will accept a signature from, with the
// id it is known by so a verified signature can say which key made it.
type trustedKey struct {
	id  string
	pub ed25519.PublicKey
}

// Keyring is the set of keys the Supervisor trusts to have signed a release.
//
// It holds more than one on purpose, and that is the rotation mechanism rather
// than a convenience: platform#76 rotates by overlap — trust the new key beside
// the old, release, switch signing, drop the old a release later — which is
// only possible because [Keyring.Verify] tries every key rather than requiring
// a signature to name one. The Platform's keyring has the same property and has
// never used it; this one is written for it from the start.
type Keyring struct {
	keys []trustedKey
}

// Trust adds a key. A malformed key is refused rather than stored and skipped
// later, because a keyring that silently holds an unusable key verifies nothing
// and reports that it verified nothing for a reason nobody can find.
func (k *Keyring) Trust(id string, pub ed25519.PublicKey) error {
	if id == "" {
		return fmt.Errorf("supervisor: a trusted key needs an id")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("supervisor: %q is not an ed25519 public key (got %d bytes, want %d)",
			id, len(pub), ed25519.PublicKeySize)
	}
	for _, existing := range k.keys {
		if existing.id == id {
			return fmt.Errorf("supervisor: %q is already trusted", id)
		}
	}
	k.keys = append(k.keys, trustedKey{id: id, pub: pub})
	return nil
}

// Empty reports whether the keyring trusts nothing. A verification against an
// empty keyring is not a failed signature, it is an unconfigured Supervisor,
// and the two want different words in front of an operator.
func (k *Keyring) Empty() bool { return len(k.keys) == 0 }

// Verify reports whether any trusted key signed message, and which one.
//
// Trying every key rather than requiring the signature to name one keeps the
// signature format minimal — a bare 64-byte ed25519 signature — and the cost is
// nothing at the one or two keys a keyring holds. The returned id is
// provenance: which key vouched for this release is worth a log line, and it is
// the only way an overlap window is observable from the outside.
func (k *Keyring) Verify(message, sig []byte) (keyID string, ok bool) {
	if len(sig) != ed25519.SignatureSize {
		return "", false
	}
	for _, tk := range k.keys {
		if ed25519.Verify(tk.pub, message, sig) {
			return tk.id, true
		}
	}
	return "", false
}

// Fingerprint is a short, stable name for a key — the SHA-256 of its public
// half, truncated. It exists for the boot log: "which key is this Supervisor
// trusting" is the question a development override makes worth answering out
// loud, and printing the key itself answers it in a form nobody can compare at
// a glance. Identical in form to the Platform's Repository.KeyFingerprint, so
// the two processes' logs name the same key the same way.
func Fingerprint(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return ""
	}
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// TrustedKeys returns the keys this build will accept a release signature from,
// and whether the set is a development one.
//
// **A build with no key returns an empty keyring rather than an error**, and
// the refusal happens where a release is verified. That is deliberate: the
// Supervisor's job is to be running when other things are not, so it must boot
// and serve its front door on an install that cannot verify an upgrade. What it
// must not do is *activate* one, and that is [VerifyArtefact]'s to refuse.
func TrustedKeys() (keys *Keyring, development bool, err error) {
	keys = &Keyring{}

	if dev, ok, err := devReleaseKey(); err != nil {
		return nil, false, err
	} else if ok {
		if err := keys.Trust("mosaic-release-dev", dev); err != nil {
			return nil, false, err
		}
		return keys, true, nil
	}

	if len(releasePublicKey) == 0 {
		// No key in this build and no development override. Honest, and the
		// current state of the world: platform#76 decided the hierarchy and the
		// key has not been generated.
		return keys, false, nil
	}
	if len(releasePublicKey) != ed25519.PublicKeySize {
		return nil, false, fmt.Errorf(
			"supervisor: the embedded release key is %d bytes, not an ed25519 public key (%d)",
			len(releasePublicKey), ed25519.PublicKeySize)
	}
	if err := keys.Trust("mosaic-release", ed25519.PublicKey(releasePublicKey)); err != nil {
		return nil, false, err
	}
	return keys, false, nil
}
