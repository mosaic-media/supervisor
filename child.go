// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// NewID returns a short random hex identifier, matching the shape the
// Platform's telemetry resource uses so boot ids look alike in a log somebody
// is reading across both processes.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// ChildState is what the Supervisor believes about one child.
type ChildState string

const (
	ChildStarting ChildState = "starting"
	ChildReady    ChildState = "ready"
	ChildStopped  ChildState = "stopped"
	ChildFailed   ChildState = "failed"
)

// ChildSnapshot is the reportable view, carried by the health probe.
type ChildSnapshot struct {
	Name     string     `json:"name"`
	State    ChildState `json:"state"`
	PID      int        `json:"pid,omitempty"`
	Restarts int        `json:"restarts"`
	// ConsecutiveFailures is the current run of failed starts, cleared once
	// the child is ready. Distinct from Restarts, which only ever grows.
	ConsecutiveFailures int `json:"consecutiveFailures"`
	// Unrecoverable says the Supervisor has stopped expecting this child to
	// come up. It is still retrying — the flag is what makes the condition
	// reportable rather than something only a log reveals, and it is where
	// ADR 0119's Issue will be raised once that exists.
	Unrecoverable bool   `json:"unrecoverable,omitempty"`
	LastErr       string `json:"lastError,omitempty"`
}

// The two children this Supervisor knows by name.
//
// **Named because the front door branches on one of them.** Whether the
// Supervisor answers the Platform's client surface itself turns on the Platform
// child being ready (ADR 0123), and writing that check against a string literal
// far from the registration that produced it would mean a rename switched the
// takeover off with nothing failing anywhere — the exact shape of silent
// breakage this repository keeps finding.
const (
	PlatformChildName = "platform"
	ShellChildName    = "shell"
)

// ChildSpec describes a process the Supervisor owns.
type ChildSpec struct {
	// Name is the reporting name — PlatformChildName or ShellChildName.
	Name string
	// Command is argv. Empty means there is nothing to run *yet*, which is two
	// opposite situations — see Managed.
	Command []string
	// Managed says the Supervisor owns this process's lifecycle.
	//
	// **It exists because an empty Command means two opposite things**, and
	// inferring from emptiness alone made one of them impossible. A dev stack
	// or a DIY deployment supplies no command because something else runs the
	// process; a first boot supplies none because the binary has not been
	// downloaded yet. Both are `Command == nil`, and only the second may be
	// given one later.
	//
	// Inferring it cost a first boot that fetched and verified a Generation
	// perfectly and then refused to start it, reporting that "the Supervisor
	// does not own its lifecycle" — about the child it had just been told to
	// provision. Nothing failed; the install simply sat at "starting" forever.
	Managed bool
	// Env is added to the Supervisor's own environment.
	Env []string
	// Readiness is the child's own assessment of itself, and must answer
	// 2xx/3xx. Nil means "running is ready", which is weaker but honest for a
	// process with no probe.
	Readiness *Probe
	// Serving is the surface a *client* reaches, and is checked because a
	// process's opinion of itself is not the question a Supervisor is asking.
	// A component can report every subsystem loaded while the listener a user
	// arrives at is unbound or unrouted, and only a probe of that listener
	// tells them apart.
	//
	// It is satisfied by **any** HTTP response, not a 2xx. The surface worth
	// probing is the RPC path clients actually use, which correctly refuses a
	// GET — so demanding success would mean issuing a real RPC, and the one
	// call reachable before authentication is rate-limited and does real work
	// (ADR 0101). A probe that consumed a user's budget, or that reported a
	// healthy Platform unready because it had spent its own, would cause the
	// restarts it exists to prevent. An answer of any kind proves the listener
	// is bound and the mux is routing, which is exactly the gap Readiness
	// leaves. It is a separate Probe rather than another path because the two
	// are different listeners — and, under ADR 0120, different sockets, so
	// they need different dialers and are not interchangeable.
	Serving *Probe
	// MaxConsecutiveFailures is how many starts may fail in a row before the
	// Supervisor reports the child as one it cannot bring up. Zero means
	// defaultMaxConsecutiveFailures.
	MaxConsecutiveFailures int
	// HealthyAfter is how long a child must stay up before the run of
	// failures behind it is forgiven. Zero means defaultHealthyAfter.
	//
	// It is a duration rather than "became ready" because readiness arrives
	// immediately for a child with no probe, and a child that starts and dies
	// a second later would otherwise clear its own record every time — and so
	// could never be reported as one that will not come up.
	HealthyAfter time.Duration
	// WorkingDir, when set, is the child's working directory. It matters for
	// more than tidiness: the Platform resolves several paths relative to it,
	// including the extension install directory (ADR 0081), so a child
	// started from the wrong directory finds none of its installed modules.
	WorkingDir string
	// StopGrace is how long this child gets between SIGTERM and SIGKILL.
	// Zero means defaultStopGrace.
	//
	// It is per child because the right answer differs by an order of
	// magnitude: the Shell serves static files and has nothing to finish,
	// while the Platform may be mid-transaction or draining a playback
	// session, and killing it early turns a planned shutdown into the kind
	// of unclean stop that costs a recovery on the next boot.
	StopGrace time.Duration
}

