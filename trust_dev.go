//go:build mosaicdev

// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// The development release key (platform#55's mechanism, platform#76's key).
//
// **This file exists only in a `-tags mosaicdev` build.** A shipped Supervisor
// does not contain the code below, so the mechanism is *absent* rather than
// switched off — which is the reason the guard is a build tag and not a runtime
// check. A runtime check is a branch an attacker can try to reach and a
// reviewer has to reason about; an absent function is neither.
//
// **Nothing is bypassed.** A development key verifies a development signature,
// and every check the real path runs still runs: the signature over the
// checksums, the artefact's presence in them, and its digest. A loop that
// skipped verification would exercise a path production does not have, which is
// the whole reason this shape was chosen over a "skip signature checks" flag.

// devReleaseKey reads a release key from the environment.
//
// Hex rather than base64, and the *public* half: it is what `modulesign genkey`
// writes beside the private one, so a development loop hands over the same
// artefact the real key would produce rather than a second encoding to get
// wrong. A malformed value is an error and never a silent fallback to trusting
// nothing — the whole point of setting it is to verify against it, so failing
// to parse it must not quietly become "verify against nothing".
func devReleaseKey() (ed25519.PublicKey, bool, error) {
	raw, set := os.LookupEnv(DevReleaseKeyEnv)
	raw = strings.TrimSpace(raw)
	if !set || raw == "" {
		return nil, false, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, false, fmt.Errorf(
			"supervisor: %s is not hex — it is the development release key's public half, "+
				"as hex (`xxd -p -c 64 key.pub`): %w", DevReleaseKeyEnv, err)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, false, fmt.Errorf(
			"supervisor: %s decodes to %d bytes, not an ed25519 public key (%d)",
			DevReleaseKeyEnv, len(key), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(key), true, nil
}
