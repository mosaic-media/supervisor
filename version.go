// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"fmt"
	"strconv"
	"strings"
)

// Comparing two release versions.
//
// **Hand-written because this module imports the standard library and nothing
// else**, and `golang.org/x/mod/semver` is not in it. That constraint is
// ADR 0121's boundary and it is worth more here than a dependency would be: the
// comparison decides whether an install moves forward, and it is forty lines.
//
// **It refuses what it cannot parse rather than guessing an order.** A version
// this does not understand is one where "is that newer?" has no answer, and the
// safe answer to a question about upgrading is no — a guess in the wrong
// direction is a silent downgrade to an old release, which is exactly what
// signing the index is meant to prevent.

// version is a parsed vMAJOR.MINOR.PATCH.
//
// Pre-release and build metadata are deliberately **not** modelled. Mosaic's
// releases are plain three-part tags, and a partial semver implementation that
// silently mis-ordered `v1.0.0-rc1` against `v1.0.0` would be worse than one
// that refuses it — so anything carrying them is unparseable here and reported
// as such. Channels are the honest way to ship a pre-release (ADR 0063 does not
// decide them yet).
type version struct{ major, minor, patch int }

func parseVersion(s string) (version, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if raw == "" {
		return version{}, fmt.Errorf("supervisor: %q is not a version", s)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return version{}, fmt.Errorf("supervisor: %q is not vMAJOR.MINOR.PATCH", s)
	}
	out := version{}
	for i, target := range []*int{&out.major, &out.minor, &out.patch} {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return version{}, fmt.Errorf("supervisor: %q is not vMAJOR.MINOR.PATCH", s)
		}
		*target = n
	}
	return out, nil
}

// newer reports whether candidate is a later version than current, and errors
// if either cannot be ordered.
//
// An empty current means a fresh install with no Generation, and everything is
// newer than nothing — which is what makes the first boot an ordinary upgrade
// rather than a special case.
func newer(current, candidate string) (bool, error) {
	c, err := parseVersion(candidate)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(current) == "" {
		return true, nil
	}
	a, err := parseVersion(current)
	if err != nil {
		return false, err
	}
	switch {
	case c.major != a.major:
		return c.major > a.major, nil
	case c.minor != a.minor:
		return c.minor > a.minor, nil
	default:
		return c.patch > a.patch, nil
	}
}
