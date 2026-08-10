// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"bytes"
	"io"
	"sync"
)

// maxBufferedLine bounds the partial line held while waiting for a newline.
// A child that writes a gigabyte without one must not be able to grow the
// Supervisor's memory until it is killed: past this, the fragment is flushed
// as if it had ended. The prefix then repeats mid-line, which is ugly and is
// the correct trade — the Supervisor staying up matters more than a tidy line.
const maxBufferedLine = 64 << 10

// prefixWriter attributes a child's console output to the child.
//
// Three processes write to one terminal, and without this their lines
// interleave with nothing to say which said what — the Platform, the Shell and
// the Supervisor all logging at once, indistinguishable. It is safe precisely
// because this is the *console* stream: the Platform's structured records go to
// its file sink (supervisor#5), and its console sink already emits human-readable
// text, so a prefix corrupts no machine-read format.
//
// Writes are line-buffered and the whole line is emitted under one lock, so a
// line never interleaves with another child's mid-way.
type prefixWriter struct {
	out    io.Writer
	mu     *sync.Mutex // shared with every writer on the same destination
	prefix []byte
	buf    []byte
}

func newPrefixWriter(out io.Writer, mu *sync.Mutex, name string) *prefixWriter {
	return &prefixWriter{out: out, mu: mu, prefix: []byte("[" + name + "] ")}
}

// Write consumes p, emitting each complete line with the prefix and holding
// any trailing fragment until the newline that finishes it arrives.
//
// It always reports len(p) consumed: a child's write must not fail because the
// Supervisor's own stdout did, and reporting a short write would have the
// child's logging package retry or error on something it cannot fix.
func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}

	// An unterminated line that has grown past the bound is emitted anyway;
	// see maxBufferedLine.
	if len(w.buf) >= maxBufferedLine {
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}

	// Reclaim the backing array once it has been drained, so a single huge
	// burst does not leave this writer holding that capacity for the life of
	// the process.
	if len(w.buf) == 0 && cap(w.buf) > maxBufferedLine {
		w.buf = nil
	}
	return len(p), nil
}

// emit writes one prefixed line. Errors are dropped deliberately: the only
// destination is the Supervisor's own stdout, and there is nowhere to report a
// failure to write to it that would not be the same broken stream.
func (w *prefixWriter) emit(line []byte) {
	out := make([]byte, 0, len(w.prefix)+len(line)+1)
	out = append(out, w.prefix...)
	out = append(out, line...)
	out = append(out, '\n')
	_, _ = w.out.Write(out)
}

// Flush emits any held fragment. It is called when a child exits, so a last
// line written without a trailing newline — which is exactly what a crashing
// process tends to produce — is not swallowed.
func (w *prefixWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}
}
