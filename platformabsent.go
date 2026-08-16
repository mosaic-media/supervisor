// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"encoding/json"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/mosaic-media/contracts/gen/mosaic/auth/v1"
	"github.com/mosaic-media/contracts/gen/mosaic/auth/v1/authv1connect"
	sessionv1 "github.com/mosaic-media/contracts/gen/mosaic/session/v1"
	"github.com/mosaic-media/contracts/gen/mosaic/session/v1/sessionv1connect"
)

// The Supervisor answering the Platform's own client surface, while there is no
// Platform to answer it (supervisor#7).
//
// One SDUI source, not two. A client asks the address it always asks; when the
// Platform is serving, the front door proxies and the answer is Mosaic's; when it
// is not, the Supervisor answers the same calls with its own state. The switch is
// the front door's and no client participates in it — the alternative is every
// client, on every platform, hand-coding a second source and a rule for choosing
// between them.
//
// It answers with its own status and with nothing else, ever. It cannot
// authenticate — the sessions are in a database belonging to the process that is
// down — so it does not try, and that is only safe because everything it can say
// is already public: the same phase, sentence and child summary the recovery
// page serves unauthenticated to anyone who can reach the port. The moment
// something here can answer with anything a session would have gated, this stops
// being a projection of public status and becomes an authentication bypass.
// Nothing in this file may read a database, a settings document or a library.

// authAbsent and sessionAbsent are two types because both services declare an
// Invoke and one Go type cannot carry two methods of that name. They share the
// same state function and the same rule; splitting them costs a struct and keeps
// the generated interfaces as the thing being satisfied, which is what makes a
// contract change here a compile error rather than a route that quietly stops
// being answered.
type authAbsent struct{ state func() RecoveryState }

type sessionAbsent struct{ state func() RecoveryState }

var (
	_ sessionv1connect.SessionServiceHandler = (*sessionAbsent)(nil)
	_ authv1connect.AuthServiceHandler       = (*authAbsent)(nil)
)

// Bootstrap answers a client that has no session yet — a fresh browser, or a
// first boot where the Platform has never existed.
//
// The doorway here is the Supervisor's screen. A client renders whatever this
// returns exactly as it renders the Platform's own door, so a client that opens
// the page mid-install sees the install rather than a sign-in form for a server
// that cannot check a password.
//
// No tokens and no definitions, and both absences are correct rather than
// missing work: the skin is Platform-delivered (contracts#4) and there is no
// Platform, and the tree is primitives only (supervisor#6) so there is nothing to
// define. A client draws it with the vocabulary it shipped with.
func (p *authAbsent) Bootstrap(
	_ context.Context, _ *connect.Request[authv1.BootstrapRequest],
) (*connect.Response[authv1.BootstrapResponse], error) {
	return connect.NewResponse(&authv1.BootstrapResponse{
		UiNode: recoveryScreen(p.state()).Build(),
	}), nil
}

// SignIn and Invoke are refused, not faked.
//
// A credential must never be answered by anything that cannot check it. An Ack
// here would tell a client it had signed in when nothing verified anything, and
// a client is entitled to believe an answer on this route. Unavailable is the
// truth: there is a thing that can check it, and it is not running.
func (p *authAbsent) SignIn(
	_ context.Context, _ *connect.Request[authv1.SignInRequest],
) (*connect.Response[authv1.SignInResponse], error) {
	return nil, errPlatformAbsent()
}

func (p *authAbsent) Invoke(
	_ context.Context, _ *connect.Request[authv1.InvokeRequest],
) (*connect.Response[authv1.InvokeResponse], error) {
	return nil, errPlatformAbsent()
}

// Refresh is refused for the same reason and with particular care about which
// refusal it is.
//
// Unavailable, never Unauthenticated. A client answers Unauthenticated by
// discarding its credential and signing out (platform#58) — so answering that here
// would destroy a perfectly good refresh chain because the server it belongs to
// was restarting. The chain is fine; the thing that can rotate it is not
// running, which is what Unavailable says.
//
// A client whose access token expires during a long outage therefore cannot
// renew it, and must present the one it has. That is the correct division: an
// expired token is not this process's to judge, and the Platform will refuse it
// properly the moment it is back.
func (p *authAbsent) Refresh(
	_ context.Context, _ *connect.Request[authv1.RefreshRequest],
) (*connect.Response[authv1.RefreshResponse], error) {
	return nil, errPlatformAbsent()
}

// SignOut is refused rather than acknowledged, unlike the session intents below.
//
// Acking a no-op is honest when nothing was supposed to change. Signing out is
// the one act where a client believing it succeeded is materially worse than an
// error: revocation happens server-side, so an Ack here would leave a live
// session on a server that never heard about it while the person who asked
// walked away believing otherwise.
func (p *authAbsent) SignOut(
	_ context.Context, _ *connect.Request[authv1.SignOutRequest],
) (*connect.Response[authv1.SignOutResponse], error) {
	return nil, errPlatformAbsent()
}

// Attach, Navigate, Invoke and SubmitInput are acknowledged and do nothing.
//
// Acked rather than refused because they are not requests for information — the
// client is declaring a route or streaming a keystroke, and the visible result
// of every one of them arrives on the push lane (contracts#5), which is answering
// with the Supervisor's screen regardless of what was asked. Failing them would
// make the Shell log an intent failure per reconnect for a state where nothing
// is wrong with the intent.
//
// Nothing here is performed, which is what makes acknowledging them honest:
// there is no state to change and none is changed.
func (p *sessionAbsent) Attach(
	_ context.Context, _ *connect.Request[sessionv1.AttachRequest],
) (*connect.Response[sessionv1.Ack], error) {
	return connect.NewResponse(&sessionv1.Ack{}), nil
}

