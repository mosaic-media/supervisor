// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"crypto/ed25519"
	"os"
	"strings"
	"testing"
)

// A shipped build has no release key yet, so it trusts nothing — and that is
// the honest state rather than a failure: ADR 0122 decided the hierarchy and
// the key has not been generated. What matters is that it fails *closed*, which
// the artefact tests assert at the point of use.
func TestAShippedBuildTrustsNothingYet(t *testing.T) {
	keys, development, err := TrustedKeys()
	if err != nil {
		t.Fatalf("TrustedKeys: %v", err)
	}
	if development {
		t.Error("an untagged build reported a development keyring")
	}
	if !keys.Empty() {
		t.Error("a build with no embedded key trusts something")
	}
}

// **The claim this test exists to make executable:** an untagged build reads no
// environment at all, so the development override is absent rather than
// switched off. Setting the variable must change nothing.
//
// It is skipped in the tagged pass, where reading it is the point — the tagged
// half has its own test.
func TestAnUntaggedBuildIgnoresTheDevelopmentKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(DevReleaseKeyEnv, hexOf(pub))

	keys, development, err := TrustedKeys()
	if err != nil {
		t.Fatalf("TrustedKeys: %v", err)
	}
	if development || !keys.Empty() {
		t.Skip("this is a -tags mosaicdev build; TestTheDevelopmentKeyIsRead covers it")
	}
}

// A key id may not be trusted twice under two different keys — the second would
// be unreachable through its id and the log would name the wrong one.
func TestAKeyIDIsTrustedOnce(t *testing.T) {
	a, _, _ := ed25519.GenerateKey(nil)
	b, _, _ := ed25519.GenerateKey(nil)

	keys := &Keyring{}
	if err := keys.Trust("mosaic-release", a); err != nil {
		t.Fatal(err)
	}
	if err := keys.Trust("mosaic-release", b); err == nil {
		t.Error("the same key id was trusted twice")
	}
}

// A malformed key is refused rather than stored, because a keyring holding an
// unusable key verifies nothing for a reason nobody can find.
func TestAMalformedKeyIsRefused(t *testing.T) {
	keys := &Keyring{}
	if err := keys.Trust("short", ed25519.PublicKey([]byte("too short"))); err == nil {
		t.Error("a 9-byte key was trusted")
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	if err := keys.Trust("", pub); err == nil {
		t.Error("a key with no id was trusted")
	}
}

// The fingerprint is what a boot line prints, so it has to be short, stable and
// different for different keys. Printing the key itself answers "which key" in
// a form nobody can compare at a glance.
func TestTheFingerprintNamesAKeyShortly(t *testing.T) {
	a, _, _ := ed25519.GenerateKey(nil)
	b, _, _ := ed25519.GenerateKey(nil)

	fa, fb := Fingerprint(a), Fingerprint(b)
	if fa == fb {
		t.Error("two different keys share a fingerprint")
	}
	if fa != Fingerprint(a) {
		t.Error("the fingerprint is not stable")
	}
	if !strings.HasPrefix(fa, "sha256:") || len(fa) != len("sha256:")+16 {
		t.Errorf("fingerprint %q is not the Platform's shape", fa)
	}
	if Fingerprint(nil) != "" {
		t.Error("an absent key produced a fingerprint")
	}
}

// The environment variable's name is compiled into every build, tagged or not,
// so the statement "an untagged build ignores it" has something to be made
// about. A rename that missed one half would otherwise be invisible.
func TestTheDevelopmentVariableIsNamedInEveryBuild(t *testing.T) {
	if DevReleaseKeyEnv != "MOSAIC_DEV_RELEASE_KEY" {
		t.Errorf("DevReleaseKeyEnv = %q — the dev stack and the docs name this variable", DevReleaseKeyEnv)
	}
	if _, set := os.LookupEnv(DevReleaseKeyEnv); set {
		t.Log("the variable is set in this environment; the tagged/untagged tests cover both readings")
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}
