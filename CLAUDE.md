# Claude Instructions — supervisor

`mosaic-supervisor` is Mosaic's host-level process manager and single public
front door: it runs the Platform and the Shell as child processes, terminates
TLS on one port, fetches and activates signed Generations, and answers for itself
when a child is down. `README.md` describes what it does; this is how to work
here.

Fleet-wide conventions — commits, decision records, citation form, the roadmap —
are in [`architecture`](https://github.com/mosaic-media/architecture/blob/main/CLAUDE.md).
This file is what is specific to `supervisor`.

## The import boundary

The standard library, `github.com/mosaic-media/contracts`,
`connectrpc.com/connect` and `go.opentelemetry.io/otel` — those three modules
and their subpackages, and nothing else. `boundary_test.go` walks the whole tree
(`cmd/` included) parsing imports, and fails on anything outside that set.

- **The Platform stays forbidden**, because this process has to run when the
  Platform cannot. `TestThePlatformStaysForbidden` asserts the matcher itself
  against a table, so a helper rewritten to match a `github.com/mosaic-media/`
  prefix — the obvious way to admit a second Mosaic module — fails there rather
  than the day somebody imports one.
- Each widening has been one module wide and earned a record; read
  [`docs/adr/README.md`](docs/adr/README.md) before proposing a fourth.

## What this process may not do

- **The front door is a projection surface**: no session state, no database read,
  and never a rewritten body.
- **`platformabsent.go` answers the Platform's own client surface while the
  Platform is down, and may read no database, settings document or library.** It
  cannot authenticate — the sessions belong to the process that is down — so
  everything it says must already be public status. The moment it can answer
  with something a session would have gated, it is an authentication bypass. A
  client asks the one address either way and the front door picks who answers;
  that is the point, so that no client hand-codes a choice between two sources.
- **The Platform's handoff listener is not routed.** `/mosaic.`, `/artwork` and
  `/playback/` reach the Platform and everything else is the Shell's;
  `/readyz`, `/healthz`, `/metadata`, `/migrations` and `/config` are the private
  channel between the two processes and stay off the public port
  (`TestThePlatformsHandoffListenerIsNotPublished`).
- **Recovery SDUI emits primitives and never definitions.** A definition is data
  the Platform delivers on connect, and there is no Platform in the states these
  screens describe, so a component would draw as a placeholder in the one state
  with nothing else to read. Tested against the contract's own `sdui.Primitives`,
  every prop included, since a props bag accepts anything.
- **The recovery page fetches nothing off its own origin**, and has a size
  ceiling: it is what arrives before anything has been downloaded. htmx and its
  SSE extension are vendored under `recoveryui/vendor/` and embedded.

## Lifecycle rules

- **Registration order is stop order**: the Platform first, the interface last,
  and the front door closes only once both are down, so a shutdown walks the
  degradation ladder instead of falling off it. Adding a third child means
  deciding where in that sequence it belongs. It is **deliberately not
  stop-dependents-first**: that rule drains traffic through the dependent, and
  clients reach the Platform through the front door rather than through the
  Shell, so taking the Shell first drains nothing and throws away the best screen
  still standing.
- **Stopping signals the process group** (`Setpgid`, then `Kill(-pid)`), so a
  child's own children go with it rather than being orphaned holding the port.
  That makes `child.go` POSIX-only and nothing here carries a build constraint
  for another OS, while `release.yml`'s matrix lists a `windows/amd64` target.
- **Provisioning runs after the front door is open**, and its failure is
  reported rather than returned: somebody opening the URL during a first boot
  must see the install happening, and a Supervisor that exits replaces an
  explanation with a closed port.
- **Never run it under `go run`.** The toolchain becomes the process that
  receives `SIGTERM`, so the Supervisor never runs its own shutdown and every
  child is killed by whatever is above it.

## Telemetry

Everything this process says goes through `Telemetry` — OpenTelemetry records to
`<state-dir>/logs/mosaic-supervisor.log`, one JSON object per line, with the
console as a second exporter of the same records. `main` is the one deliberate
exception, writing a last-resort failure to stderr with the standard logger.

- **An unclassified `Field` fails closed.** Use `String`, `Int`, `Bool`,
  `Duration`, `Err`, `Sensitive`, `Secret`; a hand-written `Field{Key:…,
  Value:…}` has no class and is redacted rather than written, because the class
  field is unexported.
- **Nothing in the boot path may dial, resolve or wait.** The file exporter is
  unconditional; an OTLP exporter is added only when an operator sets
  `MOSAIC_SUPERVISOR_OTLP_ENDPOINT`, and an unreachable collector must not cost
  the file.
- **Levels are the only filtering** — no sampling, no per-component rules,
  nothing that could quietly discard the record that mattered. `ParseLevel`
  resolves an unrecognised name to info: a typo must not silence the one process
  still able to say why nothing else started.
- **The findings spool is best-effort and nothing about it is fatal.** A
  Supervisor that would not start because it could not record that a child did
  not start is the machinery defeating what it is for.

## Trust, and the development key

- **`-tags mosaicdev` compiles `trust_dev.go`**, which reads a release key from
  `MOSAIC_DEV_RELEASE_KEY`. A shipped build compiles `trust_off.go` and contains
  no reader at all, so the mechanism is absent rather than switched off — which
  is why the guard is a build tag and why the gate exercises both configurations.
  Nothing is bypassed: a development key verifies a development signature and
  every check the real path runs still runs.
- **Verification fails closed, and its refusals stay distinct.** "Not signed by
  Mosaic" and "this build cannot tell" are different facts about different
  problems; keep a new refusal its own error rather than folding it into one.
- **The keyring holds more than one key on purpose** — that is the rotation
  mechanism, and `Verify` tries every key rather than requiring a signature to
  name one. It is not the key the Platform holds for the module index, and the
  two must not meet: this one vouches for the Platform binary itself.
- **Releases are fetched over HTTPS only**, redirects included.

## Configuration

Environment variables only, declared as constants — the `Config` surface in
`config.go`, the children's commands and directories in
`cmd/mosaic-supervisor/main.go`. Where two processes must agree (socket paths,
the boot id, the spool path) the Supervisor decides and tells the child, rather
than both ends being configured separately and able to disagree. The two
directories are not interchangeable: the runtime directory holds the children's
sockets and must **not** survive a reboot; the state directory holds the
Generations and the findings spool and must, or every restart is a first boot.

## The gate

Nothing is built or tested on the host.

```bash
docker compose -f docker-compose.test.yml run --rm test
```

That runs `adr_index.py --check`, `adr_lint.py`, `gofmt -l` over the tree, then
`go vet`, `go build` and `go test`, then `go vet` and `go test` again with
`-tags mosaicdev`. Append `bash` for a shell in the same environment.

`.github/workflows/verify.yml` runs that compose service verbatim on pushes and
pull requests, and `release.yml` calls it before it builds anything — so what
refuses a push is the command you ran. A `v*` tag then cross-compiles five
targets, rolls the per-file checksums into one `SHA256SUMS`, creates the GitHub
release, and packages the two images from those same binaries. That build
carries no `-tags mosaicdev`, deliberately.

`scripts/adr_index.py` and `scripts/adr_lint.py` are **vendored from
[`architecture`](https://github.com/mosaic-media/architecture)** and say so in
their headers; change them there and re-vendor, never here.

## Licensing

AGPL-3.0-only, with no module-linking exception — this process never links a
Module. Every `.go` file carries the `SPDX-License-Identifier` and
`SPDX-FileCopyrightText` pair; nothing in the gate checks for them, so a new
file needs them added by hand.
