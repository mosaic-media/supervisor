// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestEachLineIsAttributedToItsChild(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	w := newPrefixWriter(&out, &mu, "platform")

	if _, err := w.Write([]byte("migrating\nready\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := "[platform] migrating\n[platform] ready\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

// A write does not arrive one line at a time. The Platform's console sink
// writes whole records, but a pipe can split anywhere, and a fragment must
// wait for the newline that finishes it rather than being emitted as a line.
func TestAPartialLineWaitsForItsNewline(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	w := newPrefixWriter(&out, &mu, "shell")

	_, _ = w.Write([]byte("listening on "))
	if out.Len() != 0 {
		t.Fatalf("emitted an unfinished line: %q", out.String())
	}
	_, _ = w.Write([]byte(":8090\n"))

	if got, want := out.String(), "[shell] listening on :8090\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A crashing process's last line often has no trailing newline, and that is
// exactly the line saying why it died.
func TestFlushEmitsTheFinalUnterminatedLine(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	w := newPrefixWriter(&out, &mu, "platform")

	_, _ = w.Write([]byte("panic: nil map"))
	w.Flush()

	if got, want := out.String(), "[platform] panic: nil map\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A second flush must not repeat it.
	w.Flush()
	if got := strings.Count(out.String(), "panic"); got != 1 {
		t.Errorf("flush repeated the line %d times", got)
	}
}

// A child that never writes a newline must not be able to grow the
// Supervisor's memory until something kills it.
func TestAnEndlessLineIsBoundedRatherThanBuffered(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	w := newPrefixWriter(&out, &mu, "noisy")

	_, _ = w.Write(bytes.Repeat([]byte("x"), maxBufferedLine+10))

	if out.Len() == 0 {
		t.Fatal("nothing was emitted; the fragment is still being buffered")
	}
	if len(w.buf) > maxBufferedLine {
		t.Errorf("still holding %d bytes, past the %d bound", len(w.buf), maxBufferedLine)
	}
}

// Two children share one terminal, and a line must not land inside another's.
func TestConcurrentChildrenDoNotInterleaveWithinALine(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	platform := newPrefixWriter(&out, &mu, "platform")
	shell := newPrefixWriter(&out, &mu, "shell")

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = platform.Write([]byte("aaaaaaaaaa\n"))
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = shell.Write([]byte("bbbbbbbbbb\n"))
		}()
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		switch line {
		case "[platform] aaaaaaaaaa", "[shell] bbbbbbbbbb":
		default:
			t.Fatalf("interleaved line: %q", line)
		}
	}
}

// A child's write must not fail because the Supervisor's stdout did: a short
// write would have the child's logging package retry or error on something it
// cannot fix.
func TestAWriteAlwaysReportsFullConsumption(t *testing.T) {
	var mu sync.Mutex
	w := newPrefixWriter(failingWriter{}, &mu, "platform")

	p := []byte("a line\n")
	n, err := w.Write(p)
	if n != len(p) || err != nil {
		t.Errorf("got (%d, %v), want (%d, nil)", n, err, len(p))
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

var errWrite = &writeError{}

type writeError struct{}

func (*writeError) Error() string { return "stdout is gone" }
