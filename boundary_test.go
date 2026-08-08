// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors
// Linking exception: see LICENSE-EXCEPTION.

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
// Two reasons, and the second is why this test exists rather than a comment.
//
// It must be able to run when the Platform cannot — that is the whole of
// ADR 0005's degradation ladder — so a compile-time dependency on the Platform
// would tie the process that stays up to the one that fell over, and would
// make upgrading either mean upgrading both.
//
// And this package currently lives *inside* the platform repository because
// the Supervisor has no repository of its own yet. That is a parking spot, not
// a decision. An import of the surrounding module would resolve perfectly well
// today and would silently make the extraction a rewrite instead of a `git mv`.
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