// defaultStopGrace is deliberately generous. Waiting too long for a process
// that has already finished costs nothing — the wait ends as soon as it exits.
const defaultStopGrace = 15 * time.Second

// defaultMaxConsecutiveFailures is when the Supervisor stops treating a child
// as one that is about to come up.
//
// It does not stop trying. A household box whose database was briefly away
// must heal itself rather than wait for somebody to notice, so retries
// continue at the capped backoff. What changes is that the failure is stated
// once and reported as a distinct condition, instead of scrolling past as an
// identical line every minute — a Supervisor that never says "I cannot start
// this" is one whose logs have to be read to learn it.
const defaultMaxConsecutiveFailures = 5

// defaultHealthyAfter is how long "it started and it is still here" takes to
// become evidence.
const defaultHealthyAfter = 30 * time.Second

// maxBackoff caps the wait between restarts. A minute is short enough that a
// database coming back is noticed promptly and long enough that a hopeless
// child is not a busy loop.
const maxBackoff = time.Minute

// Manager runs the children and keeps them running.
//
// Restart policy is exponential backoff capped at a minute, reset once a child
// has stayed up long enough to be considered healthy. The reset matters: a
// process that crashes every hour must not inherit the backoff earned by a
// crash loop last week and then take a minute to come back.
type Manager struct {
	mu       sync.Mutex
	children map[string]*child
	order    []string
	bootID   string
	// generationID names the Generation the children belong to, empty when
	// nothing is managing Generations. It is handed to every child (ADR 0129)
	// because a Platform cannot otherwise say which Generation it is — and that
	// comparison is what settles an upgrade request, since the process that
	// would have acknowledged one has just been replaced by it.
	generationID string
	// tel is where the lifecycle goes (ADR 0060). These are the facts a process
	// structurally cannot report about itself — that it started, what code it
	// exited with, that it has failed five times running — so the Supervisor is
	// the only thing in a position to write them down. Nil discards.
	tel *Telemetry
	// out and outMu are the children's console destination and the lock that
	// keeps one child's line from interleaving with another's mid-way.
	out   io.Writer
	outMu sync.Mutex
	// spool is where a finding is written for the Platform to adopt
	// (ADR 0119). Nil is a Manager nobody is collecting findings from.
	spool *Spool
	// capture, when set, records everything the children write as well as
	// passing it through — the evidence a failed activation would otherwise
	// destroy by reverting. Guarded by outMu, which every write already holds.
	capture *ring
}

