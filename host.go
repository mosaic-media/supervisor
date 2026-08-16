// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import "runtime"

// hostOS and hostArch name the build of a release this host needs.
//
// They are variables rather than runtime.GOOS/GOARCH read at the call site so a
// test can ask what a release name looks like on a target it is not running on.
// That is the only way to check the Windows .exe suffix from Linux.
var (
	hostOS   = runtime.GOOS
	hostArch = runtime.GOARCH
)
