// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The record format is duplicated from the Platform's on purpose, and this is
// the guard that makes the duplication bounded (see telemetry.go).
//
// **It names every key rather than checking the ones a test happens to use.**
// The hazard is not a file that cannot be parsed — the Platform reads JSON Lines
// and ignores what it does not know — it is a key quietly renamed, which the
// reader would report as a missing field rather than as an error. A test that
// asserted only on `message` would pass through exactly that.
func TestTheRecordFormatIsThePlatforms(t *testing.T) {
	tel, path := telemetryInDir(t)
	tel.Info("child", "started", String("child", "platform"), Int("pid", 41))
	if err := tel.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(firstLine(t, path), &got); err != nil {
		t.Fatalf("the record is not JSON: %v", err)
	}

	want := []string{"boot", "component", "fields", "instance", "level", "message", "service", "time"}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("record keys are %v, want %v", keys, want)
	}

	// The Platform writes `trace`, `span` and `module` and the Supervisor has
	// none of the three. Absent rather than empty: an empty `trace` would be a
	// claim to run traces, which ADR 0060 says it does not.
	for _, absent := range []string{"trace", "span", "module"} {
		if _, present := got[absent]; present {
			t.Fatalf("the Supervisor has no %q and must not write the key", absent)
		}
	}

	if got["service"] != serviceName {
		t.Fatalf("service is %v, want %q", got["service"], serviceName)
	}
	if got["level"] != "info" {
		t.Fatalf("level is %v, want info", got["level"])
	}
	if got["boot"] != "boot-1" {
		t.Fatalf("boot is %v, want boot-1", got["boot"])
	}
	if _, err := time.Parse(time.RFC3339Nano, got["time"].(string)); err != nil {
		t.Fatalf("time is not RFC3339: %v", err)
	}
	fields, _ := got["fields"].(map[string]any)
	if fields["child"] != "platform" || fields["pid"] != float64(41) {
		t.Fatalf("fields are %v", fields)
	}
}

// A Field nobody classified is a Field nobody thought about, and it is dropped
// rather than written. This is the property the unexported class buys: the only
// way to produce a safe Field is a constructor, so a struct literal written at a
// call site cannot leak by omission.
func TestAnUnclassifiedFieldFailsClosed(t *testing.T) {
	tel, path := telemetryInDir(t)
	tel.Info("child", "started",
		Field{Key: "hand_written", Value: "postgres://mosaic:hunter2@db/mosaic"},
		Secret("token", "s3cr3t"),
		Sensitive("who", "ada"),
		String("safe", "postgres"))
	_ = tel.Close()

	line := firstLine(t, path)
	for _, leaked := range []string{"hunter2", "s3cr3t", "ada"} {
		if bytes.Contains(line, []byte(leaked)) {
			t.Fatalf("%q reached the file: %s", leaked, line)
		}
	}
	if !bytes.Contains(line, []byte(`"safe":"postgres"`)) {
		t.Fatalf("a classified-safe field was dropped: %s", line)
	}
	if bytes.Count(line, []byte(redactedPlaceholder)) != 3 {
		t.Fatalf("want three redacted values, got: %s", line)
	}
}

// The console carries the same record. Two destinations rather than two logging
// paths: a person at a terminal and a file being read afterwards see the same
// facts, so neither is a subset of the other.
func TestTheConsoleCarriesTheSameRecord(t *testing.T) {
	var console bytes.Buffer
	dir := t.TempDir()
	tel := OpenTelemetry(Config{StateDir: dir, BootID: "boot-1"}, &console)
	tel.Warn("child", "exited", Int("exit_code", 1), String("child", "platform"))
	tel.Printf("listening on %s", ":8443")
	_ = tel.Close()

	lines := strings.Split(strings.TrimSpace(console.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two console lines, got %d: %q", len(lines), console.String())
	}
	// Sorted fields, the level word only above info, and the service prefix
	// three processes on one terminal need.
	if lines[0] != "mosaic-supervisor: WARN child: exited child=platform exit_code=1" {
		t.Fatalf("console line is %q", lines[0])
	}
	if lines[1] != "mosaic-supervisor: listening on :8443" {
		t.Fatalf("console line is %q", lines[1])
	}
}

func TestRecordsBelowTheLevelAreDropped(t *testing.T) {
	var console bytes.Buffer
	dir := t.TempDir()
	tel := OpenTelemetry(Config{StateDir: dir, BootID: "boot-1", LogLevel: LevelWarn}, &console)
	tel.Info("child", "started")
	tel.Error("child", "exited")
	_ = tel.Close()

	if strings.Contains(console.String(), "started") {
		t.Fatalf("an info record survived a warn floor: %q", console.String())
	}
	if !strings.Contains(console.String(), "exited") {
		t.Fatalf("an error record was dropped: %q", console.String())
	}
}

// Rotation is the only retention there is (ADR 0060), and one previous file is
// kept so a crash loop cannot erase the boot that preceded it.
func TestTheFileRotatesAndKeepsOnePrevious(t *testing.T) {
	dir := t.TempDir()
	tel := OpenTelemetry(Config{StateDir: dir, BootID: "boot-1"}, nil)
	live := tel.Path()

	// Enough records to pass the cap several times over.
	filler := strings.Repeat("x", 4096)
	for i := 0; i < (maxLogBytes/4096)*2+16; i++ {
		tel.Info("child", "exited", String("detail", filler))
	}
	_ = tel.Close()

	entries, err := os.ReadDir(filepath.Join(dir, telemetryDirName))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want the live file and one previous, got %d", len(entries))
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.Size() > maxLogBytes {
			t.Fatalf("%s is %d bytes, past the %d cap", e.Name(), info.Size(), maxLogBytes)
		}
	}
	if _, err := os.Stat(live + ".1"); err != nil {
		t.Fatalf("the previous file is not %s.1: %v", live, err)
	}
}

