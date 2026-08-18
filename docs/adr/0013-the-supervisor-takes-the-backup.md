# The Supervisor takes the backup, and the Platform contributes its half

**Status:** Accepted. **Not built.** Gives
[platform#92](https://github.com/mosaic-media/platform/blob/main/docs/adr/0092-module-storage-is-granted-not-enforced.md)
the backup boundary it depends on for its strongest argument.

## Context

Mosaic has one PostgreSQL and no documented restore path, and
[platform#2](https://github.com/mosaic-media/platform/blob/main/docs/adr/0002-module-storage-and-delivery-model.md)
gave the Platform "the backup boundary" without anything implementing it.
[platform#92](https://github.com/mosaic-media/platform/blob/main/docs/adr/0092-module-storage-is-granted-not-enforced.md)
then leaned on that boundary as the sharpest reason a module should use its
granted storage rather than bringing its own store — and recorded honestly that
the argument was not cashable, because the boundary had no restore path to be
inside of.

**Durable state is in three places, not one.** PostgreSQL. The extensions
directory, holding verified module binaries, cached manifests and — under
[platform#92](https://github.com/mosaic-media/platform/blob/main/docs/adr/0092-module-storage-is-granted-not-enforced.md) — storage grants. And the instance file, which holds the install's
identity and is kept outside the database deliberately, "so it still answers when
the database does not". A `pg_dump` is therefore not a backup of this system.

**They can disagree after a restore.** Boot re-adopts the pinned module bytes
from disk rather than following a catalogue, so a database from Tuesday restored
against a directory from Thursday disagrees with itself about what is installed —
and boots anyway.

## Decision

**The Supervisor owns backup and restore.** This follows Home Assistant's
division, where the Supervisor rather than Core takes and restores backups, for
the reason that makes that division work: **the thing taking the backup must not
be the thing that is broken.** A backup path living in the Platform has to work
when the Platform is unhealthy, which is precisely when somebody reaches for it.

**The Platform produces the database half on request; the Supervisor takes the
filesystem half and assembles the archive.** The Supervisor may depend only on
the standard library, the contract, Connect and OpenTelemetry
([supervisor#6](0006-two-supervised-images-and-a-diy-path.md),
[supervisor#7](0007-the-supervisor-answers-the-platforms-client-surface.md)), so
it has no database driver and may never grow one. Only the Platform can quiesce
writes and export consistently; only the Supervisor can act when the Platform
cannot. Neither half is optional and neither party can do both.

**A backup taken while the Platform is unhealthy is a partial backup that says
so.** The Supervisor takes the filesystem half regardless and records in the
manifest that the database half is absent. Something outside the failure can
still act, which is the whole reason the division exists — and a partial backup
that names what it lacks is worth more than a failed one.

**The archive is a single file carrying a manifest**, so a restore can inspect
before it acts: what was included, what was deliberately excluded, the Platform
version that produced it, and whether the database half is present.

**Full and partial, as Home Assistant has them.** Module storage under
[platform#92](https://github.com/mosaic-media/platform/blob/main/docs/adr/0092-module-storage-is-granted-not-enforced.md) can hold recordings, so a full backup can be enormous. Storage is
**inside** the boundary and included by default, which is what that record's
promise depends on — but a partial backup may exclude a module's storage, and the manifest
records the exclusion so a restore knows what it will not be restoring rather
than discovering it later.

**The archive is encrypted with a key the operator holds, and a restore needs
it.** It contains password hashes, module settings with third-party API keys and
sealed values; unencrypted, every copy is a full compromise of the install. The
cost is real and is not hidden: a lost key is a lost backup.

**A restore invalidates every session, drops every cache, and lets jobs re-derive
from state.** A restore may be onto different hardware at a different address, so
nothing that assumed continuity survives it. **The sharp reason is that a
surviving session would be a credential resurrected after it may have been
deliberately revoked** — a backup must not be a way to undo a revocation.

**A restore refuses onto a Platform older than the backup**, naming both
versions. Migrations move forward only, so an older binary meeting newer schema
is a corruption path rather than an error to report afterwards.

**A restore whose origin differs from the backup's says what that costs.**
Passkeys are bound to a relying-party id
([platform#78](https://github.com/mosaic-media/platform/blob/main/docs/adr/0078-passkeys-are-an-optional-layer-on-a-public-origin.md)),
so a restore onto a different origin invalidates every one of them. That is
stated at restore time rather than discovered at the next sign-in.

## Alternatives considered

**The Platform takes its own backup.** It already has the database connection and
knows what is safely omitted. Rejected on the failure case: the component that
needs backing up is the one most likely to be unable to do it, and a backup
feature that only works on a healthy install is one that works when it is not
needed.

**A documented procedure the operator runs.** No new code, and it composes with
whatever backup tooling a deployment already has. Rejected because consistency
across a database and a directory cannot be promised by two separate commands,
and the failure is silent — a restore that boots and is quietly wrong about which
modules are installed.

**The database only, rebuilding on-disk state by reinstalling.** Small and
familiar. Rejected: module storage holds content that is not rebuildable, and the
instance file is not either. It would quietly retract what [platform#92](https://github.com/mosaic-media/platform/blob/main/docs/adr/0092-module-storage-is-granted-not-enforced.md) promised.

**Including the Supervisor's Generations.** A restore would return the whole
machine to a known state. Rejected: Generations are large, and an artefact store
belongs to the upgrade path rather than to a backup of an install's data.

**An unencrypted archive.** Nothing to lose and it can be inspected. Rejected: it
is a credential trove that will sit on a NAS.

**Excluding secrets so the archive is safe anywhere.** Rejected: a restore would
leave every module unconfigured and nobody able to sign in, which is a rebuild
wearing a restore's name.

**Sessions surviving a restore.** Better for a same-machine recovery, where
nobody would notice. Rejected on the revocation argument above.

## Consequences

- **Two components must agree on an archive format**, and the Supervisor cannot
  read the database half it is carrying. A malformed export is something it can
  neither detect nor repair, so the manifest has to carry enough for a restore to
  refuse cleanly.
- **A partial backup is a normal outcome, not an error state**, which means every
  surface that lists backups has to show what each one lacks. A backup that looks
  complete and is not is worse than no backup.
- **The encryption key is a new thing an operator must keep**, and losing it is
  indistinguishable from never having taken a backup.
- **[platform#92](https://github.com/mosaic-media/platform/blob/main/docs/adr/0092-module-storage-is-granted-not-enforced.md)'s incentive becomes real.** Storage inside the grant is backed up
  and a module's own store is not — which was the argument all along, and is only
  now true.
- **Restore is the first operation that spans both processes in both directions.**
  The Supervisor asks the Platform for an export and later hands one back, which
  is a coupling the handoff has not carried before.
