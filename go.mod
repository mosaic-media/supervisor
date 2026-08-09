// mosaic-media/supervisor: Mosaic's host-level process manager and single
// front door. Runs the Platform and the Shell as child processes, terminates
// TLS, and answers for itself when either child is down.
//
// Imports the standard library, the published contract, Connect and the
// OpenTelemetry SDK, and nothing else — enforced by
// TestSupervisorImportsNothingButTheStandardLibrary.
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
//
// ADR 0128 admitted OpenTelemetry, so this process stops hand-writing a record
// format it shares with the Platform by convention alone. It takes the OTel
// *SDK* — a binary has to wire the pipeline — but exports to a **file** and
// never over OTLP, which is ADR 0060's objection honoured rather than overruled:
// that record refused an exporter needing a running collector, and nothing
// admitted here dials, resolves or waits on anything.
module github.com/mosaic-media/supervisor

go 1.25.0

require (
	connectrpc.com/connect v1.20.0
	github.com/mosaic-media/contracts v0.60.0
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/exporters/stdout/stdoutlog v0.21.0
	go.opentelemetry.io/otel/log v0.21.0
	go.opentelemetry.io/otel/sdk v1.45.0
	go.opentelemetry.io/otel/sdk/log v0.21.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
