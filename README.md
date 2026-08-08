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
- **File-only self-observation, merging into the Platform's telemetry when it
  is up** ([ADR 0060](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0060-the-supervisor-observes-independently.md)).
  Only the boot id is built.
- **Signed release binaries and Generation activation** — the artefact half
  of [the roadmap's M4](https://github.com/mosaic-media/architecture/blob/main/docs/roadmap.md).

See the roadmap for the current, maintained account of what is built and what
is left — this file does not restate it.

## Boundary

Imports the standard library and nothing else, checked by
`TestSupervisorImportsNothingButTheStandardLibrary`. Two reasons: it has to be
able to run when the Platform cannot, so a compile-time dependency on it would
tie the process that stays up to the one that fell over; and it makes this
module's own extraction and any future move as mechanical as this one was.

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
