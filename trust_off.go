//go:build !mosaicdev

// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import "crypto/ed25519"

// The shipped build's half of the development-key override: there isn't one.
//
// **It reads no environment at all**, which is the property worth having and
// the reason this is a build tag rather than a runtime check. A release
// Supervisor does not contain the code that would look for
// MOSAIC_DEV_RELEASE_KEY, so there is nothing to reach by setting it — no
// branch to try, and none for a reviewer to reason about. The constant naming
// the variable stays compiled in `trust.go` so the claim above has something to
// be tested against.
func devReleaseKey() (ed25519.PublicKey, bool, error) { return nil, false, nil }
