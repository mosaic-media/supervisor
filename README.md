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
  on exponential backoff, hands both a shared boot id
  ([ADR 0060](https://github.com/mosaic-media/architecture/blob/main/docs/adr/0060-the-supervisor-observes-independently.md))
  so their logs stitch into one timeline, and stops a process group cleanly
  (`SIGTERM`, then `SIGKILL` after a grace period).
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

To see it front a real Platform and Shell, use `platform`'s
`docker-compose.supervisor.yml` overlay:

```bash
cd ../platform
docker compose -f docker-compose.dev.yml -f docker-compose.supervisor.yml \
  up postgres platform supervisor
```

then `https://localhost:8443`. The certificate is self-signed and the browser
will warn — that warning is the accurate description of it.

## License

AGPL-3.0-only. Unlike the Platform, this carries no module-linking exception:
the Supervisor never links a Module into its own process, so that permission
does not apply here.