func (p *sessionAbsent) Navigate(
	_ context.Context, _ *connect.Request[sessionv1.NavigateRequest],
) (*connect.Response[sessionv1.Ack], error) {
	return connect.NewResponse(&sessionv1.Ack{}), nil
}

func (p *sessionAbsent) Invoke(
	_ context.Context, _ *connect.Request[sessionv1.InvokeRequest],
) (*connect.Response[sessionv1.Ack], error) {
	return connect.NewResponse(&sessionv1.Ack{}), nil
}

func (p *sessionAbsent) SubmitInput(
	_ context.Context, _ *connect.Request[sessionv1.InputRequest],
) (*connect.Response[sessionv1.Ack], error) {
	return connect.NewResponse(&sessionv1.Ack{}), nil
}

// Subscribe is the push lane, answered by the Supervisor: it pushes its own
// screen as the shell, re-pushes it whenever it changes, and ends the stream the
// moment the Platform is serving.
//
// That ending is the handover, and it needs nothing on the client side. A
// client's reconnect loop is already how it survives a Platform restart; here
// the stream closes cleanly, the client reconnects the way it always does, and
// this time the front door proxies it to a Platform that is up. Nobody polls,
// nobody is told to refresh, and no client holds a rule about when to stop
// drawing this.
//
// It does not authenticate the session token, and cannot — see the file comment.
func (p *sessionAbsent) Subscribe(
	ctx context.Context,
	_ *connect.Request[sessionv1.SubscribeRequest],
	stream *connect.ServerStream[sessionv1.ServerMessage],
) error {
	ticker := time.NewTicker(sseInterval)
	defer ticker.Stop()

	var last string
	for {
		state := p.state()
		if state.Phase == PhaseReady {
			// Serving. End the stream rather than pushing a "we are fine"
			// screen the client would have to know to discard.
			return nil
		}
		// Compared on the rendered form for the same reason the event stream
		// does it: the Supervisor's state has no change notification, and
		// adding one would mean threading a signal through the child manager,
		// the fetcher and the activator to save a string comparison in a
		// process that is otherwise idle.
		if fragment := RecoveryFragment(state); fragment != last {
			last = fragment
			if err := stream.Send(supervisorShell(state)); err != nil {
				return err
			}
			if err := stream.Send(supervisorEvent(state)); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// supervisorShell wraps the Supervisor's screen as a shell update.
//
// Seq stays zero, and that is load-bearing. A client stores the last seq it saw
// and sends it back as the resume cursor on reconnect — to the Platform, which is
// the next thing it will reach. A cursor minted here would be a number from one
// server presented as a position in another's stream, and the Platform would
// honour it by replaying from a point that means nothing. Zero is "I have seen
// nothing", which is exactly true of the Platform's stream.
func supervisorShell(s RecoveryState) *sessionv1.ServerMessage {
	return &sessionv1.ServerMessage{
		Seq: 0,
		Body: &sessionv1.ServerMessage_Shell{
			Shell: &sessionv1.ShellUpdate{UiNode: recoveryScreen(s).Build()},
		},
	}
}

// supervisorEvent says, in a field rather than in the prose, that this is the
// Supervisor speaking and which phase it is in.
//
// It is the one thing a client needs that the tree cannot carry. Everything
// visible is in the screen and needs no interpretation, but a client may
// reasonably want to know it is not talking to Mosaic — to keep an "offline"
// affordance out of the way, or to leave a sign-out control alone. Naming it
// here means a client that wants that reads a field, and one that does not
// ignores an event type it does not know, which every client already does.
func supervisorEvent(s RecoveryState) *sessionv1.ServerMessage {
	payload, err := json.Marshal(struct {
		Phase   string `json:"phase"`
		Version string `json:"version,omitempty"`
		BootID  string `json:"bootId,omitempty"`
	}{Phase: string(s.Phase), Version: s.Version, BootID: s.BootID})
	if err != nil {
		payload = []byte("{}")
	}
	return &sessionv1.ServerMessage{
		Seq:  0,
		Body: &sessionv1.ServerMessage_Event{Event: &sessionv1.Event{Type: supervisorStateEvent, Payload: payload}},
	}
}

// supervisorStateEvent is the event type carrying the phase. Namespaced to the
// Supervisor so it cannot collide with a Platform event, and unknown to every
// client that does not care — which is the correct default for it.
const supervisorStateEvent = "supervisor.state"

// errPlatformAbsent is the one error this file returns, and it is Unavailable
// rather than Unauthenticated on purpose: a client answers Unauthenticated by
// discarding its credential (platform#58), and the credential is fine — the thing
// that checks it is not running. Signing somebody out because their server
// restarted would destroy a refresh chain that had done nothing wrong.
func errPlatformAbsent() error {
	return connect.NewError(connect.CodeUnavailable,
		errString("Mosaic is not running. The Supervisor is starting it."))
}

// errString is a minimal error so this file does not reach for a dependency to
// build one.
type errString string

func (e errString) Error() string { return string(e) }
