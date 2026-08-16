//go:build !mosaicdev

// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import "crypto/ed25519"

// devReleaseKey is the shipped build's half of the development-key override:
// there isn't one.
//
// This build reads no environment at all, which is why the guard is a build tag
// rather than a runtime check. A release Supervisor does not contain the code
// that would look for MOSAIC_DEV_RELEASE_KEY, so there is nothing to reach by
// setting it. DevReleaseKeyEnv stays compiled in trust.go so that claim has
// something to be tested against.
func devReleaseKey() (ed25519.PublicKey, bool, error) { return nil, false, nil }
