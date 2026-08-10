# Claude Instructions — Mosaic Supervisor

This repository is the process that has to work when nothing else does: Mosaic's
host-level process manager and its single front door
([supervisor#1](docs/adr/0001-supervisor-as-host-manager.md),
[supervisor#2](docs/adr/0002-supervisor-guarantees-an-interface.md)). It runs the
Platform and the Shell as children, terminates TLS, routes, and answers for
itself when a child is down.

`README.md` is the working description of what it does — read it first, and do
not restate it here. This file carries the rules that must hold while changing
it.

**Every rule below follows from one property: this process must still be
standing when the Platform is not.** A change that quietly trades that away will
not fail a build and will not go red in a test — it will simply mean that on the
one day the Supervisor exists for, it is down too.

## The boundary is the point, and it is exactly three modules wide

`boundary_test.go` parses every import in this module and fails on anything
outside the standard library, `github.com/mosaic-media/contracts`,
`connectrpc.com/connect` and `go.opentelemetry.io/otel`. Read the test before
changing what it allows: it carries the reasoning for each admission at the
point the rule is enforced, and `go.mod`'s header carries it at the point the
dependencies are declared.

- **A compile-time dependency on the Platform is out**, and
  `TestThePlatformStaysForbidden` asserts the *rule* rather than today's imports
  — so a helper rewritten to match `github.com/mosaic-media/` as a prefix fails
  there rather than at the moment it matters. Depending on the Platform would tie
  the process that stays up to the one that fell over, and would make upgrading
  either mean upgrading both.
- **A published contract module is not a running service.** That is the whole of
  why `contracts` is admissible and the Platform is not — the distinction to
  reason from when a fourth module is proposed.
- **Match on module paths and their subpackages, never on a prefix.**
  `connectrpc.com` hosts more than one module and gRPC arrives through the
  contract already; admitting either by prefix turns a stated three into
  "whatever resolves".
- **Widen only behind an emitter that needs it, and never ahead of one.** Each
  admission so far bought the deletion of a hand-written copy of something the
  fleet already had — the wire format, the client's second SDUI source, a
  third private log format. A widening that buys nothing has not made its case.

## Do not hand-roll what the contract already says

The Supervisor emits SDUI with the same generated types every other emitter uses.
**A second emit-side implementation of the wire format inside this module is the
mistake this project has already made once**, in the web client, where hand-written
components drifted from the contract with nothing reporting it. If something
cannot be expressed, that is a finding for
[`contracts`](https://github.com/mosaic-media/contracts), not a local shape.

**The embedded renderer's assets are vendored and never fetched.** The recovery
UI draws when there is no Shell and possibly no route to the internet, so a CDN
reference would be one more thing that has to work on the worst day this install
has. `recoveryui/vendor/README.md` says how to update them, and a test asserts
the page fetches nothing but its own origin.

## What the front door may and may not do

- **It is a projection surface.** It holds no session state, reads no database
  and never rewrites a body.
- **The Platform's handoff listener is deliberately not routed.** That is the
  private channel between these two processes; publishing it would put Generation
  and migration state on the public port.
- **When the Platform is absent, the Supervisor answers the Platform's own client
  surface** ([supervisor#7](docs/adr/0007-the-supervisor-answers-the-platforms-client-surface.md))
  — one address, and the front door choosing who answers it, so no client
  implements a rule for picking between two sources.
- **It answers with its own public status and with nothing else, ever.** It
  cannot authenticate: the sessions live in a database belonging to the process
  that is down. Everything it can say is already served unauthenticated to
  anyone who can reach the port. **The moment something here can answer with
  anything a session would have gated, this stops being a projection of public
  status and becomes an authentication bypass.** Treat that as the invariant when
  adding a method to `platformabsent.go`.

## Stopping, and why the order is not the conventional one

Children stop in registration order — the Platform first, the interface last —
and the front door stays open until both are down. That walks the degradation
ladder instead of falling off it. **This is deliberately not
stop-dependents-first:** that rule exists to drain traffic through the dependent,
and clients reach the Platform through the front door directly rather than
through the Shell, so stopping the Shell first would drain nothing and would
discard the best screen still standing.

The signal goes to the process *group*, so a child's own children go with it
rather than being orphaned holding the port the replacement wants.

## Trust: an absent key fails closed and says so

`trust.go` is the Supervisor's side of the release trust model. It downloads the
Platform and the Shell, so it is the process that decides whether the bytes it is
about to execute are Mosaic's.

- **This is a different key from the one the Platform holds**, and the two must
  not meet: the Platform's vouches for an extension module that runs in a
  separate process with controlled egress; this one vouches for the Platform
  binary itself, whose compromise is bounded by nothing.
- **The release key does not exist yet.** The variable is declared rather than
  the file faked, because an absent key must fail closed and say so. When the key
  exists this becomes an embed of 32 raw bytes and nothing else changes.
- **The keyring holds more than one key on purpose** — that is the rotation
  mechanism, not a convenience. Verification tries every key rather than
  requiring a signature to name one.
- **The development key lives behind the `mosaicdev` build tag, and nothing is
  bypassed.** A development key verifies a development signature and every check
  the real path runs still runs. A shipped binary does not contain the code that
  reads the environment — the mechanism is *absent* rather than switched off,
  which is why the guard is a build tag and not a runtime check. `trust_off.go`
  is that claim; keep it a claim something compiles.

## Observability: the file is the guarantee

The Supervisor records what a broken process cannot report about itself — child
starts and exits, crash loops, readiness transitions, Generation selection and
revert — as OpenTelemetry records to a file under the state directory, one JSON
object per line, under a resource carrying the boot id its children share.

- **The file sink is unconditional. OTLP is an operator's opt-in *extra*, never a
  replacement** (`Config.OTLPEndpoint`). A collector that is down, unreachable or
  misconfigured must cost records in the collector and none on disk. Nothing here
  may dial, resolve or wait on anything to start.
- **Levels are the only filtering there is** — no sampling, no per-component
  rules, nothing that could quietly discard the one record that mattered. An
  unrecognised level resolves to info rather than being rejected: a typo must not
  silence the one process still able to explain why nothing else started.
- **Findings are spooled to a file and adopted by the Platform later**
  ([supervisor#5](docs/adr/0005-the-supervisor-observes-independently.md),
  [platform#74](https://github.com/mosaic-media/platform/blob/main/docs/adr/0074-operational-findings-are-durable-state.md)).
  The dependency does not invert, and **everything about it is best-effort and
  nothing is fatal**: a Supervisor that failed to start because it could not
  write down that it had failed to start a child would be the machinery defeating
  the thing it is for.

## Configuration

Environment only, read in one place (`config.go`) so the surface is greppable,
and **validated rather than coerced** — an unusable upstream is an error at
startup, not a 502 later that points at the wrong layer. The runtime directory
holds sockets and must not survive a reboot; the state directory holds
Generations and must, or every restart would be a first boot.

## The records this repository owns

`docs/adr/` holds the decisions whose mechanism is here — the process manager,
the front door, the trust keyring, the artefact pairing.
[`docs/adr/README.md`](docs/adr/README.md) is the generated index and is not
edited by hand; read it first, since it is the bounded thing.

**The index generator and the citation lint are not vendored into this
repository, and this repository's gate does not run them.** They live in
`architecture/scripts/`. Do not claim a check that does not exist here — if you
regenerate or lint, say which script you ran and from where.

<!-- shared-rules:begin -->
## Rules every Mosaic repository shares

*Generated. The source is `architecture/shared/repository-rules.md`; edit it there
and run `scripts/shared_rules.py --write` across the fleet. A copy edited in place
fails its repository's gate, which is the point: these rules were eleven
hand-kept copies in four variants, and the abridged ones had quietly dropped the
reasoning while keeping the rules — and in one case dropped a rule outright.*

### What this file may say

**A `CLAUDE.md` states rules, and facts about its own repository. It does not
state facts about another one — it links instead.**

An audit of all twelve of these files against their source found 74 stale claims.
None of roughly 180 rules was wrong; 62 of the 74 were facts about somebody
else's repository. Ownership predicts rot: a fact about this repository stays true
because whoever changes the code changes the sentence in the same session, and a
fact about another one dies the moment they edit it with nothing here going red.

The same applies to facts this repository already publishes in a generated
artefact — counts, versions, what is built. Point at the artefact.

### Decision records live with the code they govern

Each repository owns the records whose *mechanism* it holds — the spec file, the
lint gate, the conformance corpus, the composition root, the release workflow.
A decision can bind five repositories and still have exactly one steward.

- **`docs/adr/`**, numbered from 1 in every repository, with `docs/adr/README.md`
  a **generated** index. Read the index first; it is the bounded thing.
- **A record's heading carries no number.** The number lives in the filename and
  the index only, so a record's anchor survives being renumbered.
- **Cite a record as `repo#N`, and make it a link** — a relative path within a
  repository, an absolute URL across them, and the bare label only where no URL
  is possible, such as a code comment or a Dockerfile. The old `ADR NNNN`
  spelling is refused by a lint: once every repository numbers from 1, that form
  resolves quietly to a *different* record instead of dangling, and no tool in
  the fleet could detect it.
- **Cross-cutting records stay in [`architecture`](https://github.com/mosaic-media/architecture)** —
  the ones with no enforcing mechanism anywhere: licensing, repository naming and
  topology, the module tier model.

### Decision records are append-only

An ADR is an account of what was decided and why, at a time. It is evidence, not
documentation, and its value is that it was not edited afterwards.

- **Never rewrite a record's body** — not to correct it, not to annotate it, not
  to add "as built, this differs". That turns a record into a running commentary
  and destroys the thing it is for.
- **State changes go in the `**Status:**` line and nowhere else** — built, built
  in part (naming the part), or superseded, wholly or partly.
- **A changed decision earns a new record that supersedes it**, with its own
  Context / Decision / Alternatives / Consequences, and both records then point
  at each other through their Status lines. The old body stays exactly as it was.
- **An unbuilt decision is not a superseded one.** "Not done yet" belongs in the
  Status line and the roadmap; only a reversal earns a new record.

### The roadmap is maintained, not consulted

**`docs/roadmap.md` in [`architecture`](https://github.com/mosaic-media/architecture)
is the single record of where the build is, across every repository.** It stays
there because a milestone spans repositories by construction. Read it before
starting, and **update it in the same session as the change that dates it** — not
in a follow-up, which does not happen.

- A slice that lands is marked landed, **with what it left out named in the same
  sentence**. "Built" with no qualifier claims the whole slice shipped.
- Implementation that departed from its record is recorded where it departed.
  The surprises are the most valuable thing in it.
- **Do not restate the roadmap here.** A second copy of "what is built" in a
  `CLAUDE.md` is how the first copy goes stale unnoticed.
- A capability with no client path is not done — it is
  [owed](https://github.com/mosaic-media/architecture/blob/main/docs/unreachable-capability.md).

### Demonstrated, not asserted

**Say what you actually ran.** A skipped test is not a passed test, and "it should
work" is not evidence.

Each repository's container is the authority on its own gate, and the command is
in that repository's section below. It exists because the checks that matter fail
*soft*: a missing PostgreSQL skips storage tests and still prints `ok`, a missing
generator toolchain produces a drift guard that passes by not running. Where the
container cannot be run, running what you can on the host is better than running
nothing — **provided you report which checks ran and which did not.** Claiming a
gate passed when it was not executed is the one thing this rule exists to stop.

### Commit and push

- **Commit and push each repository separately.** They are siblings on disk and
  independent in git.
- **Commit author identity** must be `AdamNi-7080 <anicholls41@gmail.com>`. If git
  has no identity configured, set it repo-locally rather than globally.
- **Push once the change has been demonstrated working in this session.** Commit
  locally and say so otherwise. **Force-push always requires asking.**
<!-- shared-rules:end -->

## The gate

**Do not run `go build`, `go test`, `go vet` or `gofmt` on the host.** This
repository's gate runs in its container:

```bash
docker compose -f docker-compose.test.yml run --rm test
```

That is gofmt, `go vet`, `go build` and `go test`, then the same vet and tests
again with `-tags mosaicdev` for the development-key path. Append `bash` for a
shell in the same environment. `.github/workflows/verify.yml` runs this compose
file rather than a transcription of it, so what refuses a push is the command you
just ran.

**This repository needs the container least and keeps it anyway.** There is no
database, no browser and no generator to supply — the point of importing almost
nothing is that `go build ./...` really is the whole dependency graph. It stays
because a rule with an exception in one repository is a rule nobody applies
reliably in the other twelve, and because "it builds on this machine" is not a
claim worth making about the process that has to run everywhere.

**Both build configurations are gated, and that is not symmetry for its own
sake.** The tagged path is otherwise code nothing executes, and the untagged
claim — *this build reads no environment at all* — is only a claim if nothing
compiles it.

**To watch it own a real Platform and Shell**, use `platform`'s
`docker-compose.supervisor.yml` overlay rather than starting the Platform
yourself; the Supervisor starts its children, which is the only shape where the
boot id is honoured end to end. **Never run it under `go run`:** the toolchain
becomes the process being signalled, the Supervisor never sees `SIGTERM`, and
every child is killed by whatever is above it rather than stopped in order.

## Working in this repository

- **Every Go file carries an SPDX header** (`AGPL-3.0-only`, no module-linking
  exception — this process links no Module, so that permission does not apply).
  Match the files already present; nothing generates them here.
- **Both images are packaging artefacts, not builds.** They copy a binary the
  release workflow already cross-compiled, so a container deployment and a
  bare-metal one run identical bytes. Neither carries the Platform or the Shell,
  which is the shape of a supervised install rather than an omission.
- When a change touches what the Platform or a client sees, verify against a real
  one — a degradation path that has never been walked is a description, not a
  behaviour.
