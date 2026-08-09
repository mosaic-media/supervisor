# Mosaic Supervisor

The always-on manager that keeps Mosaic running: starts the Platform and the
interface, and answers for itself when either goes down
([ADR 0004](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0004-supervisor-as-host-manager.md),
[ADR 0005](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0005-supervisor-guarantees-an-interface.md)).
It is the single public entry point — one TLS port, the Shell at the root,
the Platform's API behind it — and it is deliberately small: what it used to
be responsible for has shrunk a long way since those records, as extension
modules became the Platform's own concern
([ADR 0079](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0079-the-platform-manages-extension-modules.md))
and per-install builds were replaced by a CI-built binary
([ADR 0063](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0063-platform-binary-built-by-ci.md)).
What is left is process lifecycle, the front door, and activating an artefact
somebody else built.

Extracted from the `platform` repository, where it was parked before this one
existed — see `git log` for the two commits that built it, carried over with
full history via `git subtree split`.

## What it does

- **Runs child processes.** Starts the Platform and the Shell, restarts either
  on exponential backoff, and hands both a shared boot id
  ([ADR 0060](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0060-the-supervisor-observes-independently.md))
  so all three processes' records stitch into one timeline.
- **Stops them in order, and keeps answering while it does.** Children stop in
  registration order — the Platform first, the interface last — and the front
  door stays open until both are down. That walks the degradation ladder
  instead of falling off it: the Platform goes and the Shell, still up,
  renders its offline state; the Shell goes and the holding page answers; only
  then does the door close. Each child is fully stopped before the next is
  asked to, with its own grace period. The signal goes to the process *group*,
  so a child's own children (an extension module, an `ffmpeg`) go with it
  rather than being orphaned holding the port the replacement wants. A child
  that ignores `SIGTERM` is killed once its grace elapses.

  Note that this is *not* the conventional stop-dependents-first. That rule
  exists to drain traffic through the dependent, and it does not apply here:
  clients reach the Platform through the front door directly, never through
  the Shell, so stopping the Shell first would drain nothing and would only
  discard the best screen still standing.
- **Records what it does, to a file** ([ADR 0060](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0060-the-supervisor-observes-independently.md)).
  There is a whole class of failure where the process that would normally
  report is the process that is broken — a migration that will not run, a
  database that is not there, a Generation that starts and immediately dies —
  and the Supervisor is the process that survives all of it *and* the one that
  caused the transition. It writes JSON Lines to
  `<state-dir>/logs/mosaic-supervisor.log` in the Platform's own record format,
  under the same boot id, so one reader parses both: child starts with their
  pid, exits with their code and how long they lasted, the run of failures
  behind a crash loop, readiness transitions, and Generation selection,
  activation and revert. Size-capped rotation keeping one previous file is the
  whole retention policy. There is no database, no exporter and no collector,
  because every one of those can be unavailable at the moment it is needed.
- **Attributes their output.** Three processes share one terminal, so each
  child's console lines are prefixed with its name. This is safe precisely
  because it is the console stream: the Platform's structured records go to
  its file sink, and a line is emitted whole under one lock so two children
  cannot interleave within one.
- **Terminates TLS on one port.** A self-signed certificate is generated in
  memory for each boot when no real one is configured, covering `localhost`
  and the host's own addresses — nothing is written to disk, and every boot
  warns that it is doing this.
- **Routes.** `/mosaic.*` (both Connect services), `/artwork` and
  `/playback/` reach the Platform; everything else is the Shell's. The
  Platform's own handoff listener (`/readyz`, `/healthz`, `/metadata`,
  `/migrations`, `/config`) is deliberately *not* routed — it is the private
  channel between these two processes, and publishing it would put
  Generation and migration state on the public port.
- **Answers for itself when a child cannot.** When the Platform is down, the
  front door returns the Platform's own error vocabulary so the Shell's
  already-loaded offline screen can render it — the richest layer still
  available in ADR 0005's degradation ladder. When the Shell itself is down,
  it serves a small dependency-free holding page, which is the bottom rung
  and says plainly that it is one.

## What it does not do yet

- **Recovery SDUI.** ADR 0005's richer degradation rungs — the Supervisor
  emitting Recovery SDUI, the Shell or an embedded renderer drawing it — are
  not built. The holding page above is a stopgap, not that feature.
- **Reading those records without shell access.** The file is written; nothing
  serves it. ADR 0060's two read paths — the Platform merging it into expert
  mode when it is up, and the Supervisor showing it when the Platform is down —
  are not built, so today finding out what the Supervisor saw means logging in
  to the host, which is the thing
  [ADR 0058](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0058-telemetry-storage-retention-and-expert-mode.md)
  set out to avoid.
- **Signed release binaries and Generation activation** — the artefact half
  of [the roadmap's M4](https://github.com/mosaic-media/architecture/blob/main/docs/roadmap.md).

See the roadmap for the current, maintained account of what is built and what
is left — this file does not restate it.

## Boundary

Imports the standard library, the published contract and Connect, and nothing
else, checked by `TestSupervisorImportsNothingButTheStandardLibrary`. It has to
be able to run when the Platform cannot, so a compile-time dependency on *it*
is out — a published contract module is not a running service.

The contract was admitted by [ADR 0121](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0121-the-supervised-container-images.md),
so the Supervisor emits Recovery SDUI with the same generated types every other
emitter uses; Connect by [ADR 0123](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0123-the-supervisor-answers-the-platforms-client-surface.md),
so it can *answer* the Platform's own client surface while the Platform is down
and every client has one SDUI source rather than a hand-coded choice between
two. The second admission added nothing to the build graph — the contract
already required Connect — so what moved was an import from transitive to
direct.

## Running it

```bash
docker compose -f docker-compose.test.yml run --rm test
```

That is the whole gate — gofmt, `go vet`, `go build`, `go test` — in a
container, nothing on the host. Append `bash` for a shell in the same
environment.

To see it own a real Platform and Shell, use `platform`'s
`docker-compose.supervisor.yml` overlay — one process tree, as a deployed
install has it:

```bash
cd ../platform
docker compose -f docker-compose.dev.yml -f docker-compose.supervisor.yml \
  up postgres supervisor
```

then `https://localhost:8443`. The certificate is self-signed and the browser
will warn — that warning is the accurate description of it. Note that the
`platform` service is deliberately not in that command: the Supervisor starts
the Platform itself, which is the only shape where the boot id is honoured
end to end, since a process compose started mints its own and can adopt
nobody's.

**Do not run it under `go run`.** The toolchain becomes the process being
signalled, the Supervisor never sees `SIGTERM`, and every child is killed by
whatever is above it rather than stopped in order. The overlay above execs a
built binary for exactly this reason.

## License

AGPL-3.0-only. Unlike the Platform, this carries no module-linking exception:
the Supervisor never links a Module into its own process, so that permission
does not apply here.
