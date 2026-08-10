# Two supervised images, a DIY path, and the Supervisor's own contract dependency

**Status:** Built in part, and partly superseded: the import boundary was widened again by [supervisor#7](../../../../../workspace/supervisor/docs/adr/0007-the-supervisor-answers-the-platforms-client-surface.md); the rest stands. The contract dependency, the emitter it was for and both images are built; onboarding's module step is not.
**Date:** 2026-08-09

Amends [platform#50](https://github.com/mosaic-media/platform/blob/main/docs/adr/0050-deployment-topologies.md)'s topology table (the supervised
container splits in two, and the unsupervised arrangement is named as a path
rather than left implied). Settles a question
[supervisor#2](../../../../../workspace/supervisor/docs/adr/0002-supervisor-guarantees-an-interface.md) opened and did not answer:
how a Supervisor that imports the standard library alone emits a contract that
lives in a generated protobuf module. Depends on
[platform#38](https://github.com/mosaic-media/platform/blob/main/docs/adr/0038-platform-binary-built-by-ci.md) for the artefacts and
[platform#51](https://github.com/mosaic-media/platform/blob/main/docs/adr/0051-extension-installation-is-user-initiated-and-persistent.md) for
who installs a module.

## Context

Three questions about how a person actually gets Mosaic running have been
answered in conversation and never written down, and each one has produced work
built against a guess.

**What is in the image.** [supervisor#1](../../../../../workspace/supervisor/docs/adr/0001-supervisor-as-host-manager.md) opens
with "add Mosaic to a Docker Compose file and start", and
[platform#50](https://github.com/mosaic-media/platform/blob/main/docs/adr/0050-deployment-topologies.md) names a Container topology — but
neither says whether the database is inside it. PostgreSQL is deliberately
outside the Generation boundary
([platform#7](https://github.com/mosaic-media/platform/blob/main/docs/adr/0007-platform-transports-events.md)'s reload classes put the DSN in
Recovery for exactly that reason), so "the container" has been ambiguous between
a self-contained appliance and a process that needs a database somebody else
supplied.

**Whether the Supervisor is required.** [platform#38](https://github.com/mosaic-media/platform/blob/main/docs/adr/0038-platform-binary-built-by-ci.md) makes installation "downloading
a binary", and the release workflow says a user can run the Platform binary
directly "meanwhile" — a word that reads as a stopgap until the Supervisor
exists. Whether the unsupervised arrangement survives the Supervisor's arrival
has never been decided, and the difference matters to anyone who has already
built one: if it is a stopgap, the Platform's image and its standalone
`docker-compose.yml` are debt; if it is a supported path, they are the product.

**How the Supervisor emits SDUI.** [supervisor#2](../../../../../workspace/supervisor/docs/adr/0002-supervisor-guarantees-an-interface.md) decides that the Supervisor emits
Recovery SDUI, that the Shell renders it when present, that native clients render
it with their own renderers, and that onboarding runs on it. It does not say how
a process whose whole guarantee is running when everything else has fallen over
obtains a contract that lives in
[`contracts`](https://github.com/mosaic-media/contracts) as generated protobuf.
`boundary_test.go` currently answers by refusing every non-standard-library
import, which makes [supervisor#2](../../../../../workspace/supervisor/docs/adr/0002-supervisor-guarantees-an-interface.md) unimplementable as written. That is not a defect in
either record; it is a question neither was asked.

## Decision

### The supervised install ships as two images

| Image | Contains | For |
|---|---|---|
| **full** | Supervisor, ffmpeg, PostgreSQL | One `docker run` and a working Mosaic. The default, and what the documentation shows first. |
| **lite** | Supervisor, ffmpeg | A homelab that already runs PostgreSQL and does not want a second one. |

ffmpeg is in **both**, and that is not an optimisation. The Platform shells out
to ffprobe to decide what a release is and to ffmpeg to re-encode what a client
cannot decode ([platform#29](https://github.com/mosaic-media/platform/blob/main/docs/adr/0029-probing-and-the-per-stream-playback-decision.md)); absent, it relays unprobed
and a release with undecodable audio plays silently. A supervised image that
omitted ffmpeg would be a subtly broken Mosaic whose breakage presents as bad
media rather than as an error.

PostgreSQL is what splits them, because it is the one component whose *ownership*
a homelab reasonably already has. Nothing else in the supervised install is
shareable in that way.

### The unsupervised path is supported, not a stopgap

**A homelab owner may be the Supervisor.** Running the Platform image with a
PostgreSQL of their own, fetching the Shell binary themselves, and installing
modules through the Platform's own settings is a supported arrangement, as is
running every binary on bare metal. The Platform image keeps ffmpeg and keeps
*not* carrying a database, which is exactly the split it has today.

The two paths differ in who performs provisioning and in nothing else. Both run
identical bytes ([platform#50](https://github.com/mosaic-media/platform/blob/main/docs/adr/0050-deployment-topologies.md)), both reach the same Platform, and a module installed
either way is installed by the Platform on a user's request ([platform#51](https://github.com/mosaic-media/platform/blob/main/docs/adr/0051-extension-installation-is-user-initiated-and-persistent.md)).

What the unsupervised path forfeits is what the Supervisor provides: the single
front door, TLS termination, restart supervision, Generation activation and
rollback, and an interface that survives the Platform being down. Those are the
Supervisor's value proposition, and stating the path plainly is what makes them
legible as a choice rather than as an accident of packaging.

### Onboarding's module step widens from one role to the catalogue

**This generalises a step that already exists rather than adding one.**
Onboarding's fourth step offers a single stream source, chosen from the
catalogued modules that fill that role, and `ClaimServer` installs it as the new
owner — best-effort, so a failure to install does not fail the claim. The
decision is that the step stops being about *streams* and becomes about
*modules*: a person picks from what the repository catalogues, and the Platform
installs each one they chose.

The Supervisor never fetches a module ([platform#49](https://github.com/mosaic-media/platform/blob/main/docs/adr/0049-the-platform-manages-extension-modules.md)). The step remains a surface over
[platform#51](https://github.com/mosaic-media/platform/blob/main/docs/adr/0051-extension-installation-is-user-initiated-and-persistent.md)'s
install command, and widening it is a change to what onboarding offers, not to
how anything is installed.

It stays in onboarding rather than becoming a handoff to the extensions screen
because a first run is the one moment when "what should this Mosaic be able to
do" is the question a person is already answering. Sending them to a settings
screen afterwards makes module selection an administrative errand rather than
part of deciding what they are setting up — and the extensions screen remains
where the decision is revisited.

Two properties of the existing step must survive the widening, and they are the
reason it is a widening rather than a rewrite. **Installation stays best-effort:**
a repository that is unreachable, or a module that will not install, must not
cost somebody their claim, which is the one action in Mosaic that cannot be
retried by a second person. And **the consent surface stays as it is** — a module
is described by what its signed manifest declares it can do, and installing is an
act taken after being told. Onboarding is a better *place* for that decision, not
a licence to make it lighter; a multi-select that installed four modules on one
tap without saying what each does would be exactly that.

### The Supervisor may import `contracts`, and nothing else

`boundary_test.go` widens from "the standard library" to "the standard library
and `github.com/mosaic-media/contracts`". No other dependency, first-party or
otherwise, and the Platform remains forbidden.

The rule's stated purpose is unchanged by this. It exists so the process that has
to run when the Platform cannot carries no compile-time dependency **on the
Platform** — and a published contract module is not a running service. What the
Supervisor gains is the one thing it cannot hand-roll safely: the same generated
types every other emitter uses, guarded by the same drift check.

The alternative was a second emit-side implementation of the SDUI wire format
inside the Supervisor. It is cheaper and smaller and it is the mistake this
project has already made once: ~30 components lived as hand-written TypeScript in
the web client while the contract held four stale copies of four of them, three
had drifted, and nothing anywhere reported it
([contracts#7](https://github.com/mosaic-media/contracts/blob/main/docs/adr/0007-components-are-authored-only-in-the-contract.md)). A drifting
Supervisor emitter would be worse, because it is read by *every* client and the
screens it draws are the ones a user sees when something is already wrong.

**What the Supervisor emits is deliberately narrow: primitives only, no
definitions.** Definitions are server-delivered data
([contracts#4](https://github.com/mosaic-media/contracts/blob/main/docs/adr/0004-server-delivered-definitions-and-skin.md)) and shipping a
definition library inside the Supervisor would put a second copy of the
composition set in the one binary that must not grow. A Supervisor screen is
composed from the native vocabulary, which is what every client implements by
construction.

## Alternatives considered

**One image, always carrying PostgreSQL.** *Rejected.* It is simpler and it is
the right default, which is why `full` exists — but it forces a second database
on every homelab that already runs one, and a bundled database that a user
ignores is disk, memory and a backup story they did not ask for.

**One image, never carrying PostgreSQL.** *Rejected.* It makes the first-run
experience "install a database first", which is the thing [platform#38](https://github.com/mosaic-media/platform/blob/main/docs/adr/0038-platform-binary-built-by-ci.md) removed a Go
toolchain from the install path to avoid.

**Withdraw the unsupervised path once the Supervisor exists.** *Rejected.* It
would delete a working install topology to make a diagram simpler. The Platform
image and the bare-metal binaries exist, are gated, and are what a developer and
an experienced homelab owner actually want; [platform#50](https://github.com/mosaic-media/platform/blob/main/docs/adr/0050-deployment-topologies.md) already rejected
container-only for the adjacent reason.

**Module selection after onboarding, on the extensions screen.** *Rejected*, and
it was close. It adds no surface and the screen already exists. It also makes the
first thing a new Mosaic does be "nothing yet, go to Settings", and it would mean
*removing* the stream-source step that is there — a step whose whole point is
that a fresh install can play something. The extensions screen remains where the
decision is revisited, so nothing is lost by asking once at the start.

**Hand-roll the SDUI wire format in the Supervisor.** *Rejected*, with the
reasoning above. The published conformance fixture
([contracts#17](https://github.com/mosaic-media/contracts/blob/main/docs/adr/0017-the-conformance-corpus.md)) would have made the
drift *detectable*, which is genuinely more than the web client had, but a guard
against divergence is not the same as an implementation that cannot diverge.

**Let the Supervisor import whatever it needs.** *Rejected.* The boundary is
load-bearing and the widening is deliberately one module wide. Every dependency
is a way for the process that must come up to fail to come up.

## Consequences

- **`boundary_test.go` must widen when the emitter lands, and not before.**
  Loosening a guard ahead of the thing that needs it is loosening it for nothing.
  Until then the code enforces stdlib-only and this record says why that is about
  to change — which is a decision not yet built, not a disagreement.
- **The Supervisor's release now tracks a `contracts` version.** It gains the
  protobuf runtime and a `require` to bump, which is a real cost paid for one
  implementation of the contract.
- **Two images to build, sign and publish**, from the same Supervisor binary —
  packaging artefacts, not two builds, exactly as the Platform's image is.
- **The Supervisor has no CI at all today** — no gate, no release, no image — and
  it is the thing a supervised install installs. That gap is the first work this
  record implies.
- **Nothing publishes the Shell binary.** Both paths need it: the supervised one
  fetches it at startup, and the unsupervised one expects a person to. The web
  repository's release workflow publishes only the npm package.
- **A Supervisor screen is constrained to primitives**, so an onboarding step
  that wants a composition it cannot express is a finding about the native
  vocabulary rather than a reason to ship definitions in the Supervisor.
