// mosaic-media/supervisor: Mosaic's host-level process manager and single
// front door. Runs the Platform and the Shell as child processes, terminates
// TLS, and answers for itself when either child is down.
//
// Imports the standard library and nothing else, enforced by
// TestSupervisorImportsNothingButTheStandardLibrary: it has to be able to run
// when the Platform cannot, which rules out a compile-time dependency on it.
module github.com/mosaic-media/supervisor

go 1.25.0
