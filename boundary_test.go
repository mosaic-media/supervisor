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

// The Supervisor must import nothing but the standard library.
//
// It must be able to run when the Platform cannot — that is the whole of
// ADR 0005's degradation ladder — so a compile-time dependency on the Platform
// would tie the process that stays up to the one that fell over, and would
// make upgrading either mean upgrading both. This module was extracted from
// the platform repository, where it was parked before this one existed; the
// boundary held during that move (a `git subtree split` plus a push, no
// import to rewrite) and this test is what makes sure it keeps holding now
// that the two are separate repositories with separate release cycles.
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
			t.Errorf("%s: imports %q — the Supervisor must depend on the standard library alone, "+
				"so it can run when the Platform cannot and can be extracted without a rewrite", rel, importPath)
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
