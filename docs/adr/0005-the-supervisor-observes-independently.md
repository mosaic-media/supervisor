# The Supervisor observes independently

**Status:** Built in part — the writing, not the reading. The Supervisor keeps
its own file-and-console telemetry under a shared boot id, records the lifecycle
enumerated below, and rotates size-capped; the Platform adopts an inbound
`MOSAIC_BOOT_ID` rather than always minting one. Both read paths are unbuilt:
expert mode does not merge the Supervisor's records, and the Supervisor serves no
status-and-log page. The support bundle carries no log file from either process.
Partly superseded by [sdk#8](https://github.com/mosaic-media/sdk/blob/main/docs/adr/0008-opentelemetry-is-the-telemetry-implementation.md),
which reverses the rejection of the OTel SDK below — the objection was to OTLP
needing a collector rather than to OTel, so the Supervisor takes the SDK with a
file exporter — and closes the duplicated record format this record's
Consequences left open. Everything else here stands.
**Date:** 2026-07-22

## Context

[platform#36](https://github.com/mosaic-media/platform/blob/main/docs/adr/0036-telemetry-storage-retention-and-expert-mode.md) writes telemetry
to two sinks and explains why: PostgreSQL makes it queryable, and a file is what
survives PostgreSQL failing. That covers a Platform that is running badly.

It does not cover a Platform that is not running.

There is a whole class of failure where the process that would normally report is
the process that is broken, and it is the class an operator meets first:

- a migration that fails, so the Platform exits before serving anything
  (`storage migration failed` and nothing more);
- a `MOSAIC_POSTGRES_DSN` that is wrong or absent — today the process prints a
  line and exits cleanly, which looks identical to a successful boot to anything
  watching exit codes;
- a port already bound, a capability registry that fails `Verify()`, an artwork
  key that cannot be generated;
- a **crash loop** — the shape of the loop is the diagnosis, and no single
  process instance can see it;
- readiness that never goes true, so the Supervisor never activates the
  Generation;
- a Generation activated that immediately dies, where the interesting question is
  what changed between the previous Generation and this one.

Every one of those produces at most a line on stderr. The Platform's structured
telemetry, its Postgres store and its expert-mode viewer are all unavailable
precisely when these happen, because they are all inside the thing that failed.

The Supervisor is the process that survives all of it — and more than that, it
is the process that *caused* the transition. It selected the Generation, built
it, started it and watched it. It already holds the context that makes these
failures legible; it simply has nowhere to put it.

It is also, today, **largely unbuilt** ([supervisor#1](0001-supervisor-as-host-manager.md)–[platform#4](https://github.com/mosaic-media/platform/blob/main/docs/adr/0004-static-go-module-composition.md)).
This record decides how it will observe, so that when it is built it is not built
blind. It describes nothing that exists.

## Decision

**The Supervisor keeps its own deliberately smaller telemetry, with no dependency
on the Platform, on PostgreSQL, or on anything being alive but itself.**

- **File and stderr, JSON lines, nothing else.** The same record format and the
  same redaction vocabulary as the Platform
  ([platform#34](https://github.com/mosaic-media/platform/blob/main/docs/adr/0034-redaction-classes-are-the-pii-boundary.md)), so one reader
  parses both — and no database, no OTel SDK, no collector, no HTTP client, no
  exporter. Every dependency is something that can be unavailable at the moment
  it is needed, and the Supervisor's entire value here is being the thing that
  still works.
- **It records the lifecycle the Platform structurally cannot**: Generation
  selection and activation, build outcome, process start, exit and exit code,
  crash loops and backoff state, readiness and liveness probe transitions,
  migration status as observed from outside the process, and handover. These are
  facts *about* a process, and a process cannot reliably report its own death.
- **A boot id stitches the two timelines.** The Supervisor mints an id scoped to
  a Generation activation and passes it to the Platform on start; the Platform
  adopts it as the parent of its own boot span rather than minting one
  ([platform#32](https://github.com/mosaic-media/platform/blob/main/docs/adr/0032-the-correlation-id-is-the-trace-id.md)). One identifier then
  spans "the Supervisor decided to start this Generation" through "the Platform
  became ready" — or through the exact point where it stopped instead.
- **Two read paths, chosen by what is alive.**
  - **Platform up:** the Supervisor exposes its recent records on the handoff
    surface, and expert mode
    ([platform#36](https://github.com/mosaic-media/platform/blob/main/docs/adr/0036-telemetry-storage-retention-and-expert-mode.md)) reads them
    so an administrator sees one merged timeline rather than two half-stories.
  - **Platform down:** the Supervisor serves a minimal static status-and-log page
    itself. No SDUI, no session transport, no database — deliberately *not* the
    Platform's interface, because the Platform's interface is the thing that is
    unavailable. This is the "take over" case, and it is the only user-facing
    surface the Supervisor owns.
- **The dependency must never invert.** The Platform must not require the
  Supervisor to be observable. It runs standalone today
  (`go run ./cmd/mosaic-platform`) and must keep doing so; the Supervisor
  collects and merges when it is present, and is never a prerequisite. A
  telemetry design that made the Platform undebuggable without its host would
  have reproduced this record's own problem one level up.
- **Deliberately less, and the omissions are the decision.** No traces, no
  metrics store, no query surface, no retention beyond size-capped rotation, no
  module surface. Levels, timestamps, structure, redaction, rotation. Anything
  richer belongs in the Platform, and if the Platform is down the richer thing
  was not available anyway — so building it twice buys nothing and doubles what
  can break in the component whose job is not to break.

## Alternatives considered

**The Supervisor writes into the Platform's PostgreSQL telemetry tables.**
*Rejected.* It is the exact failure mode this record exists to cover: the
Platform failing to start is very often the database being unreachable, and a
Supervisor that logs there loses its records in precisely that case. It would
also make the Supervisor depend on a schema owned by the component it supervises.

**The Supervisor ships the OTel SDK and exports over OTLP.** *Rejected.* It
carries real weight into a component that should stay small, and an exporter
needs a running collector — the same aliveness assumption, relocated. The
Supervisor may *not* assume anything else is up.

**The Supervisor forwards its records to the Platform to store.** *Rejected*, for
the same reason, and it inverts the dependency the wrong way round: the component
that survives would rely on the component that does not.

**One shared telemetry file both processes append to.** *Rejected.* Interleaved
writes, rotation races between two independent rotators, and a locking scheme
neither process should need. Two files and one reader is simpler and has no
shared failure mode.

**Only stderr, and let journald or Docker capture it.** *Rejected.* It is a
reasonable answer for an operator with `journalctl` and useless for the
in-product requirement that motivated
[platform#36](https://github.com/mosaic-media/platform/blob/main/docs/adr/0036-telemetry-storage-retention-and-expert-mode.md) — an
administrator should not need shell access to their own host to find out why
Mosaic will not start. Capture as text also discards the structure at the moment
of capture.

## Consequences

- **The hardest failures become the best-documented ones.** Today the failures
  furthest from a user's ability to diagnose are the ones with the least
  recorded; this inverts that, which is a larger practical gain than any
  additional detail on the healthy path.
- **The support bundle must collect both files.** `diagnostics`'s bundle
  currently knows about one process. It gains a second source, and the boot id is
  what makes the merged result readable rather than two interleaved streams.
- **This supersedes [platform#36](https://github.com/mosaic-media/platform/blob/main/docs/adr/0036-telemetry-storage-retention-and-expert-mode.md)'s
  open note** that the Supervisor was unaccounted for and would be the natural
  owner of collection. It is the owner of *merging for display*; it is not the
  owner of storage, and the Platform does not depend on it.
- **Almost none of this is buildable yet**, and the roadmap must keep saying so.
  The Supervisor does not exist. The one piece that can land ahead of it is on
  the Platform side: accept an inbound boot id and adopt it, minting one when
  absent. That is small, it is useful immediately (it names a boot in the logs),
  and it means the Supervisor has something to hand over to when it arrives.
- **Two components now share a record format and a redaction vocabulary that live
  in the Platform's repository.** Either the Supervisor imports a small shared
  package or it duplicates a struct definition. Duplication of a serialisation
  format between two binaries that must agree is a real hazard, and where that
  shared piece lives is a question this record does not answer — it is a
  packaging decision that belongs with the Supervisor's own scaffold.

## Implementation implications

Nothing in the Supervisor can be built until the Supervisor is. On the Platform
side, two small pieces can land within the telemetry thread: accepting and
adopting a boot id at startup, and teaching `diagnostics`'s support bundle to
include a second log source when one is present. The handoff surface gains a
records endpoint when the Supervisor exists to serve one; expert mode's merged
timeline is gated on the same. The shared record format's home is decided with
the Supervisor's scaffold, not here.
