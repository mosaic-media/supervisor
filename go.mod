// mosaic-media/supervisor: Mosaic's host-level process manager and single
// front door. Runs the Platform and the Shell as child processes, terminates
// TLS, and answers for itself when either child is down.
//
// Imports the standard library, the published contract and Connect, and
// nothing else — enforced by TestSupervisorImportsNothingButTheStandardLibrary.
// It has to be able to run when the Platform cannot, which rules out a
// compile-time dependency on *it*; a published contract module is not a running
// service.
//
// ADR 0121 admitted the contract, so the Supervisor emits Recovery SDUI with
// the same generated types every other emitter uses rather than hand-rolling
// the wire format. ADR 0123 admitted Connect, so it can *answer* the Platform's
// own client surface while the Platform is down — which is what lets every
// client have one SDUI source instead of a hand-coded choice between two.
//
// The second admission added nothing to the build graph: the contract already
// required Connect to generate those handlers, so the module moved from
// transitive to direct. The boundary counts direct imports because those are
// what a reader can audit; the set of code that must work when everything else
// is broken did not change.
module github.com/mosaic-media/supervisor

go 1.25.0

require (
	connectrpc.com/connect v1.20.0
	github.com/mosaic-media/contracts v0.60.0
)

require google.golang.org/protobuf v1.36.11 // indirect
