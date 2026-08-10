# The Supervisor answers the Platform's client surface when the Platform is absent

**Status:** Built. The front door switches, both services are answered, and the
Shell needed no code for it.
**Date:** 2026-08-09

Completes [supervisor#2](../../../../../workspace/supervisor/docs/adr/0002-supervisor-guarantees-an-interface.md)'s "one emitter,
many renderers" by removing the second *source* a client would otherwise have
had. Partly supersedes
[supervisor#6](../../../../../workspace/supervisor/docs/adr/0006-two-supervised-images-and-a-diy-path.md): the Supervisor's import
boundary widens from one module to two.

## Context

[supervisor#2](../../../../../workspace/supervisor/docs/adr/0002-supervisor-guarantees-an-interface.md) has the Supervisor emit its own SDUI so that a person is told what is
happening when the Platform cannot tell them. The emitter was built, and so were
two renderers: the embedded htmx page, and the Shell.

The Shell's version is where the design went wrong, and it went wrong in a way
that only shows up on the *second* client. It was given a `useSupervisor` hook: a
fetch of `/supervisor/ui`, an `EventSource` on `/supervisor/ui/events`, a poll
beneath that as a floor, a three-state presence so an absent Supervisor could be
told from an unanswered one, a branch choosing which of two trees to draw, and a
`reconnect()` added to the live session so the client could abandon its backoff
when the Supervisor said the Platform was serving. About 150 lines, all correct,
all verified in a browser.

All of it is a **rule about when to ask a different server**, and a rule is not
transferable. A Flutter client, a TV client and every client anyone writes later
would each have to implement the same rule from a description rather than from a
contract — and the failure mode of getting it slightly wrong is silent, because a
client that never asks the Supervisor looks exactly like one asking a Supervisor
that has nothing to say.

The reasoning that produced it is worth recording because it was not stupid. A
Supervisor-owned path was chosen deliberately so that *a client could never draw
the Supervisor's screen believing it was Mosaic's*: asking `/supervisor/ui` is
asking a different question, and every client that renders the answer knows which
question it asked. That objection is real. It is also solvable in one line — say
who is speaking in the payload — whereas the cost it bought is not solvable at
all.

## Decision

**The Supervisor answers the Platform's own client surface while the Platform is
not serving.** A client calls the address it always calls. The front door
proxies to the Platform when the Platform is ready, and answers the same two
Connect services itself when it is not.

- **The switch is the front door's, and no client participates in it.** There is
  no endpoint a client must know to ask, no presence to establish and no rule for
  choosing between sources. A client that has never heard of the Supervisor gets
  the Supervisor's screen, drawn with the renderer it already has.
- **`Subscribe` ends when the Platform is serving.** That ending *is* the
  handover: the client's ordinary reconnect — the same one that already survives
  a Platform restart — reconnects, and is proxied to the Platform this time.
  Nothing polls, nothing is told to refresh, and no client holds a rule about
  when to stop drawing the Supervisor.
- **It answers with its own status and nothing else, ever.** It cannot
  authenticate, so it does not try; that is safe only because everything it can
  say is already served unauthenticated to anyone who can reach the port. The
  moment anything here can answer with something a session would have gated, this
  is an authentication bypass rather than a projection of public status.
- **Credential calls are refused, not faked**, and refused as `Unavailable`
  rather than `Unauthenticated` — a client answers the latter by discarding its
  refresh chain ([platform#58](https://github.com/mosaic-media/platform/blob/main/docs/adr/0058-the-session-credential-is-a-bearer-pair.md)), and signing somebody out because their server
  restarted would destroy a credential that had done nothing wrong. The session
  intents are Acked instead, because they change nothing and their visible result
  arrives on the push lane regardless.
- **A `supervisor.state` event carries the phase**, so a client that wants to
  know it is not talking to Mosaic reads a field rather than the prose. Nothing
  is required to read it; an unknown event type is already ignored by every
  client.
- **The resume cursor stays zero.** A sequence number minted here would be
  handed back to the *Platform* as a position in a stream it never sent.
- **The import boundary widens to two modules**: the published contract, and
  `connectrpc.com/connect`. This adds nothing to the build graph — the contract
  already requires Connect to generate those handlers — so the module moves from
  transitive to direct. The boundary counts direct imports because those are what
  a reader can audit; the set of code that must work when everything else is
  broken is unchanged.

## Alternatives

**Keep the client-side switch.** Rejected on the grounds above: it is a rule
every client reimplements, and the objection it was protecting against is
answerable in a field.

**Hand-roll the Connect wire format** rather than admit the dependency. The
unary JSON protocol is simple and the streaming envelope is not much worse, so
this was genuinely available. Rejected because it is a second implementation of a
wire format inside the process that must not have surprises, to avoid a module
that is already in the build graph — and because this project has paid for a
second implementation of a contract once already.

**Have the Platform's client redirect to the Supervisor.** There is no Platform
to issue the redirect. This is only worth stating because it is the shape a
reader reaches for first.

**Answer only `Bootstrap`, and let a signed-in client fall back to Standby.**
Cheaper, and wrong in the case that matters most: a signed-in client is the
ordinary one, and an upgrade is exactly when somebody is watching.

## Consequences

- The Shell has no code about the Supervisor at all. `useSupervisor`,
  `SupervisorHost` and `reconnect()` are deleted; what remains is a
  `!session` condition on the doorway, which fixed a defect of its own.
- **`/supervisor/ui` is removed.** Its only audience was a client implementing
  the rule this replaces. The fragment and event-stream endpoints stay: they
  serve the embedded page, which is a renderer rather than a client.
- A client whose access token expires mid-outage cannot renew it — the Supervisor
  has no database — so a client must present the credential it holds rather than
  treating a failed renewal as fatal. The Platform refuses it properly when it
  returns.
- The switch reads the health report rather than discovering the Platform by
  dialling, because a stream's request body is gone by the time a dial has
  failed. The cost is a lag of one health probe, during which a client gets the
  `unavailable` it got before this existed.
- **A standing notice the Platform issued survives onto the Supervisor's
  screen**, because retraction is by name and the process that would retract it
  is the one that is gone. Cosmetic, recorded rather than worked around.
- The Supervisor can now say things a client will render. Every sentence it emits
  is a sentence a person reads while something is wrong, which is a higher bar
  than the emitter had when only its own page drew it.
