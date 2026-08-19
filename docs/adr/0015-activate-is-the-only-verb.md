# Activate is the only verb, and a version panel is where it happens

**Status:** Accepted. **Not built.** Discharges the rollback debt
`architecture/docs/unreachable-capability.md` records as owed. Rides
[supervisor#14](0014-a-generation-carries-its-selection.md)'s request path and
completes the half
[platform#77](https://github.com/mosaic-media/platform/blob/main/docs/adr/0077-the-upgrade-channel-is-the-handoff-and-the-register.md)
built going forwards.

## Context

`Activator.Rollback` swaps the pointers rather than dropping one — the detail that
makes a failed rollback still have somewhere to go — and nothing calls it outside
Go. `Updater.UpgradeTo` is in the same position: installing a *named* version,
newer or older, is Go-only.

Going forwards has a control: a finding names a version and a suggestion applies
it. Going backwards has none, and that is the wrong way round for the case that
matters. **A version that installs, comes up, and is simply worse is exactly what
the automatic revert cannot catch**, because that revert gates on "did it start".
The one situation needing a person's judgement is the one with no control.

The reason it did not land with the upgrade half is that the two are not
symmetrical. An upgrade is offered by a finding that names a version. A rollback
has nothing to hang on, because no detector says "this version is worse" — a
person does.

## Decision

**There is one verb, `activate`, and it takes a version.** Rollback stops being a
distinct operation and becomes activating the previous Generation; installing a
named newer version is the same call with a different argument. `Rollback` and
`UpgradeTo` are two spellings of one thing, and the asymmetry above dissolves once
the surface stops being a *reaction to a finding* and becomes a *list of versions*.

**The surface is a version panel** listing the Generations this install holds —
which is live, which was live before it, and any others retained on disk — each
with activate. That answers the "nothing to hang on" problem directly: the panel
is always there, so it does not depend on a detector having fired or an Issue
still being open. A person who concludes on Thursday that Tuesday's release is
worse has somewhere to go.

**It travels supervisor#14's request path**, which is the same path the upgrade
control already uses: the Platform records a pending request on the private
handoff listener, the Supervisor polls it, acts, and writes the outcome to the
spool the Platform adopts as Issues. No new listener, no new direction of trust,
and one mechanism carrying selection and version alike.

**The private endpoints stay private.** The Generation facts a panel renders reach
an authenticated administrator through the Platform's own authorized surface,
which is a different thing from publishing the handoff endpoint — that stays off
the public port exactly as
[platform#75](https://github.com/mosaic-media/platform/blob/main/docs/adr/0075-the-children-listen-on-unix-sockets.md)
requires. Activating a version is an authorized action like any other, so it is
subject to the same gate rather than being available to anyone who can reach the
screen.

## Alternatives considered

**A revert action on the upgrade Issue, open for a window.** *Rejected:* it reuses
the register with no new surface, and it only exists in the window after an
upgrade. "This is worse" is a realisation that arrives days later, by which time
the Issue is resolved and gone — so the control would be absent precisely when it
is wanted.

**A dismissible banner after an upgrade.** *Rejected:* the same window problem,
with less to act on.

**Separate rollback and install-named-version controls.** *Rejected:* two surfaces
for one operation, and the difference between them is only which direction the
version number moved.

**Automatic rollback on a health signal.** *Rejected:* it already exists for "did
it start", and extending it to "is it worse" requires a detector for a judgement
nobody can automate. That is the premise of this record rather than an option
under it.

## Consequences

**A person can activate an older Generation than the one they upgraded from**,
which the pointer's two fields do not model — they name what is live and what was
live before it, not a history. That stays true and stays sufficient: the
directories are still on disk and named by version, and a panel reads the
directory rather than the pointer.

**A rollback can now change which core modules are wired in**, because
supervisor#14 put the selection in the Generation. That is the intended
composition of the two records and it is a bigger event than "the old binaries came
back" — it belongs in what the panel says before a person presses the button, not
discovered afterwards from a missing source.

**Retention becomes a real policy rather than an implementation detail.** A panel
that lists Generations makes visible how many are kept, and "what gets pruned and
when" stops being something only the disk knows.