// The file's order must be its timestamps' order. Records come from every
// child's goroutine at once, and a reader merging this file with the Platform's
// sorts on time — so a file that disagreed with itself would make the merge look
// wrong for a reason that was not the merge's. Caught in a real run, where a
// shutdown wrote three lines out of order.
func TestRecordsAreInTimestampOrderUnderConcurrency(t *testing.T) {
	tel, path := telemetryInDir(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				tel.Info(componentChild, "started", Int("writer", n))
			}
		}(i)
	}
	wg.Wait()
	_ = tel.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var previous time.Time
	for i, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var got struct {
			Time time.Time `json:"time"`
		}
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if got.Time.Before(previous) {
			t.Fatalf("line %d is stamped %s, before the line above it (%s)", i, got.Time, previous)
		}
		previous = got.Time
	}
}

// A restart must not reset the rotation budget, or a Supervisor that restarts
// often would never rotate and the cap would be a number nothing enforced.
func TestReopeningResumesTheFilesSize(t *testing.T) {
	dir := t.TempDir()
	first := OpenTelemetry(Config{StateDir: dir, BootID: "boot-1"}, nil)
	first.Info("child", "started")
	_ = first.Close()

	second := OpenTelemetry(Config{StateDir: dir, BootID: "boot-2"}, nil)
	defer second.Close()
	if second.written == 0 {
		t.Fatal("the second open started its budget from zero")
	}
	second.Info("child", "started")
	data, err := os.ReadFile(second.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if lines := bytes.Count(data, []byte("\n")); lines != 2 {
		t.Fatalf("want both boots' records in the file, got %d lines", lines)
	}
}

// A Supervisor that cannot write its log still boots and still speaks. The
// failure to open is itself the first thing it says.
func TestAnUnwritableStateDirectoryIsConsoleOnly(t *testing.T) {
	var console bytes.Buffer
	// A file where the directory should be: MkdirAll cannot create through it.
	blocked := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tel := OpenTelemetry(Config{StateDir: blocked, BootID: "boot-1"}, &console)
	defer tel.Close()
	if tel.Path() != "" {
		t.Fatalf("Path is %q, want empty", tel.Path())
	}
	if !strings.Contains(console.String(), "console-only") {
		t.Fatalf("the failure was not reported: %q", console.String())
	}

	tel.Info("child", "started")
	if !strings.Contains(console.String(), "started") {
		t.Fatal("records stopped when the file could not be opened")
	}
}

// Nil is a working Telemetry that discards. Components are handed one before
// there is anywhere to write, and a logging call must never be the thing that
// takes the process down.
func TestANilTelemetryDiscards(t *testing.T) {
	var tel *Telemetry
	tel.Info("child", "started", String("child", "platform"))
	tel.Printf("anything")
	if tel.Path() != "" {
		t.Fatal("a nil Telemetry claimed a path")
	}
	if err := tel.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestExitCodeOf(t *testing.T) {
	// A process that exited cleanly when it was not supposed to exit at all —
	// the shape a Platform with a missing DSN has — must be distinguishable
	// from one that was killed or never started.
	if got := exitCodeOf(nil); got != 0 {
		t.Fatalf("a clean exit is %d, want 0", got)
	}
	if got := exitCodeOf(errors.New("starting platform: no such file")); got != -1 {
		t.Fatalf("a start that never happened is %d, want -1", got)
	}
	// A real *exec.ExitError, because that is the only thing runOnce ever hands
	// this and a hand-built stand-in would not prove errors.As reaches it.
	err := exec.Command("sh", "-c", "exit 3").Run()
	if got := exitCodeOf(fmt.Errorf("child exited: %w", err)); got != 3 {
		t.Fatalf("exit code is %d, want 3", got)
	}
}

func telemetryInDir(t *testing.T) (*Telemetry, string) {
	t.Helper()
	tel := OpenTelemetry(Config{StateDir: t.TempDir(), BootID: "boot-1"}, nil)
	if tel.Path() == "" {
		t.Fatal("no file was opened")
	}
	return tel, tel.Path()
}

func firstLine(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	line, _, found := bytes.Cut(data, []byte("\n"))
	if !found {
		t.Fatalf("no complete line in %s", path)
	}
	return line
}
