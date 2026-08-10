# The Platform and the Shell are resolved independently and paired by contract major

**Status:** Proposed and partly superseded: the pairing shape stands, and the component it reads was corrected by [supervisor#11](../../../../../workspace/supervisor/docs/adr/0011-the-monitored-version-is-the-contract-not-the-artefact.md) — pre-1.0 it is the contract minor, not the major. Nothing here is built; the Supervisor reads a signed index from a configured URL today, and no official one exists.
**Date:** 2026-08-09

Settles where the release catalogue
[supervisor#6](../../../../../workspace/supervisor/docs/adr/0006-two-supervised-images-and-a-diy-path.md)'s images need actually
lives. Applies the compatibility rule
[platform#39](https://github.com/mosaic-media/platform/blob/main/docs/adr/0039-extension-module-boundary.md) already uses for modules to the
other two artefacts. Uses the artefact verification of
[platform#40](https://github.com/mosaic-media/platform/blob/main/docs/adr/0040-module-distribution-and-trust.md) and the keys of
[platform#76](https://github.com/mosaic-media/platform/blob/main/docs/adr/0076-the-signing-key-hierarchy.md).

## Context

The Supervisor fetches a Generation on first boot and can activate one: both are
built, and both are pointed at a URL that has to serve a signed index. **No
official index exists**, so a supervised image can only provision against a
development catalogue. That is the one gap between `docker run` and a working
Mosaic.

A Generation is two binaries from two repositories — the Platform from
`platform`, the Shell from `web` — and both release workflows already publish
their binaries and a `SHA256SUMS` as GitHub release assets.

**The first version of this record assumed the two are coupled** and proposed
publishing a tested *pair*: one signed document naming a Platform version and
the Shell version it shipped with. That was wrong, and the reason it was wrong is
the reason the extension boundary exists.

**The Platform and the Shell are not coupled to each other. They are each
coupled to a contract.** A module declares the SDK major it was built against and
the Platform refuses a mismatch before executing anything — "the one
compatibility number a user reasons about"
([platform#39](https://github.com/mosaic-media/platform/blob/main/docs/adr/0039-extension-module-boundary.md)). The Platform and the Shell share
a contract in exactly the same way, and anything speaking the same major of it
works with anything else speaking that major. Pinning a pair would replace a
stated compatibility rule with an editorial one, and would make a Shell fix wait
for a Platform release that has nothing to do with it.

The risk is not skew. **The risk is a contract major that only one side has
reached** — a Platform that has moved to the next major with no Shell that
speaks it yet.

## Decision

**Each artefact is resolved from its own repository's releases, and the two are
paired on the contract major they declare.**

- **Every release declares the contract major it speaks**, in a small document
  covered by the release's own `SHA256SUMS` — so it is signed by the signature
  that already exists rather than by a second one. This is `sdk_major` for the
  other two artefacts, and it is the same number doing the same job.
- **The Supervisor asks each repository for its own latest release**, through
  GitHub's releases API. There is no aggregated catalogue, no new repository and
  no Pages site: the pairing is computed from what each side declares, not
  published by anybody.
- **A Generation is the newest Platform and the newest Shell that declare the
  same major.** Where the newest of each already agree — the ordinary case,
  every day that nobody has bumped the contract — that is simply both latest
  releases.
- **A Platform whose major no Shell has reached is not assemblable**, and the
  Supervisor holds at the newest pair that is. It is a
  [platform#74](https://github.com/mosaic-media/platform/blob/main/docs/adr/0074-operational-findings-are-durable-state.md) finding rather than
  a silence: an install that stopped taking upgrades must say why it stopped.
- **Crossing a contract major is a major upgrade** under
  [supervisor#9](../../../../../workspace/supervisor/docs/adr/0009-major-upgrades-are-never-automatic.md), whatever the version
  numbers on either side say. It is the one upgrade where both halves change
  what they say to each other, so it is the one that most needs a person.
- **The verification path is unchanged.** Each artefact is checked against its
  own signed `SHA256SUMS` by the same `VerifyArtefact` that runs today, with the
  same four refusals.

## Alternatives

**A signed pairing published on the Platform's release.** *Rejected*, and it was
this record's first answer. It makes the Platform's release workflow the arbiter
of which Shell an install runs, which is authority it has no basis for — and it
couples two release cadences that the contract exists to keep apart. A
Shell-only fix would need a Platform release to carry it.

**Extend the module registry.** *Rejected.* Its pipeline exists and its job is
"what a user may install"; adding "what the install itself runs" puts two
audiences and two trust decisions behind one key.

**A dedicated releases repository.** *Rejected.* A fourteenth repository, a
second signing pipeline and a second Pages site, to publish something that can
be computed from two facts each side already knows.

**Pair the two latest releases with no compatibility check at all.**
*Rejected.* It is what "just query GitHub" means if nothing declares a major,
and it works right up until the moment it silently does not — which is precisely
the contract-major boundary this is arranged to notice.

## Consequences

- **The Shell releases on its own cadence and reaches installs immediately.**
  This is the point, and it is the property the rejected pairing would have
  destroyed.
- **The contract major is currently 0 for everything**, because `contracts` is
  pre-1.0 — so the check is real code with nothing yet to refuse. That is honest
  rather than useless: it is the same position `sdk_major` is in, and the wiring
  has to exist before the first bump rather than be written during it.
- **Something must publish the declared major**, which neither release workflow
  does today. It is a line in each, and the smaller half of this change.
- **Rollback by staleness is not solved**, by this or by any alternative. A host
  in the path can serve an old-but-genuine answer forever and every signature
  still verifies. A signed freshness claim is what closes it, and it is left out
  deliberately: it needs a decision about what an install does when it *is* too
  old, which is a worse thing to get wrong than the attack.
- **The GitHub API is an unauthenticated dependency** rate-limited per IP per
  hour, and this now queries two repositories rather than one. A check measured
  in hours is far inside it; a poll measured in minutes is not.
- **`MOSAIC_SUPERVISOR_RELEASE_URL` stays**, because a development catalogue and
  an air-gapped mirror are the same shape and both are worth keeping reachable.
