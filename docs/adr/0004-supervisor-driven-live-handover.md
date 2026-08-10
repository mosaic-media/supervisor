# Supervisor-driven live-session handover

**Status:** Superseded by [contracts#5](https://github.com/mosaic-media/contracts/blob/main/docs/adr/0005-cross-client-transport-two-lane-rpc.md)
— its bespoke going-away/reconnect handover folds into [contracts#5](https://github.com/mosaic-media/contracts/blob/main/docs/adr/0005-cross-client-transport-two-lane-rpc.md)'s stream resume
(the `Subscribe` cursor replays or rebuilds across a rolling upgrade). Recorded
here for the reasoning; not built as a separate mechanism.
**Date:** 2026-07-20

## Context

The live session ([platform#22](https://github.com/mosaic-media/platform/blob/main/docs/adr/0022-live-session-websocket.md)) holds one
bidirectional WebSocket per client. Slice 3 of the live-client thread made that
session *resilient*: the client reconnects with exponential backoff and jitter,
re-declares its current route on reconnect, and the Platform closes sockets with
a "going away" status (1001) on graceful shutdown so a drop reads as a reconnect
rather than an error. Resume works because the **auth session is DB-backed**
(it survives a Platform restart or a new Generation) and the **live-session
state is disposable** — the client re-declares `{session id, current route}` and
the Platform inherits nothing.

That is enough for a single Generation restarting in place. It is **not** the
full story Mosaic's operational model wants. The Supervisor
([supervisor#1](0001-supervisor-as-host-manager.md)–[platform#4](https://github.com/mosaic-media/platform/blob/main/docs/adr/0004-static-go-module-composition.md))
activates a new **Generation** — a freshly built binary — and the intended
experience is a *seamless* upgrade: clients move from Generation N to N+1
without a visible standby blip, and without cutting a user off mid-playback.
Reconnect-with-backoff gives a *self-healing* handover (drop, standby,
reconnect); it does not give a *seamless make-before-break* one.

The Supervisor is largely unbuilt, so this decision is **recorded now and
deferred**, so the design is not lost and slice 3's reconnect+resume is
understood as step 1 of the path to it rather than the destination.

## Decision

**A live client holds one persistent *control* channel to the Supervisor and
*N* *session* channels to a Generation (normally one; transiently two during a
make-before-break handover). The control plane orchestrates migration; session
channels are direct client↔Generation and the Supervisor stays out of the hot
path. Migration is cooperative and activity-aware.**

### Two channel kinds

- **One control channel — client ↔ Supervisor.** Persistent, authenticated,
  low-traffic. It carries lifecycle orchestration only: "a new Generation is
  available," "migrate to N+1 at endpoint X when it is safe for you,"
  "Generation N is draining." It is **also the recovery-UI fallback authority**:
  when no Generation can serve (a failed activation, a recovery flow —
  [supervisor#2](0002-supervisor-guarantees-an-interface.md)), the control channel
  is what the client still has, so the Supervisor can present a recovery surface
  through it. The control channel outlives any single Generation.

- **N session channels — client ↔ Generation.** The live session of
  [platform#22](https://github.com/mosaic-media/platform/blob/main/docs/adr/0022-live-session-websocket.md), one per Generation the client is
  attached to. Normally exactly one. **Transiently two** during a handover: the
  client opens a session to N+1 while its session to N is still live, renders
  from whichever it has settled on, and closes N once N+1 is serving — *make
  before break*. The Supervisor is **not** a proxy on these; it told the client
  where N+1 is and the client connects directly, so session traffic never routes
  through the control plane.

### Cooperative, activity-aware migration

Migration is **not** a unilateral server disconnect. The old Generation
**drains**:

1. On activation of N+1, the Supervisor tells N to **drain**: stop accepting
   *new* sessions immediately, keep serving *existing* ones.
2. The Supervisor tells each client (over the control channel) that N+1 is
   available at endpoint X and requests migration.
3. **The client asserts when it is safe.** A client mid-playback (or mid-form,
   mid-critical-interaction) replies "not yet"; an idle client migrates at once.
   Safety is the client's call because only the client knows its foreground
   activity. This is the cooperative half — the server proposes, the client
   disposes, within a bound.
4. The client opens a session to N+1, re-declares `{session, route}` (the
   [platform#22](https://github.com/mosaic-media/platform/blob/main/docs/adr/0022-live-session-websocket.md) resume it already does on
   reconnect — the same mechanism, now make-before-break rather than
   after-the-drop), confirms N+1 renders, then closes its session to N.
5. N **shuts down only when all its sessions are gone** — or when a **hard
   deadline** expires, after which it closes remaining sessions with "going
   away" and those clients fall back to the slice-3 reconnect path (which will
   land them on N+1). The deadline bounds a stuck or malicious "never safe"
   client so an upgrade cannot be held hostage forever.

The going-away close and client reconnect+resume built in slice 3 are the
**floor** this stands on: if the cooperative path is skipped (hard deadline,
crash, network fault), the client still self-heals onto N+1. The handover
protocol is the *graceful* layer over that floor, not a replacement for it.

## Alternatives considered

**Reconnect-with-backoff only (slice 3, status quo).** *Insufficient as the end
state.* It gives self-healing but not seamlessness: every upgrade shows a
standby blip, and a client mid-playback is cut and must recover rather than
being allowed to finish. Correct as the foundation; not the whole handover.

**Supervisor proxies all session traffic.** *Rejected.* Routing every live
session through the Supervisor puts it in the hot path, makes it a throughput
bottleneck and a single point of failure for *rendering*, not just
orchestration. The control/session split keeps the Supervisor on the cold path
(lifecycle) and Generations on the hot path (rendering).

**Server-unilateral cutover (close N, let clients reconnect to N+1).** *Rejected
as the primary path.* It is exactly the slice-3 floor with no cooperation: it
ignores foreground activity (cuts mid-playback) and shows a standby blip on
every upgrade. It remains the *fallback* when the deadline expires, not the
design.

**A single channel that the Supervisor hands off at the socket layer.**
*Rejected.* Conflating control and session on one socket means the recovery
authority dies with the Generation, and make-before-break (two live sessions at
once) is impossible with one channel.

## Consequences

- **A seamless upgrade becomes possible** without a visible blip and without
  cutting a user off mid-activity — the operational payoff of the Generation
  model.
- **The Supervisor gains a control-plane protocol** (advertise, request-migrate,
  drain, deadline) and clients gain a control channel distinct from their
  session. This is new surface on both sides, and it depends on the Supervisor
  existing — hence deferred.
- **The client grows a small migration state machine** (hold two sessions,
  assert safety, settle, close the old) on top of the reconnect loop it already
  has. The resume payload is unchanged — `{session, route}` — so the render side
  needs nothing new.
- **The recovery UI has a home**: the control channel is the authority that
  survives when no Generation can serve, which [supervisor#2](0002-supervisor-guarantees-an-interface.md)
  needs a transport for.

Honest limits: this is **design recorded ahead of build**. The message
protocol, the deadline value, the "safe to migrate" vocabulary, and how the
control channel authenticates relative to the session are all left to the slice
that builds it — alongside the Supervisor itself. What is decided here is the
**shape**: control vs session channels, direct client↔Generation session
traffic, and cooperative activity-aware draining with a hard-deadline fallback
onto the slice-3 reconnect floor.

## Implementation implications

Not built in the live-client thread's slice 3. Slice 3 delivered the floor:
client reconnect (backoff + jitter), route re-declaration on reconnect, and the
Platform's going-away close on graceful shutdown. Building *this* ADR waits on
the Supervisor ([supervisor#1](0001-supervisor-as-host-manager.md)–[platform#4](https://github.com/mosaic-media/platform/blob/main/docs/adr/0004-static-go-module-composition.md)),
and will add: the control-channel transport and protocol on both Supervisor and
client, the drain state on a Generation's live surface, and the client's
two-session migration state machine. Sequenced with the Supervisor's Generation
activation work, not before it.
