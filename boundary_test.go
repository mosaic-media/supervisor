// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The Supervisor must import nothing but the standard library and the
// published SDUI contract.
//
// It must be able to run when the Platform cannot — that is the whole of
// ADR 0005's degradation ladder — so a compile-time dependency on the Platform
// would tie the process that stays up to the one that fell over, and would
// make upgrading either mean upgrading both. This module was extracted from
// the platform repository, where it was parked before this one existed; the
// boundary held during that move (a `git subtree split` plus a push, no
// import to rewrite) and this test is what makes sure it keeps holding now
// that the two are separate repositories with separate release cycles.
//
// **`contracts` is the one exception, decided by
// [ADR 0122](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0121-two-supervised-images-and-a-diy-path.md)
// and widened here rather than ahead of the emitter that needed it.** ADR 0005
// has the Supervisor emit Recovery SDUI, which every client — the Shell, a
// native app, the embedded renderer — then draws. The alternative was a second
// emit-side implementation of the wire format inside this module, which is the
// mistake this project has already made once: ~30 components lived as
// hand-written TypeScript in the web client while the contract carried four
// stale copies, three had drifted, and nothing reported it.
//
// The rule's stated purpose is unchanged. It exists so the process that has to
// run when the Platform cannot carries no compile-time dependency **on the
// Platform**, and a published contract module is not a running service. The
// widening is exactly one module wide, and the Platform stays forbidden —
// which is what the second half of this test asserts.
func TestSupervisorImportsNothingButTheStandardLibrary(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("ParseFile(%s): %v", path, parseErr)
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == "github.com/mosaic-media/supervisor" ||
				strings.HasPrefix(importPath, "github.com/mosaic-media/supervisor/") {
				continue // this module's own packages
			}
			if isStandardLibrary(importPath) {
				continue
			}
			if allowedNonStandard(importPath) {
				continue
			}
			t.Errorf("%s: imports %q — the Supervisor may depend only on the standard library "+
				"and %s, so it can run when the Platform cannot (ADR 0121)", rel, importPath, contractsModule)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if checked == 0 {
		t.Fatal("no .go files were checked — the module root resolution is broken")
	}
}

// contractsModule is the one non-standard dependency this module may have.
const contractsModule = "github.com/mosaic-media/contracts"

// allowedNonStandard reports whether an import is the permitted exception.
//
// Matched on the module path and its subpackages, and on nothing else — in
// particular **not** on a prefix like `github.com/mosaic-media/`, which would
// silently admit the Platform, the SDK and every module repository. The
// widening is one module wide and the test is what keeps it that way.
func allowedNonStandard(importPath string) bool {
	return importPath == contractsModule || strings.HasPrefix(importPath, contractsModule+"/")
}

// TestThePlatformStaysForbidden is the half of the boundary the widening could
// have quietly undone.
//
// It is a separate test because the one above walks this module's files and
// would pass on a day nobody imported the Platform, which is every day until
// somebody does. This asserts the *rule*, so a helper rewritten to match a
// prefix — the obvious way to allow a second Mosaic module later — fails here
// rather than at the moment it matters.
func TestThePlatformStaysForbidden(t *testing.T) {
	for _, forbidden := range []string{
		"github.com/mosaic-media/platform",
		"github.com/mosaic-media/platform/internal/platform/app",
		"github.com/mosaic-media/sdk",
		"github.com/mosaic-media/sdk/contracts/platform/v1",
		"github.com/mosaic-media/module-tmdb",
		"github.com/mosaic-media/contractsfoo",
	} {
		if allowedNonStandard(forbidden) {
			t.Errorf("%q is allowed — the widening is one module wide (ADR 0121)", forbidden)
		}
	}
	for _, allowed := range []string{
		"github.com/mosaic-media/contracts",
		"github.com/mosaic-media/contracts/ui",
		"github.com/mosaic-media/contracts/sdui",
	} {
		if !allowedNonStandard(allowed) {
			t.Errorf("%q is refused — the SDUI contract is the permitted exception", allowed)
		}
	}
}

// isStandardLibrary reports whether an import path is in the standard library.
// The standard library is the set of paths whose first segment contains no dot
// — a domain name is what makes an import external.
func isStandardLibrary(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's path")
	}
	return filepath.Dir(thisFile)
}
