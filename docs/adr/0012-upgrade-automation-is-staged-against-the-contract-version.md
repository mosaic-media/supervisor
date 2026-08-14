# Upgrade automation is staged against the contract version, never the artefact's

**Status:** Proposed. Consolidates supervisor#9, supervisor#10 and supervisor#11, whose bodies this replaces. Nothing here is built: there is no schedule, no policy setting and no surface, and neither the caution window the second of those proposed nor the contract reading that replaced it was ever built. The fetch/activate split this relies on exists. Partly supersedes [supervisor#8](0008-artefacts-are-paired-by-contract-major.md): its pairing shape stands, and the component that pairing reads is corrected here — pre-1.0 the contract minor, not the major.
**Date:** 2026-08-10

Decides the product question [supervisor#1](0001-supervisor-as-host-manager.md)'s
mechanism deliberately left open, and which the update code names as "a decision
this does not make". Depends on
[supervisor#8](0008-artefacts-are-paired-by-contract-major.md) for something to
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

### The size of a change is not read off the artefact, and it took two goes to see it

**This is the most useful thing in the run, and it is kept as a correction
rather than replaced by its answer.** Three records in a row got the subject of
the versioning rule wrong, and the mistake survived two corrections because each
fixed a smaller thing than the one underneath it. The three are the retired
supervisor#9, supervisor#10 and supervisor#11, in that order, and they are
called the first, second and third answer below.

**The first answer made caution depend on the size of a change in the
artefact's own version** — a major of the Platform or the Shell is never
activated unattended. **The second kept that subject and only asked how to read
it before 1.0.** Mosaic is pre-1.0, and under strict semver
every `0.x` bump is a breaking change; read literally that makes every release
Mosaic has ever cut a major, so the automatic setting would be a control that
never fires — present, chosen, and doing nothing. Read as though `0.x` were
`1.x`, every release becomes eligible for unattended activation, which is the
opposite mistake on software that says of itself that it is not stable. Both
readings are available in the same three numbers, which is why it needed
deciding rather than defaulting, and the second answer decided it by shifting
the window one place down the artefact's version. Both are answers to the
wrong question.

**An artefact's own version cannot break anything.** The Platform can release
thirty times, the Shell forty, and every one of those is compatible with
everything else as long as the contract they were built against has not moved.
What can break is a change to that contract, and only that. Gating on the
artefact's version therefore blocks upgrades that are safe by construction and —
worse — says nothing at all about the one that is not.

**The second answer then got the second number wrong in the opposite
direction.** It withheld the shift from the contract, asserting that a contract
minor bump
before 1.0 is not breaking, and that the compatibility rule should read the
contract's leading component unshifted — on the reasoning that the contract
bumps minor additively by design, that consumers adopt a new field when they
want it, and that a client declares what it can draw on every Attach so a server
emits only that. It held that shifting the window onto the contract would break
the arrangement that makes independent release cadences work at all, and it said
so as loudly as it said the rest. **That is false, and this repository already
carried the evidence.** The Shell was pinned to `@mosaic-media/sdui` `0.9.0`
while the Platform had shipped `ClientProfile` in `0.10.0`; Connect-ES serialises only the
fields in the schema it was built with, so the client sent the field, the wire
dropped it, the call returned `200`, and the server saw `nil`. Nothing errored
anywhere. That is a minor skew inside `0.x` producing exactly the silent break
the whole compatibility rule exists to prevent.

So the window shift was the right idea applied to the wrong number, twice over:
applied to the artefact's version, where it does not belong, and withheld from
the contract's, where it does. **The third answer changed the subject rather
than the reading, and its answer is the one this record keeps.**

## Decision

**Automation is a setting the user chooses, it never covers a contract change,
and the version it reads is the published contract each artefact was built
against — never the artefact's own.**

### How careful an upgrade is

Three levels, and each is exactly one of the two things the Supervisor already
does — which is why this is a policy and not new machinery:

| Level | Same contract version | Contract change |
|---|---|---|
| **Manual** | nothing is fetched | nothing is fetched |
| **Staged** | fetched, verified, activated only when asked | fetched, verified, never activated |
| **Automatic** | fetched, verified and activated | **fetched and staged, never activated** |

- **A contract change is never activated without a person**, at any level. This
  is the invariant the rest is arranged around: the most automation it gets is
  the download and the verification, so the cutover is a decision somebody takes
  with the bytes already on disk and the wait already spent.
- **Crossing a contract boundary is always the careful case**, whatever the
  artefacts' own version numbers say
  ([supervisor#8](0008-artefacts-are-paired-by-contract-major.md)). It is the
  upgrade where the Platform and the Shell change what they say to each other,
  so it is the one a person most needs to have chosen — and it is computable
  rather than editorial, which is what makes this rule enforceable instead of
  advisory.
- **Everything else may be automatic, and it is opt-in.** The default is not
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
- **This governs caution, and nothing else.** The question it answers is "how
  careful should an install be about taking this", which is a product judgement
  about surprise. It is not a claim about what works with what — except that
  here, unlike in the two records this replaces, the two questions happen to
  read the same number.

### Which version is read

- **Mosaic has two published contracts and the rule is the same for both.**
  `sdk` is what a module compiles against; `contracts` is what the Platform, the
  Shell and every other client share. Each artefact declares the version of the
  one it uses, and that declaration is the only version any of this reads.
- **Same contract version: any artefact change is safe.** A module, the Platform
  or the Shell may release without limit, and an install may take those upgrades
  automatically, because nothing about what they say to each other has changed.
- **A contract change is the risky upgrade**, at any artefact version — it is
  the only change that can alter what two processes say to each other, so it is
  the only one a person needs to have chosen.
- **Before 1.0, the breaking component of the contract is the minor.** After 1.0
  it is the major, as semver says. The boundary needs no transition rule: the
  first `1.0.0` is a breaking-class change under both readings.

| Contract version change | Pre-1.0 | 1.0 and after |
|---|---|---|
| `v0.26.1` → `v0.26.2` | not breaking — may be automatic | patch — may be automatic |
| `v0.26` → `v0.27` | **breaking — staged, never automatic** | minor — may be automatic |
| `v0.59` → `v1.0.0` | breaking — staged | major — staged |

- **The same component decides compatibility.**
  [supervisor#8](0008-artefacts-are-paired-by-contract-major.md) pairs the
  Platform and the Shell on what they declare; that pairing reads this
  component, so pre-1.0 it is the contract minor. An artefact whose contract
  component no counterpart has reached is not assemblable, and the install holds
  at the newest pair that is.

## Alternatives considered

**No automation at all.** *Rejected.* It is the safest and it leaves household
boxes on old versions indefinitely, which is how a security fix does not arrive.
The whole point of an install that can verify and revert is that a small upgrade
can be routine.

**Automatic for everything, with the revert as the safety net.** *Rejected.* The
revert catches a Generation that will not *start*; it cannot catch one that
starts perfectly and works differently. The upgrade that changes a contract is
precisely the change where "it came up" is not the question being asked.

**Prompt for everything, including patches.** *Rejected.* A prompt that appears
for every patch is a prompt that gets dismissed, and the one that mattered is
dismissed with it.

**Decide by change size from the release notes.** *Rejected.* It makes the
decision a reading comprehension exercise over text a human wrote, at the moment
nobody is watching. Semver is a claim the publisher makes deliberately, and
holding Mosaic to it is the point of using it.

**Read the artefact's own version, and treat every `0.x` bump as a major.**
*Rejected*, and it was the first thing the second answer considered. Strictly
correct by semver and it makes the setting inert for the whole pre-1.0 life of
the project, which is most of the time anyone will actually be running this. A
control that has never once done anything is one nobody trusts when it starts
to.

**Read the artefact's own version, treating `0.x` as `1.x` under ordinary
semver.** *Rejected.* It would let an install activate a `0.30 → 0.31` change
unattended, on software whose version number is the statement that its
interfaces are still moving.

**Defer the whole pre-1.0 reading until 1.0.** *Rejected.* It is the same as
treating every `0.x` bump as a major, with extra steps: the setting ships, does
nothing, and the rule that makes it work arrives with the release that needed it
least.

**Shift the caution window one place down the artefact's own version.**
*Rejected*, and it is the second answer's own decision, held for one record. It read the
artefact's minor as a major and its patch as a minor while the leading component
was `0`:

| Artefact version change | Pre-1.0 | 1.0 and after |
|---|---|---|
| `0.30.1` → `0.30.2` | minor — may be automatic | patch — may be automatic |
| `0.30.2` → `0.31.0` | **major — staged, never automatic** | minor — may be automatic |
| `0.31.0` → `1.0.0` | major — staged | major — staged |

The shift itself was sound reasoning about how to read a pre-1.0 number; the
number was the wrong one. An artefact's version cannot break anything, so a
window over it is caution spent where no risk is.

**Treat the contract's leading component as breaking, unshifted.** *Rejected* —
it is the second answer's other half, held for the same record, and the
`ClientProfile` incident above is the counterexample. It would have made
`contracts` `v0.59` and `v0.60` interchangeable, which is exactly the skew that
returned `200` and a `nil`.

**Keep watching the artefact version as well, as a second signal.** *Rejected*,
and it is the shape the first answer left behind. A Platform can certainly change
what it *does* without changing what it *says*, which is the argument for it —
but that is a release-notes judgement, not something a version number carries,
and adding it back would make almost every release major again and return the
setting to being inert. The honest answer is that automation covers
compatibility risk and not behavioural surprise, and saying so is better than
implying a number measures something it does not.

**Negotiate at runtime and skip the version check.** *Rejected.* The Shell
already declares its vocabulary on every Attach and the Platform emits only what
it can draw, which is real and genuinely prevents a class of this. It does not
cover the wire itself: a field the client's generated code does not know is
dropped silently in both directions, and no negotiation the client performs can
report a field it has never heard of.

## Consequences

- **Automation becomes useful before 1.0**, which reading the artefact's version
  could not manage under either window. Most releases in these repositories do
  not move the contract, so most upgrades become eligible — and the ones that do
  move it are exactly the ones that stop and wait. That is the behaviour the
  setting was supposed to have.
- **Mosaic's own artefact versioning stops being load-bearing for this**, and
  with it the choice the first answer left to the implementation — treat `0.x`
  minors as majors, or read `0.x.y` as major-minor — no longer has to be made.
  The contract's version carries the decision instead. What that answer said in
  passing survives as the limit stated above: a Platform can change what it does
  without changing what it says, and no version number reports that.
- **Cutting patch releases stops being necessary.** The second answer's consequence
  — that making automation useful meant actually using the patch component for
  fixes, a change to how releases are cut rather than to any code — is withdrawn
  with the window it followed from. The existing minor-per-change cadence across
  these repositories is fine.
- **The hazard of two numbers meaning different things is dissolved rather than
  managed.** The second answer had a shifted window for caution and an unshifted
  leading component for compatibility, and said so as loudly as it could,
  because unifying them would have looked like a simplification and would
  silently have made every contract release a breaking one. Here one component
  answers both questions, so there is nothing to keep apart.
- **Most pre-1.0 upgrades that do move a contract will be staged rather than
  applied**, which is the honest outcome: the download and the verification
  happen unattended, the cutover waits for a person, and the register says a
  version is sitting there.
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
- **A staged Generation occupies disk that nothing reclaims yet.** Generations
  accumulate; a policy for how many are kept is not built and is the obvious
  next thing this needs.
- **The setting is Platform configuration and the actor is the Supervisor**,
  which is the one place they must agree and the Supervisor cannot read the
  Platform's database. How the choice reaches it — the handoff channel, an
  environment variable, a file — is an implementation question this leaves open,
  and it is the same question the upgrade *trigger* has.
- **"Check" becomes a thing that runs on its own**, so the rate limit and the
  freshness gap in
  [supervisor#8](0008-artefacts-are-paired-by-contract-major.md) stop being
  hypothetical.
