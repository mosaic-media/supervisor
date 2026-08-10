# Major upgrades are staged, never automatic

**Status:** Proposed and partly superseded: the three levels and the never-automatic invariant stand, and the version they read was corrected by [supervisor#11](../../../../../workspace/supervisor/docs/adr/0011-the-monitored-version-is-the-contract-not-the-artefact.md) — the contract's, not the artefact's. Nothing here is built: there is no schedule, no policy setting and no surface. The fetch/activate split it relies on exists.
**Date:** 2026-08-09

Decides the product question [supervisor#1](../../../../../workspace/supervisor/docs/adr/0001-supervisor-as-host-manager.md)'s
mechanism deliberately left open, and which the update code names as "a decision
this does not make". Depends on
[supervisor#8](../../../../../workspace/supervisor/docs/adr/0008-artefacts-are-paired-by-contract-major.md) for something to
check against.

## Context

The Supervisor can fetch a Generation, verify it, activate it and revert if it
does not come up. Nothing polls and nothing decides: `Check` and `Upgrade` are
called, and today only from Go.

That was correct to leave open, because "should a box upgrade itself" is not a
property of the mechanism. It is a question about what somebody has agreed to
run, and the answer differs by the *size* of the change in a way no amount of
health-checking substitutes for: a patch that fixes a crash and a major that
changes what a screen does are both "it came back up", and only one of them is
a surprise a household should not wake up to.

## Decision

**Automation is a setting the user chooses, and it never covers a major
version.**

Three levels, and each is exactly one of the two things the Supervisor already
does — which is why this is a policy and not new machinery:

| Level | Minor and patch | Major |
|---|---|---|
| **Manual** | nothing is fetched | nothing is fetched |
| **Staged** | fetched, verified, activated only when asked | fetched, verified, never activated |
| **Automatic** | fetched, verified and activated | **fetched and staged, never activated** |

- **A major version is never activated without a person**, at any level. This is
  the invariant the rest is arranged around: the most automation a major gets is
  the download and the verification, so the cutover is a decision somebody takes
  with the bytes already on disk and the wait already spent.
- **Crossing a contract major is always major**, whatever the artefacts' own
  version numbers say ([supervisor#8](../../../../../workspace/supervisor/docs/adr/0008-artefacts-are-paired-by-contract-major.md)).
  It is the upgrade where the Platform and the Shell change what they say to each
  other, so it is the one a person most needs to have chosen — and it is
  computable rather than editorial, which is what makes this rule enforceable
  instead of advisory.
- **Minor and patch may be automatic, and it is opt-in.** The default is not
  automatic: an install that upgrades itself the first night without being asked
  is a surprise, and the revert existing does not make it not one.
- **Staging is the fetch that already exists.** `Fetcher.Fetch` completes a
  Generation on disk and `Activator.Activate` moves the pointer; they are
  separate calls today because the failure model needed them separate, and
  "staged" is simply the first without the second.
- **A staged Generation is a finding**
  ([platform#74](https://github.com/mosaic-media/platform/blob/main/docs/adr/0074-operational-findings-are-durable-state.md)): the register is
  how an install says "there is a version here you have not taken", with the
  suggestion being to apply it. That is what makes the offer reachable without
  inventing a notification channel.

## Alternatives

**No automation at all.** *Rejected.* It is the safest and it leaves household
boxes on old versions indefinitely, which is how a security fix does not arrive.
The whole point of an install that can verify and revert is that a small upgrade
can be routine.

**Automatic for everything, with the revert as the safety net.** *Rejected.* The
revert catches a Generation that will not *start*; it cannot catch one that
starts perfectly and works differently. A major version is precisely the change
where "it came up" is not the question being asked.

**Prompt for everything, including patches.** *Rejected.* A prompt that appears
for every patch is a prompt that gets dismissed, and the one that mattered is
dismissed with it.

**Decide by change size from the release notes.** *Rejected.* It makes the
decision a reading comprehension exercise over text a human wrote, at the moment
nobody is watching. Semver is a claim the publisher makes deliberately, and
holding Mosaic to it is the point of using it.

## Consequences

- **Mosaic's own versioning becomes load-bearing.** A major bump now means
  "somebody has to press this", so the difference between minor and major stops
  being editorial. Pre-1.0 this bites immediately: under strict semver every
  `0.x` bump is a breaking change, so the Supervisor must either treat `0.x`
  minors as majors — never automatic, which makes the setting useless before 1.0
  — or read `0.x.y` as major-minor. That is a decision this record does not make
  and the implementation must. The *contract* major does not have this problem
  and is the sharper of the two signals, but it does not replace this one: a
  Platform can change what it does without changing what it says.
- **A staged Generation occupies disk that nothing reclaims yet.** Generations
  accumulate; a policy for how many are kept is not built and is the obvious
  next thing this needs.
- **The setting is Platform configuration and the actor is the Supervisor**,
  which is the one place they must agree and the Supervisor cannot read the
  Platform's database. How the choice reaches it — the handoff channel, an
  environment variable, a file — is an implementation question this leaves open,
  and it is the same question the upgrade *trigger* has.
- **"Check" becomes a thing that runs on its own**, so the rate limit and the
  freshness gap in [supervisor#8](../../../../../workspace/supervisor/docs/adr/0008-artefacts-are-paired-by-contract-major.md) stop being hypothetical.
