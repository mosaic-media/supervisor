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

// The Supervisor must import nothing but the standard library and the three
// modules named below.
//
// It must be able to run when the Platform cannot — that is the whole of
// supervisor#2's degradation ladder — so a compile-time dependency on the Platform
// would tie the process that stays up to the one that fell over, and would make
// upgrading either mean upgrading both.
//
// contracts is the first exception, decided by supervisor#6 and widened here
// rather than ahead of the emitter that needed it. supervisor#2
// has the Supervisor emit Recovery SDUI, which every client — the Shell, a
// native app, the embedded renderer — then draws. The alternative was a second
// emit-side implementation of the wire format inside this module, which is a
// mistake this project has already made once and which nothing reported.
//
// The rule's purpose is unchanged: the process that has to run when the Platform
// cannot carries no compile-time dependency on the Platform, and a published
// contract module is not a running service. Each widening is exactly one module
// wide, and the Platform stays forbidden — which is what the second half of this
// test asserts.
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

// The non-standard dependencies this module may have.
//
// connectrpc.com/connect adds no code to the binary that contracts had not
// already brought in: contracts requires it itself — it generates the Connect
// handlers for both client-facing services — so this widening moves a module
// from transitive to direct rather than admitting anything new to the linked
// graph. The rule counts direct imports because those are what a reader can
// audit, while the property it protects is about what has to work when
// everything else is broken, and that set is unchanged.
//
// It is needed because supervisor#7 has the Supervisor answer the Platform's own
// client surface while the Platform is down, so a client has one SDUI source
// rather than two. Implementing the generated handler interfaces means naming
// connect.Request and connect.ServerStream.
const (
	contractsModule = "github.com/mosaic-media/contracts"
	connectModule   = "connectrpc.com/connect"
	// OpenTelemetry, admitted by sdk#8. The Supervisor takes the SDK here rather
	// than the API a module gets, because it is a binary and something has to
	// wire the pipeline.
	//
	// supervisor#5 rejected an exporter that needs a running collector, and that
	// is honoured by what the pipeline is allowed to depend on rather than by
	// the module list: the file exporter is unconditional, an OTLP exporter is
	// added only when an operator configures an endpoint, and nothing admitted
	// here may dial, resolve or wait on anything before the process is serving.
	otelModule = "go.opentelemetry.io/otel"
)

// allowedNonStandard reports whether an import is one of the permitted three.
//
// Matched on the module paths and their subpackages, and on nothing else — never
// on a prefix like github.com/mosaic-media/, which would silently admit the
// Platform, the SDK and every module repository. The widening is three modules
// wide and this is what keeps it that way.
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
// somebody does. This asserts the rule, so a helper rewritten to match a
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
		// None of the three is a licence for its neighbours. connectrpc.com
		// hosts more than one module, and grpc arrives in this build graph
		// through the contract already — admitting either by prefix would turn
		// a stated three into "whatever resolves".
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
