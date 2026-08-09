// mosaic-media/supervisor: Mosaic's host-level process manager and single
// front door. Runs the Platform and the Shell as child processes, terminates
// TLS, and answers for itself when either child is down.
//
// Imports the standard library and the published SDUI contract, and nothing
// else — enforced by TestSupervisorImportsNothingButTheStandardLibrary. It has
// to be able to run when the Platform cannot, which rules out a compile-time
// dependency on *it*; a published contract module is not a running service.
//
// ADR 0121 widened the boundary by exactly this one module, so the Supervisor
// emits Recovery SDUI with the same generated types every other emitter uses
// rather than hand-rolling the wire format. The protobuf runtime below is what
// that costs, and it is the only transitive dependency it brings.
module github.com/mosaic-media/supervisor

go 1.25.0

require github.com/mosaic-media/contracts v0.60.0

require google.golang.org/protobuf v1.36.11 // indirect
