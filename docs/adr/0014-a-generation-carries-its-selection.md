# A Generation carries its selection, and a screen asks for a change

**Status:** Accepted. **Not built.** Depends on
[platform#38](https://github.com/mosaic-media/platform/blob/main/docs/adr/0038-platform-binary-built-by-ci.md)
for a Generation that is selected rather than built, and on
[platform#74](https://github.com/mosaic-media/platform/blob/main/docs/adr/0074-operational-findings-are-durable-state.md)
for the channel the request travels. Closes the second of the three items on
[architecture](https://github.com/mosaic-media/architecture/blob/main/docs/index.md)'s
deliberately-undecided list.

## Context

The list called this the Supervisor's *build-time* selection of core modules, and
that framing died with [platform#38](https://github.com/mosaic-media/platform/blob/main/docs/adr/0038-platform-binary-built-by-ci.md):
every core module is compiled into the one
binary CI publishes, so a selection decides which are **wired in**, not which are
present. What survives from [supervisor#3](0003-supervisor-orchestrates-isolated-builds.md)
is that the Supervisor owns the desired composition — it selects and activates,
it no longer compiles.

The selecting half is built, in the Platform. A core module the selection does
not name is never constructed: its `New` is not called, nothing it holds is
opened, and no registry sees it. A selection naming a module the binary does not
carry is refused, because otherwise a typo would drop the module it meant to keep
and say nothing. The default is every core module, so an unconfigured deployment
still gets the metadata floor.

What is not built is where the selection comes from. The Platform reads one from
its environment and its own comment says that is a bridge until the Supervisor
exists. Meanwhile a Generation here is deliberately boring: a directory of
verified binaries, a version naming them, and a pointer with exactly two fields —
what is live and what was live before it. There is nowhere in that shape to put a
selection, which is the question rather than an incidental gap.

There is also no way for a person to change one. That makes it *owed* by the
roadmap's own rule — a working capability with no client path — and a debt is
discharged by building the path or by deciding it is not a client capability,
never by leaving it unstated.

## Decision

**A Generation carries its selection, and activating a selection is activating a
Generation.**

The reason is one property nothing else on the table has: **rollback**. A
selection that breaks a deployment is then undone by the same one-pointer move
that undoes a bad release, using machinery that already exists and is already
exercised. The alternatives each lose it in a different way, and the loss is
quiet in both.

The Platform does not stop reading a selection from its environment — that
becomes the *interface* rather than the bridge. `Child.Env` already carries
entries into the child process, so a Generation's selection reaches the Platform
by the route every other piece of its configuration takes. What changes is who
writes it: the Supervisor, from the Generation it is activating, instead of a
person in a compose file.

**An administrator reaches it through a screen, and the request travels the path
that already exists.** The Platform cannot write the selection itself — Generation
state is the Supervisor's and is deliberately kept off the public port
([platform#75](https://github.com/mosaic-media/platform/blob/main/docs/adr/0075-the-children-listen-on-unix-sockets.md)) —
so at first sight a screen needs a channel from the Platform to the Supervisor
that points the wrong way. It does not. The upgrade control is the same shape and
is already built: the Platform records a pending request on its private handoff
listener, the Supervisor polls it every ten seconds, carries it out, and writes
the outcome to the spool the Platform adopts as Issues. A selection change is that
flow with a different verb, and it costs no new surface, no new listener and no
new direction of trust.

**The refusal moves in front of the activation.** A selection leaving a required
role class empty is already refused — but if that refusal happens only at boot,
the Platform stops, and the screen that would fix it is served by the Platform
that will not start. With the screen in front, an impossible selection is refused
while a person is looking at it. The Supervisor validates again before activating,
because a screen is not the only way a request can arrive and a check that exists
in one place is a check that can be bypassed by the other.

## Alternatives considered

**Platform configuration the Supervisor passes through unchanged.** *Rejected:*
closest to what runs today, and it decouples the selection from the release it was
made against. A rollback then restores the old binaries under the new selection —
a combination nobody chose and nobody tested.

**An operator setting in the database, applied at next boot.** *Rejected:* it is
where every other operator setting lives, and it is a bootstrap trap. A selection
that leaves a required class empty correctly stops the Platform booting, and the
row that would fix it is behind the Platform that will not boot.

**No screen — deployment configuration only.** *Rejected:* it would be consistent
with a selection living in the Generation, and it leaves "stop showing me this
source" with no answer inside the product. The debt would close by declaration
rather than by being paid.

**A selection in the Generation *and* an override in Platform config.**
*Rejected without being asked:* two sources for one fact, which the fleet's own
rules exist to prevent. The override would win, silently, on precisely the
deployments where somebody had forgotten it was set.

## Consequences

**A Generation stops being "binaries and a version".** It now carries
configuration, so the pointer's two fields no longer fully name what is running —
the directory does. That is a real cost, paid deliberately: the alternative was a
selection with no rollback.

**A rollback can now change which modules are wired in.** That is the point, and
it is a bigger event than "the old binaries came back": rolling back can remove a
source a household was using. It belongs in what a rollback reports, not left for
somebody to discover from an empty row.

The environment variable the Platform reads stops being a bridge and becomes the
contract between the two processes, so removing it is not the tell that this
landed — the Supervisor writing it is.

Nothing here is built. The Generation shape, the request path and the screen move
together, and until they do the selection is whatever a person put in the
environment, which is the state this record describes rather than the one it
leaves.
