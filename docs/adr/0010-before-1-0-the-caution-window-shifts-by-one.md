# Before 1.0, the upgrade caution window shifts by one

**Status:** Superseded by [supervisor#11](0011-the-monitored-version-is-the-contract-not-the-artefact.md). The window shift was applied to the artefact's own version and withheld from the contract's; both were the wrong way round. Never built.
**Date:** 2026-08-09

Fills the gap [supervisor#9](0009-major-upgrades-are-never-automatic.md) named and
deliberately left to the implementation. Does **not** change
[supervisor#8](0008-artefacts-are-paired-by-contract-major.md) — see the decision's
second half.

## Context

[supervisor#9](0009-major-upgrades-are-never-automatic.md) makes automation depend on the size of a version change: minor and
patch may be automatic, a major never is. It then names the problem that rule
has today and does not solve it.

**Mosaic is pre-1.0, and under strict semver every `0.x` bump is a breaking
change.** Read literally, that makes every release Mosaic has ever cut a major,
so the automatic setting would be a control that never fires — present, chosen,
and doing nothing. Read as though `0.x` were `1.x`, every release becomes
eligible for unattended activation, which is the opposite mistake on software
that says of itself that it is not stable.

Both readings are available in the same three numbers, which is why this needed
deciding rather than defaulting.

## Decision

**While the leading version component is `0`, the window shifts by one: the
minor is read as the major, and the patch as the minor.**

| Version change | Pre-1.0 | 1.0 and after |
|---|---|---|
| `0.30.1` → `0.30.2` | minor — may be automatic | patch — may be automatic |
| `0.30.2` → `0.31.0` | **major — staged, never automatic** | minor — may be automatic |
| `0.31.0` → `1.0.0` | major — staged | major — staged |

- **It governs caution, and nothing else.** The question it answers is "how
  careful should an install be about taking this", which is a product judgement
  about surprise. It is not a claim about what works with what.
- **The compatibility rule is untouched, and must stay untouched.** [supervisor#8](0008-artefacts-are-paired-by-contract-major.md)
  pairs the Platform and the Shell on the *contract* major, and that major is
  the leading component with no shift — `0` for everything today. Applying this
  window there would make `contracts` `v0.59` and `v0.60` incompatible, which is
  false: the contract bumps minor additively by design, consumers adopt a new
  field when they want it, and a client declares what it can draw on every
  Attach so a server emits only that. Shifting the window there would break the
  arrangement that makes independent release cadences work at all.
- **The shift ends at 1.0**, with no transition rule. The first `1.0.0` is a
  major under both readings, so the boundary needs no special case.

## Alternatives

**Treat every `0.x` bump as a major.** *Rejected.* Strictly correct by semver
and it makes the setting inert for the whole pre-1.0 life of the project, which
is most of the time anyone will actually be running this. A control that has
never once done anything is one nobody trusts when it starts to.

**Treat `0.x` as `1.x` and apply ordinary semver.** *Rejected.* It would let an
install activate a `0.30 → 0.31` change unattended, on software whose version
number is the statement that its interfaces are still moving.

**Defer until 1.0.** *Rejected.* It is the same as the first alternative with
extra steps: the setting ships, does nothing, and the rule that makes it work
arrives with the release that needed it least.

## Consequences

- **Patch releases become the automatic path, and Mosaic barely cuts any.** The
  cadence across these repositories is a minor bump per change; under this rule
  none of those is eligible. Making automation useful before 1.0 therefore means
  actually using the patch component for fixes, which is a change to how
  releases are cut rather than to any code.
- **Most pre-1.0 upgrades will be staged rather than applied**, which is the
  honest outcome: the download and the verification happen unattended, the
  cutover waits for a person, and the register says a version is sitting there.
- **Two numbers now mean different things in the same decision** — a shifted
  window for caution, an unshifted leading component for compatibility. That is
  a genuine hazard, and it is why this record states the second half as loudly
  as the first: unifying them would look like a simplification and would silently
  make every contract release a breaking one.
