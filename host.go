// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import "runtime"

// Which build of a release this host needs.
//
// Variables rather than `runtime.GOOS`/`GOARCH` read at the call site, so a
// test can ask what a release name looks like on a target it is not running on
// — which is the only way to check the windows `.exe` suffix from Linux, and
// the suffix is exactly the kind of detail that is wrong until somebody runs it
// on Windows.
var (
	hostOS   = runtime.GOOS
	hostArch = runtime.GOARCH
)
