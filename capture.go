// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

// Keeping the evidence of a failed activation.
//
// **A revert destroys the reason it was needed.** The new Generation's Platform
// said something on its way down — a migration it would not run, a config it
// would not parse — and then the Supervisor puts the old one back, which starts
// cleanly and writes its own output over the top. Home Assistant copies the
// previous log aside before reverting for exactly this reason, and it is the
// detail worth copying outright: without it the only artefact of a failed
// upgrade is "it rolled back", which is the one fact that needs no explaining.
//
// The capture is a **ring** rather than a growing buffer. A child that fails by
// logging furiously is the ordinary case, and the last few hundred kilobytes
// are where the reason is; keeping everything would let a failing child fill
// memory in the process whose job is to stay up.

// captureLimit is how much of a failing activation's console output is kept.
// 256KiB is thousands of lines — far more than the tail anybody reads — and
// small enough to be irrelevant to a process that must not grow.
const captureLimit = 256 << 10

// ring keeps the most recent bytes written to it.
//
// It grows to twice the limit and then trims back, rather than trimming on
// every write once full. A child that fails by logging furiously is the
// ordinary case here, and trimming per write would make each one an O(limit)
// copy — amortising it costs one extra buffer's worth of memory and removes a
// cost that scales with how badly the child is behaving.
type ring struct {
	buf   []byte
	limit int
}

func newRing(limit int) *ring {
	if limit <= 0 {
		limit = captureLimit
	}
	return &ring{limit: limit}
}

// Write never fails and never blocks: it is on the path a child's console
// output takes, and the Supervisor's bookkeeping must not be able to stall a
// child's writes.
func (r *ring) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	if len(r.buf) > 2*r.limit {
		r.buf = append(r.buf[:0], r.buf[len(r.buf)-r.limit:]...)
	}
	return len(p), nil
}

// Bytes returns what is held, oldest first, trimmed to the limit.
func (r *ring) Bytes() []byte {
	b := r.buf
	if len(b) > r.limit {
		b = b[len(b)-r.limit:]
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// sink is the Manager's console destination, consulted per write so a capture
// attached after a child started still sees its output.
//
// It exists because prefixWriter takes its destination once, at construction,
// and children are constructed long before an activation decides to record
// them. Every write already happens under the Manager's output lock, so the
// capture needs no lock of its own.
type sink struct{ m *Manager }

func (s sink) Write(p []byte) (int, error) {
	if s.m.capture != nil {
		_, _ = s.m.capture.Write(p)
	}
	return s.m.out.Write(p)
}

// Capture starts recording every child's console output and returns a function
// that stops recording and yields what was seen.
//
// Only one capture at a time, which is all an activation needs: activations are
// serialised by the operation hold, so a second concurrent one is a bug rather
// than a case to support. A second call replaces the first rather than failing,
// because losing a recording is a worse outcome than losing the older of two.
func (m *Manager) Capture() (stop func() []byte) {
	m.outMu.Lock()
	r := newRing(captureLimit)
	m.capture = r
	m.outMu.Unlock()

	return func() []byte {
		m.outMu.Lock()
		defer m.outMu.Unlock()
		if m.capture == r {
			m.capture = nil
		}
		return r.Bytes()
	}
}