type child struct {
	spec  ChildSpec
	state ChildState
	pid   int
	// starts counts every start including the first. Restarts are reported as
	// starts-1, because the first start is not a restart — a freshly booted
	// install reporting "1 restart" reads as a crash that never happened, and
	// this number is one an operator judges stability by.
	starts int
	// consecutiveFailures resets the moment a child stays up long enough to
	// count as healthy, so a process that crashes once an hour never inherits
	// the count earned by a crash loop last week.
	consecutiveFailures int
	// unrecoverable records that consecutiveFailures passed the child's
	// ceiling. Retries continue; this is what makes the condition sayable.
	unrecoverable bool
	// startedAt is when the current process was started, used to decide
	// whether it has been up long enough to be believed.
	startedAt time.Time
	lastErr   string
	cmd       *exec.Cmd
	// holds counts the operations that own this child. While any do, the
	// watchdog does not restart it — see Hold.
	holds int
	// restart carries a deliberate stop to the running process. Buffered by
	// one so a Restart that arrives between runs is not lost.
	restart chan struct{}
}

// NewManager builds a Manager. tel may be nil, which discards.
func NewManager(bootID string, tel *Telemetry) *Manager {
	return &Manager{
		children: map[string]*child{},
		bootID:   bootID,
		tel:      tel,
		out:      os.Stdout,
	}
}

// Add registers a child. It does not start it.
func (m *Manager) Add(spec ChildSpec) error {
	if spec.Name == "" {
		return errors.New("a child needs a name")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.children[spec.Name]; exists {
		return fmt.Errorf("child %q is already registered", spec.Name)
	}
	m.children[spec.Name] = &child{spec: spec, state: ChildStopped, restart: make(chan struct{}, 1)}
	m.order = append(m.order, spec.Name)
	return nil
}

// Run supervises every registered child until ctx is cancelled, then stops
// them **in registration order** and returns. It blocks.
//
// The order is the point, and it is deliberately *not* the conventional
// stop-dependents-first. That rule exists to drain traffic through the
// dependent before the thing it depends on goes away, and it does not apply
// here: clients reach the Platform through the front door directly, never
// through the Shell, which serves static files and has nothing in flight. So
// stopping the Shell first would drain nothing — it would only destroy the
// best interface still standing.
//
// Register children in the order they should be given up, which means most
// expendable first and the interface last. ADR 0005 has the Supervisor use the
// richest available presentation layer, and a shutdown is where that ladder is
// walked rather than skipped: the Platform goes, and the Shell — still up —
// renders its offline state; then the Shell goes and the holding page answers;
// then the front door closes. Taking the Shell first would jump straight to
// the holding page while a far better screen was still available.
//
// Each child is fully stopped before the next is asked to, so a child's stop
// grace is its own rather than a share of one global deadline. The cost is
// that shutdown is the sum of the children's stops rather than the longest of
// them, which is what buys the ordering.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	names := append([]string(nil), m.order...)
	m.mu.Unlock()

	// Each child gets its own cancellation so they can be stopped one at a
	// time. Deriving them from context.Background() rather than from ctx is
	// what makes that possible: a child derived from ctx would be cancelled
	// the instant ctx was, and every "ordered" stop would in fact be
	// simultaneous.
	stops := make([]context.CancelFunc, len(names))
	dones := make([]chan struct{}, len(names))

	for i, name := range names {
		childCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		stops[i], dones[i] = cancel, done

		go func(name string) {
			defer close(done)
			m.superviseOne(childCtx, name)
		}(name)
	}

	<-ctx.Done()

	for i := range names {
		m.tel.Info(componentChild, "stopping", String("child", names[i]))
		stops[i]()
		<-dones[i]
	}
}

