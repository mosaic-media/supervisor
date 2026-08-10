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
// supervisor#2's degradation ladder — so a compile-time dependency on the Platform
// would tie the process that stays up to the one that fell over, and would
// make upgrading either mean upgrading both. This module was extracted from
// the platform repository, where it was parked before this one existed; the
// boundary held during that move (a `git subtree split` plus a push, no
// import to rewrite) and this test is what makes sure it keeps holding now
// that the two are separate repositories with separate release cycles.
//
// **`contracts` is the one exception, decided by
// [platform#76](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0121-two-supervised-images-and-a-diy-path.md)
// and widened here rather than ahead of the emitter that needed it.** supervisor#2
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
			t.Errorf("%s: imports %q — the Supervisor may depend only on the standard library, "+
				"%s, %s and %s, so it can run when the Platform cannot "+
				"(supervisor#6, supervisor#7, sdk#8)",
				rel, importPath, contractsModule, connectModule, otelModule)
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

// The two non-standard dependencies this module may have.
//
// **The second one adds no code to the binary that the first had not already
// brought in.** `contracts` requires `connectrpc.com/connect` itself — it
// generates the Connect handlers for both client-facing services — so this
// widening moves a module from transitive to direct rather than admitting
// anything new to the linked graph. That distinction is why supervisor#7 was
// affordable: the rule counts direct imports because those are what a reader
// can audit, but the property it protects is about what has to work when
// everything else is broken, and that set is unchanged.
//
// It is needed because supervisor#7 has the Supervisor *answer* the Platform's own
// client surface while the Platform is down, so a client has one SDUI source
// rather than two. Implementing the generated handler interfaces means naming
// `connect.Request` and `connect.ServerStream`.
const (
	contractsModule = "github.com/mosaic-media/contracts"
	connectModule   = "connectrpc.com/connect"
	// OpenTelemetry, admitted by sdk#8. The Supervisor takes the *SDK* here
	// rather than the API a module gets, because it is a binary and something
	// has to wire the pipeline — but it exports to a **file** and never over
	// OTLP, which is precisely supervisor#5's objection honoured rather than
	// overruled: that record rejected an exporter needing a running collector,
	// and nothing admitted here dials, resolves or waits on anything.
	//
	// It is what ended the third hand-written copy of Mosaic's telemetry. The
	// duplication it replaced was a record format shared with the Platform by
	// convention, guarded by a test naming its JSON keys — the open question
	// supervisor#5's own Consequences left.
	otelModule = "go.opentelemetry.io/otel"
)

// allowedNonStandard reports whether an import is one of the permitted three.
//
// Matched on the module paths and their subpackages, and on nothing else — in
// particular **not** on a prefix like `github.com/mosaic-media/`, which would
// silently admit the Platform, the SDK and every module repository. The
// widening is three modules wide and the test is what keeps it that way.
func allowedNonStandard(importPath string) bool {
	for _, m := range []string{contractsModule, connectModule, otelModule} {
		if importPath == m || strings.HasPrefix(importPath, m+"/") {
			return true
		}
	}
	return false
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
		// Neither of the two is a licence for its neighbours. `connectrpc.com`
		// hosts more than one module, and grpc arrives in this build graph
		// through the contract already — admitting either by prefix would turn
		// a stated two into "whatever resolves".
		"connectrpc.com/grpcreflect",
		"connectrpc.com/connectfoo",
		"google.golang.org/grpc",
		"google.golang.org/protobuf/encoding/protojson",
	} {
		if allowedNonStandard(forbidden) {
			t.Errorf("%q is allowed — the widening is exactly two modules wide (supervisor#6, supervisor#7)", forbidden)
		}
	}
	for _, allowed := range []string{
		"github.com/mosaic-media/contracts",
		"github.com/mosaic-media/contracts/ui",
		"github.com/mosaic-media/contracts/sdui",
		"github.com/mosaic-media/contracts/gen/mosaic/session/v1/sessionv1connect",
		"connectrpc.com/connect",
	} {
		if !allowedNonStandard(allowed) {
			t.Errorf("%q is refused — the contract and Connect are the two permitted exceptions", allowed)
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
