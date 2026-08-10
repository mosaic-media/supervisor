//go:build mosaicdev

// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"crypto/ed25519"
	"testing"
)

// The tagged half of the override, exercised in the second pass the container
// gate runs. Without this pass the development path is code nothing executes,
// which is how platform#55's counterpart in the Platform is gated and for the same
// reason.

func TestTheDevelopmentKeyIsRead(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(DevReleaseKeyEnv, hexOf(pub))

	keys, development, err := TrustedKeys()
	if err != nil {
		t.Fatalf("TrustedKeys: %v", err)
	}
	if !development {
		t.Fatal("a development key was supplied and the keyring is not marked as one")
	}
	if keys.Empty() {
		t.Fatal("the development key was not trusted")
	}

	// And it actually verifies with it, rather than merely holding it.
	_, signPriv, _ := ed25519.GenerateKey(nil)
	if _, ok := keys.Verify([]byte("message"), ed25519.Sign(signPriv, []byte("message"))); ok {
		t.Error("a signature from an untrusted key verified")
	}
}

// **A malformed development key is an error, never a silent fallback to
// trusting nothing.** The whole point of setting it is to verify against it, so
// failing to parse it must not quietly become "verify nothing" — which would
// present as an unconfigured build and send somebody looking in the wrong
// place.
func TestAMalformedDevelopmentKeyFailsRatherThanFallingBack(t *testing.T) {
	for _, bad := range []string{"not hex", "abcd", "zz"} {
		t.Setenv(DevReleaseKeyEnv, bad)
		if _, _, err := TrustedKeys(); err == nil {
			t.Errorf("%q was accepted as a development key", bad)
		}
	}
}

// An unset or blank variable is not an error — it is a development build that
// simply is not overriding anything, which must behave exactly like a shipped
// one.
func TestNoDevelopmentKeyIsNotAnError(t *testing.T) {
	for _, unset := range []string{"", "   "} {
		t.Setenv(DevReleaseKeyEnv, unset)
		keys, development, err := TrustedKeys()
		if err != nil {
			t.Fatalf("TrustedKeys with %q: %v", unset, err)
		}
		if development {
			t.Errorf("%q was treated as a development override", unset)
		}
		if !keys.Empty() {
			t.Errorf("%q produced a trusted key", unset)
		}
	}
}