// superviseOne is the restart loop for one child.
func (m *Manager) superviseOne(ctx context.Context, name string) {
	backoff := time.Second

	for ctx.Err() == nil {
		spec := m.specOf(name)

		if len(spec.Command) == 0 {
			// Nothing to run and nobody is going to give us anything: the
			// child is somebody else's, and the Supervisor fronts it and
			// reports on it without owning it. The dev stack's shape.
			if !spec.Managed {
				m.pollReadiness(ctx, name)
				return
			}
			// Owned, with no binary yet — a first boot before its Generation
			// has been fetched. Wait rather than return: the Activator sets a
			// command and signals a restart, and returning here is what left a
			// provisioned install with two children nothing would ever start.
			select {
			case <-ctx.Done():
				return
			case <-m.restartOf(name):
			}
			continue
		}

		started := time.Now()
		err := m.runOnce(ctx, name, spec)
		if ctx.Err() != nil {
			return
		}

		// A stop the Supervisor asked for is not a failure: no count, no
		// backoff, and straight back up. Counting it would have a Generation
		// activation look like a crashing Platform.
		if errors.Is(err, errRestartRequested) {
			backoff = time.Second
			m.waitWhileHeld(ctx, name)
			continue
		}

		healthy := time.Since(started) >= healthyAfter(spec)
		if healthy {
			backoff = time.Second
		}
		crossed := m.recordFailure(name, err, healthy, spec.MaxConsecutiveFailures)

		// The exit itself, always, because the shape of a crash loop is the
		// diagnosis and no single process instance can see it: the exit code,
		// how long it lasted and how many failures precede it are what
		// distinguish a database that was briefly away from a binary that
		// cannot run here at all.
		// Warn rather than error every time round: an exit here is always
		// unexpected — a stop the Supervisor asked for and a cancelled context
		// both returned above — but one exit is a process that fell over and
		// will be back, which is not the same claim as the ceiling below.
		m.tel.Warn(componentChild, "exited",
			String("child", name),
			Int("exit_code", exitCodeOf(err)),
			Duration("uptime", time.Since(started)),
			Int("consecutive_failures", m.failureCountOf(name)),
			Duration("backoff", backoff),
			Err(err))

		// Crossing the ceiling is said once, on the transition. Repeating it
		// every minute is how the one line that matters becomes the one nobody
		// reads — the exit record above is already written every time round.
		if crossed {
			m.tel.Error(componentChild, "is not coming up and is still being retried",
				String("child", name),
				Int("consecutive_failures", m.failureCountOf(name)),
				Duration("retry_interval", maxBackoff),
				Err(err))
			// And recorded, because a line said once is a line that scrolls
			// away (ADR 0119). On the transition rather than on every failure,
			// for the same reason the log is: the register folds repeats into
			// a count, but a spool line per minute is a file that grows
			// without bound on the one install that most needs it readable.
			m.spool.Record(FindingChildUnrecoverable, ContextChild, name, errText(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}

		// A child that died while an operation owned it stays down until that
		// operation lets go. The failure above is still recorded first, so the
		// health probe reports what happened rather than a pid that no longer
		// exists — the hold delays the *restart*, not the accounting.
		m.waitWhileHeld(ctx, name)
	}
}

// runOnce starts the child and waits for it, probing readiness meanwhile.
func (m *Manager) runOnce(ctx context.Context, name string, spec ChildSpec) error {
	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.WorkingDir
	// Attributed, because three processes share one terminal. Both streams go
	// to the same destination through the same lock, so a child's stderr line
	// cannot land inside its own stdout line.
	stdout := newPrefixWriter(sink{m}, &m.outMu, name)
	stderr := newPrefixWriter(sink{m}, &m.outMu, name)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The boot id reaches every child, which is what stitches the three
	// processes' records into one timeline (ADR 0060).
	env := append(os.Environ(), "MOSAIC_BOOT_ID="+m.bootID)
	if generation := m.generationOf(); generation != "" {
		env = append(env, "MOSAIC_GENERATION_ID="+generation)
	}
	cmd.Env = append(env, spec.Env...)
	// Its own process group, so stopping the Supervisor can stop a child's
	// whole tree rather than orphaning grandchildren.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}

	m.mu.Lock()
	c := m.children[name]
	c.cmd, c.pid, c.state, c.lastErr = cmd, cmd.Process.Pid, ChildStarting, ""
	c.startedAt = time.Now()
	c.starts++
	starts := c.starts
	m.mu.Unlock()
	m.tel.Info(componentChild, "started",
		String("child", name),
		Int("pid", cmd.Process.Pid),
		Int("start", starts),
		String("command", spec.Command[0]))

	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	go m.pollReadiness(probeCtx, name)

	// Stop the child when the Supervisor is asked to stop.
	//
	// `done` is buffered and carries the exit error; `exited` is closed
	// alongside it purely as a broadcast, so stop can wait for the process
	// without consuming the value runOnce still needs to return.
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(exited)
	}()

	// A crashing process's last line often has no trailing newline. Flushing
	// on the way out is what keeps it from being swallowed — which would lose
	// exactly the line that says why the child died.
	defer func() {
		stdout.Flush()
		stderr.Flush()
	}()

	m.mu.Lock()
	restart := c.restart
	m.mu.Unlock()

	select {
	case err := <-done:
		return err
	case <-restart:
		// Asked for, so it is stopped the same polite way a shutdown stops it
		// and reported distinctly, because a planned stop must not count
		// against the child.
		m.stop(cmd, spec.StopGrace, exited)
		<-done
		return errRestartRequested
	case <-ctx.Done():
		m.stop(cmd, spec.StopGrace, exited)
		<-done
		return ctx.Err()
	}
}

// stop asks politely, then insists. A Platform mid-transaction deserves the
// chance to finish it; a Platform that ignores SIGTERM must not be able to
// block an activation forever.
//
// The signal goes to the process *group* (the negative pid), which is why
// runOnce sets Setpgid: a child that spawned its own children — an extension
// module's process, an ffmpeg — would otherwise leave them orphaned and still
// holding the port the replacement is about to want.
func (m *Manager) stop(cmd *exec.Cmd, grace time.Duration, exited <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	if grace <= 0 {
		grace = defaultStopGrace
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(grace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	}
}

// pollReadiness moves a child to ready once its probe answers, and back to
// starting when it stops answering.
func (m *Manager) pollReadiness(ctx context.Context, name string) {
	spec := m.specOf(name)
	if spec.Readiness == nil && spec.Serving == nil {
		// No probe: running is the strongest claim available.
		m.setState(name, ChildReady)
		return
	}
	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()
	for {
		if m.ready(ctx, spec) {
			m.setState(name, ChildReady)
			// Ready *and* up for long enough is what lets a child stop being
			// reported as one that will not come up. Readiness alone is not
			// evidence: a child with no probe is ready the instant it starts,
			// so clearing on that would let one that dies a second later
			// forgive itself on every attempt.
			m.clearFailuresIfSettled(name, healthyAfter(spec))
		} else {
			m.setState(name, ChildStarting)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ready asks both questions, because they fail independently: a component can
// declare itself healthy while the listener a client arrives at is unbound,
// and a listener can accept while the component behind it has no database.
// Whichever is configured must pass.
func (m *Manager) ready(ctx context.Context, spec ChildSpec) bool {
	if spec.Readiness != nil {
		status, answered := spec.Readiness.ask(ctx)
		// The child's own assessment, so success is the question.
		if !answered || status < 200 || status >= 400 {
			return false
		}
	}
	if spec.Serving != nil {
		// Only that something answered. See ChildSpec.Serving: the honest
		// signal from a client-facing RPC path is that the listener is bound
		// and the mux routed, which a 405 from a POST-only method proves
		// without invoking it.
		if _, answered := spec.Serving.ask(ctx); !answered {
			return false
		}
	}
	return true
}

func (p *Probe) ask(ctx context.Context) (status int, answered bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return 0, false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, true
}

// restartOf is the channel a restart request arrives on. Read under the lock
// because the child map is, though the channel itself never changes.
func (m *Manager) restartOf(name string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.children[name].restart
}

func (m *Manager) specOf(name string) ChildSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.children[name].spec
}

// setState moves a child and records the move when it is one.
//
// **The transition is the fact, not the state**, which is why this is not a
// plain setter: readiness is polled every couple of seconds, so recording the
// answer would be a line every tick and recording nothing would make a child
// that flaps ready → starting → ready completely invisible. Only the edges are
// written, and they are what a crash loop or a flapping probe looks like in a
// file somebody reads afterwards.
//
// For the Platform child this edge is also the **handover**: the front door
// answers the client surface itself while the Platform is not ready and steps
// out of the way when it is (ADR 0123), and it reads the same state this sets.
// There is deliberately no second record saying so — one fact, one statement.
func (m *Manager) setState(name string, state ChildState) {
	m.mu.Lock()
	c, ok := m.children[name]
	if !ok || c.state == state {
		m.mu.Unlock()
		return
	}
	from := c.state
	c.state = state
	m.mu.Unlock()

	m.tel.Info(componentChild, "state changed",
		String("child", name),
		String("from", string(from)),
		String("to", string(state)))
}

// recordFailure marks a child failed and returns whether this failure is the
// one that crossed its ceiling — so the caller can say so exactly once.
//
// `healthy` reports that the child had stayed up long enough to count before
// it died, which clears the run of failures: a process that crashes once an
// hour is not the same condition as one that has never started.
func (m *Manager) recordFailure(name string, err error, healthy bool, ceiling int) (crossed bool) {
	if ceiling <= 0 {
		ceiling = defaultMaxConsecutiveFailures
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok {
		return false
	}
	c.state = ChildFailed
	c.pid = 0
	if err != nil {
		c.lastErr = err.Error()
	}
	if healthy {
		c.consecutiveFailures, c.unrecoverable = 1, false
		return false
	}
	c.consecutiveFailures++
	if c.consecutiveFailures >= ceiling && !c.unrecoverable {
		c.unrecoverable = true
		return true
	}
	return false
}

// clearFailuresIfSettled forgives a child's run of failures once it has been
// up for `settled`, which is what makes "unrecoverable" a condition a child
// can leave rather than a verdict it carries until the Supervisor restarts.
func (m *Manager) clearFailuresIfSettled(name string, settled time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.children[name]
	if !ok || c.startedAt.IsZero() || time.Since(c.startedAt) < settled {
		return
	}
	c.consecutiveFailures, c.unrecoverable = 0, false
}

// healthyAfter resolves a child's settling time.
func healthyAfter(spec ChildSpec) time.Duration {
	if spec.HealthyAfter > 0 {
		return spec.HealthyAfter
	}
	return defaultHealthyAfter
}

func (m *Manager) failureCountOf(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.children[name]; ok {
		return c.consecutiveFailures
	}
	return 0
}

func (m *Manager) unrecoverableOf(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.children[name]; ok {
		return c.unrecoverable
	}
	return false
}

// Snapshot reports every child, in registration order so the output is stable.
func (m *Manager) Snapshot() Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChildSnapshot, 0, len(m.order))
	for _, name := range m.order {
		c := m.children[name]
		restarts := c.starts - 1
		if restarts < 0 {
			restarts = 0
		}
		out = append(out, ChildSnapshot{
			Name:                name,
			State:               c.state,
			PID:                 c.pid,
			Restarts:            restarts,
			ConsecutiveFailures: c.consecutiveFailures,
			Unrecoverable:       c.unrecoverable,
			LastErr:             c.lastErr,
		})
	}
	return Health{Children: out}
}

// SetGenerationID records which Generation the children belong to, so the next
// one started is told. Set at boot from the active pointer and again by an
// activation, which is the only other moment it changes.
func (m *Manager) SetGenerationID(version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generationID = version
}

func (m *Manager) generationOf() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generationID
}

// SetSpool supplies where findings are recorded. Wired after construction
// because the state directory is resolved with the Generations, which are
// opened after the Manager exists.
func (m *Manager) SetSpool(s *Spool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spool = s
}

// errText is an error's message, or a stand-in when there is none. A child that
// exited zero when it should not have has no error to quote, and an empty
// detail on the screen reads as a finding that lost its reason.
func errText(err error) string {
	if err == nil {
		return "the process exited without an error"
	}
	return err.Error()
}

// exitCodeOf reports what a child exited with, for the record (ADR 0060).
//
// Three outcomes and they are genuinely different: `0` is a process that exited
// cleanly when it was not supposed to exit at all — the shape a Platform with a
// missing DSN has, which is why it is worth distinguishing rather than folding
// into "it died". A positive code is the process's own verdict. `-1` is either a
// signal or a start that never happened, and Go reports both the same way, so
// the error text beside this is what tells them apart.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}
