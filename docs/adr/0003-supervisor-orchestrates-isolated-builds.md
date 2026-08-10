# Supervisor orchestrates isolated runtime builds

**Status:** Accepted
**Date:** 2026-07-14

## Context

Mosaic composes a tailored Platform from independently versioned Modules. The Supervisor owns the desired runtime composition, update flow, rollback, diagnostics and activation, whereas the Build Pipeline owns build mechanics such as temporary workspace preparation, `go.mod` updates, generated imports, `go mod tidy` and `go build`.

If the Supervisor instead behaved like a package manager or a plugin loader, Mosaic would either mutate the active installation in place or load unverified code into a running Platform. Both options weaken rollback and recovery.

## Decision

The Supervisor orchestrates isolated runtime builds before activation, taking the declarative Build Specification produced by onboarding as the input to that orchestration. Every build starts from a desired runtime composition and proceeds through:

1. Module selection
2. Module manifest resolution
3. dependency graph validation
4. SDK compatibility validation
5. isolated build workspace creation
6. Go module download and temporary `go.mod` update
7. generated `imports.go`
8. `go mod tidy`
9. `go build`
10. pre-activation health checks
11. atomic activation

Two prohibitions keep that sequence safe. The Supervisor must not modify source repositories or the active Generation during build preparation, and it must not analyse Go source code to discover Module identity, permissions, dependencies or contracts, because manifests remain the Supervisor's source of truth. The Build Pipeline must therefore generate only the `imports.go` integration file required for blank imports.

The Supervisor activates only validated candidate runtimes. During first installation, the Shell remains loaded while the Build Pipeline runs, and switches to the Platform only after health checks pass.

## Alternatives considered

**In-place Platform mutation.** *Rejected:* it weakens rollback and risks corrupting the active runtime.

**Runtime plugin loading.** *Rejected:* it bypasses static validation and introduces runtime compatibility failure modes.

**Supervisor owns Go build mechanics.** *Rejected:* build mechanics would make the Supervisor larger and harder to recover.

**Source-code discovery of Module identity and contracts.** *Rejected:* manifests should be the non-executing source of truth.

**Activate before health checks.** *Rejected:* users could be switched onto an invalid runtime.

**Replace or reload the Shell after the initial build.** *Rejected:* the Shell can switch SDUI producer and backend without disrupting the presentation layer, so replacing it is unnecessary churn.

## Consequences

The Supervisor behaves more like a compiler toolchain orchestrator than a traditional package manager, which makes builds deterministic: every candidate runtime is assembled in a clean workspace from declared inputs. The active Generation remains untouched until a candidate has passed validation, so rollback stays simple — activation switches between known Generations rather than undoing mutations. Development and production can share the same static composition model.

## Implementation implications

Supervisor diagnostics should expose progress for each build stage, and the Recovery UI should report:

- manifest resolution failures
- dependency validation failures
- SDK compatibility failures
- Go dependency failures
- generated import failures
- compilation failures
- health check failures
- activation failures

The active runtime should continue running while candidate preparation fails, and previous known good runtimes should be retained until an explicit garbage collection policy removes them.
