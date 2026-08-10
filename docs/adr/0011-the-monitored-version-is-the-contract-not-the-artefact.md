# The monitored version is the contract, not the artefact

**Status:** Proposed. Nothing here is built.
**Date:** 2026-08-09

Supersedes [supervisor#10](../../../../../workspace/supervisor/docs/adr/0010-before-1-0-the-caution-window-shifts-by-one.md)
wholly. Partly supersedes
[supervisor#8](../../../../../workspace/supervisor/docs/adr/0008-artefacts-are-paired-by-contract-major.md) and
[supervisor#9](../../../../../workspace/supervisor/docs/adr/0009-major-upgrades-are-never-automatic.md): their shape stands and
the version they read does not.

## Context

Three records in a row got the subject of the versioning rule wrong, and the
mistake survived two corrections because each fixed a smaller thing than the one
underneath it.

[supervisor#9](../../../../../workspace/supervisor/docs/adr/0009-major-upgrades-are-never-automatic.md) made caution depend on the size of a change in **the artefact's own
version** — a major of the Platform or the Shell is never activated unattended.
[supervisor#10](../../../../../workspace/supervisor/docs/adr/0010-before-1-0-the-caution-window-shifts-by-one.md) then asked how to read that version before 1.0 and shifted the window
by one, so a `0.30 → 0.31` bump counted as major. Both are answers to the wrong
question.

**An artefact's own version cannot break anything.** The Platform can release
thirty times, the Shell forty, and every one of those is compatible with
everything else as long as the contract they were built against has not moved.
What can break is a change to that contract, and only that. Gating on the
artefact's version therefore blocks upgrades that are safe by construction and —
worse — says nothing at all about the one that is not.

[supervisor#10](../../../../../workspace/supervisor/docs/adr/0010-before-1-0-the-caution-window-shifts-by-one.md) went further and asserted that a contract minor bump before 1.0 is not
breaking, so the compatibility rule should read the leading component unshifted.
**That is false, and this repository already carried the evidence.** The Shell
was pinned to `@mosaic-media/sdui` `0.9.0` while the Platform had shipped
`ClientProfile` in `0.10.0`; Connect-ES serialises only the fields in the schema
it was built with, so the client sent the field, the wire dropped it, the call
returned `200`, and the server saw `nil`. Nothing errored anywhere. That is a
minor skew inside `0.x` producing exactly the silent break the whole
compatibility rule exists to prevent.

## Decision

**The version an install monitors is the published contract each artefact was
built against, and never the artefact's own.**

- **Mosaic has two published contracts and the rule is the same for both.**
  `sdk` is what a module compiles against; `contracts` is what the Platform, the
  Shell and every other client share. Each artefact declares the version of the
  one it uses, and that declaration is the only version any of this reads.
- **Same contract version: any artefact change is safe.** A module, the Platform
  or the Shell may release without limit, and an install may take those upgrades
  automatically, because nothing about what they say to each other has changed.
- **A contract change is the risky upgrade**, at any artefact version — it is
  the only change that can alter what two processes say to each other, so it is
  the only one a person needs to have chosen. It is staged and never activated
  unattended, exactly as [supervisor#9](../../../../../workspace/supervisor/docs/adr/0009-major-upgrades-are-never-automatic.md) describes, with the subject corrected.
- **Before 1.0, the breaking component of the contract is the minor.** ADR
  0126's window shift was the right idea applied to the wrong number: `sdk`
  `v0.26 → v0.27` and `contracts` `v0.59 → v0.60` are breaking-class changes,
  and `v0.26.1 → v0.26.2` is not. After 1.0 it is the major, as semver says.
- **The same component decides compatibility.** [supervisor#8](../../../../../workspace/supervisor/docs/adr/0008-artefacts-are-paired-by-contract-major.md) pairs the Platform and
  the Shell on what they declare; that pairing reads this component, so pre-1.0
  it is the contract minor. An artefact whose contract component no counterpart
  has reached is not assemblable, and the install holds at the newest pair that
  is.

## Alternatives

**Keep watching the artefact version as well, as a second signal.** *Rejected*,
and it is the shape [supervisor#9](../../../../../workspace/supervisor/docs/adr/0009-major-upgrades-are-never-automatic.md) left behind. A Platform can certainly change what
it *does* without changing what it *says*, which is the argument for it — but
that is a release-notes judgement, not something a version number carries, and
adding it back would make almost every release major again and return the
setting to being inert. The honest answer is that automation covers
compatibility risk and not behavioural surprise, and saying so is better than
implying a number measures something it does not.

**Treat the contract's leading component as breaking, unshifted.** *Rejected* —
it is [supervisor#10](../../../../../workspace/supervisor/docs/adr/0010-before-1-0-the-caution-window-shifts-by-one.md)'s position and the `ClientProfile` incident is the counterexample.

**Negotiate at runtime and skip the version check.** *Rejected.* The Shell
already declares its vocabulary on every Attach and the Platform emits only what
it can draw, which is real and genuinely prevents a class of this. It does not
cover the wire itself: a field the client's generated code does not know is
dropped silently in both directions, and no negotiation the client performs can
report a field it has never heard of.

## Consequences

- **Automation becomes useful before 1.0, which [supervisor#10](../../../../../workspace/supervisor/docs/adr/0010-before-1-0-the-caution-window-shifts-by-one.md) could not manage.**
  Most releases in these repositories do not move the contract, so most upgrades
  become eligible — and the ones that do move it are exactly the ones that stop
  and wait. That is the behaviour the setting was supposed to have.
- **Cutting patch releases stops being necessary.** [supervisor#10](../../../../../workspace/supervisor/docs/adr/0010-before-1-0-the-caution-window-shifts-by-one.md)'s consequence — that
  making automation useful meant changing how releases are cut — is withdrawn
  with it. The existing minor-per-change cadence is fine.
- **Every releasable artefact must declare its contract version**, including the
  ones that do not today: the Platform, the Shell, and each module beyond the
  `sdk_major` it already carries. `sdk_major` is the shape and it needs the
  minor beside it while the contracts are pre-1.0.
- **A contract release becomes a coordination point.** Bumping `contracts` means
  no install takes either side of it until both have caught up and a person has
  chosen — which is the correct cost and is worth knowing before the bump rather
  than after.
- **Two contracts can move independently**, so an install can be holding on one
  and current on the other. Nothing here says they must be reasoned about
  together, and a surface that reports "an upgrade is waiting" will have to say
  which contract is holding it.
