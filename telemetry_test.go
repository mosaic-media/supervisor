// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The file is ordinary OpenTelemetry, and that is the whole of ADR 0128 as it
// applies here.
//
// **The test this replaced pinned a JSON key set**, because the record format
// was hand-written and duplicated from the Platform's, and a key quietly renamed
// on one side was the hazard nothing else would catch. There is no longer a
// Mosaic-authored format to pin: the shape is OTLP's, both processes will read
// it with the same tooling, and what is worth asserting instead is that the
// facts a reader needs actually arrive in it.
func TestTheFileIsOrdinaryOpenTelemetry(t *testing.T) {
	tel, path := telemetryInDir(t)
	tel.Info(componentChild, "started", String("child", "platform"), Int("pid", 41))
	if err := tel.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var record map[string]any
	if err := json.Unmarshal(firstLine(t, path), &record); err != nil {
		t.Fatalf("the record is not JSON: %v", err)
	}

	if got := rendered(record["Body"]); got != "started" {
		t.Errorf("body is %q, want the message", got)
	}
	if record["SeverityText"] != "info" {
		t.Errorf("severity text is %v, want info", record["SeverityText"])
	}
	if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(record["Timestamp"])); err != nil {
		t.Errorf("timestamp is not RFC3339: %v", err)
	}

	attrs := keyValues(t, record["Attributes"])
	if attrs["child"] != "platform" || attrs["pid"] != "41" {
		t.Errorf("attributes are %v", attrs)
	}
	if attrs[componentAttribute] != componentChild {
		t.Errorf("component is %q, want %q", attrs[componentAttribute], componentChild)
	}

	// The resource is what makes two processes' records tell themselves apart
	// in a merged read, and the boot id is what stitches them into one timeline
	// (ADR 0060).
	res := keyValues(t, record["Resource"])
	if res["service.name"] != serviceName {
		t.Errorf("service.name is %q, want %q", res["service.name"], serviceName)
	}
	if res[bootAttribute] != "boot-1" {
		t.Errorf("%s is %q, want boot-1", bootAttribute, res[bootAttribute])
	}
}

// A Field nobody classified is a Field nobody thought about, and it is dropped
// rather than written. OpenTelemetry has no notion of classification — an
// attribute carries its value, full stop — so the conversion is the last place
// the rule can be applied, and the unexported class is what makes a struct
// literal written at a call site fail closed rather than leak.
func TestAnUnclassifiedFieldFailsClosed(t *testing.T) {
	tel, path := telemetryInDir(t)
	tel.Info(componentChild, "started",
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
	var record map[string]any
	if err := json.Unmarshal(line, &record); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	attrs := keyValues(t, record["Attributes"])
	if attrs["safe"] != "postgres" {
		t.Fatalf("a classified-safe field was dropped: %v", attrs)
	}
	for _, key := range []string{"hand_written", "token", "who"} {
		if attrs[key] != redactedPlaceholder {
			t.Errorf("%s is %q, want %q — a redacted field must still show it was there",
				key, attrs[key], redactedPlaceholder)
		}
	}
}

// The console is an exporter beside the file's, not a second logging path, so a
// person at a terminal and a file read afterwards see the same records.
func TestTheConsoleCarriesTheSameRecord(t *testing.T) {
	var console bytes.Buffer
	tel := OpenTelemetry(Config{StateDir: t.TempDir(), BootID: "boot-1"}, &console)
	tel.Warn(componentChild, "exited", Int("exit_code", 1), String("child", "platform"))
	tel.Printf("listening on %s", ":8443")
	_ = tel.Close()

	lines := strings.Split(strings.TrimSpace(console.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two console lines, got %d: %q", len(lines), console.String())
	}
	// Sorted fields, the level word only above info, the component lifted into
	// the prefix, and the service name three processes on one terminal need.
	if lines[0] != "mosaic-supervisor: WARN child: exited child=platform exit_code=1" {
		t.Fatalf("console line is %q", lines[0])
	}
	if lines[1] != "mosaic-supervisor: listening on :8443" {
		t.Fatalf("console line is %q", lines[1])
	}
}

func TestRecordsBelowTheLevelAreDropped(t *testing.T) {
	var console bytes.Buffer
	tel := OpenTelemetry(Config{StateDir: t.TempDir(), BootID: "boot-1", LogLevel: LevelWarn}, &console)
	tel.Info(componentChild, "started")
	tel.Error(componentChild, "exited")
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

	filler := strings.Repeat("x", 4096)
	for i := 0; i < (maxLogBytes/4096)*2+16; i++ {
		tel.Info(componentChild, "exited", String("detail", filler))
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
		// One record may cross the cap, since rotation happens before a write
		// rather than mid-way through one.
		if info.Size() > maxLogBytes+int64(len(filler))*2 {
			t.Fatalf("%s is %d bytes, well past the %d cap", e.Name(), info.Size(), maxLogBytes)
		}
	}
	if _, err := os.Stat(live + ".1"); err != nil {
		t.Fatalf("the previous file is not %s.1: %v", live, err)
	}
}

// A restart must not reset the rotation budget, or a Supervisor that restarts
// often would never rotate and the cap would be a number nothing enforced.
func TestReopeningResumesTheFilesSize(t *testing.T) {
	dir := t.TempDir()
	first := OpenTelemetry(Config{StateDir: dir, BootID: "boot-1"}, nil)
	first.Info(componentChild, "started")
	path := first.Path()
	_ = first.Close()

	before := sizeOf(t, path)
	if before == 0 {
		t.Fatal("the first boot wrote nothing")
	}

	second := OpenTelemetry(Config{StateDir: dir, BootID: "boot-2"}, nil)
	second.Info(componentChild, "started")
	_ = second.Close()

	if after := sizeOf(t, path); after <= before {
		t.Fatalf("the file is %d bytes after the second boot, was %d — it was truncated", after, before)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Both boots' records, distinguishable by the resource attribute that
	// stitches a boot's three processes together.
	for _, boot := range []string{"boot-1", "boot-2"} {
		if !bytes.Contains(data, []byte(boot)) {
			t.Errorf("%s is not in the file", boot)
		}
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

	tel.Info(componentChild, "started")
	if !strings.Contains(console.String(), "started") {
		t.Fatal("records stopped when the file could not be opened")
	}
}

// Nil is a working Telemetry that discards. Components are handed one before
// there is anywhere to write, and a logging call must never be the thing that
// takes the process down.
func TestANilTelemetryDiscards(t *testing.T) {
	var tel *Telemetry
	tel.Info(componentChild, "started", String("child", "platform"))
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

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return info.Size()
}

// keyValues flattens the exporter's attribute list into key → rendered value.
func keyValues(t *testing.T, raw any) map[string]string {
	t.Helper()
	out := map[string]string{}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("attributes are %T, want a list", raw)
	}
	for _, entry := range list {
		kv, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("attribute is %T, want an object", entry)
		}
		out[fmt.Sprint(kv["Key"])] = rendered(kv["Value"])
	}
	return out
}

// rendered pulls the text out of the exporter's value envelope, which carries a
// type tag beside the value.
func rendered(raw any) string {
	value, ok := raw.(map[string]any)
	if !ok {
		return fmt.Sprint(raw)
	}
	if v, present := value["Value"]; present {
		return fmt.Sprint(v)
	}
	return fmt.Sprint(raw)
}

// A collector that is down must cost records in the collector and none on disk.
// An install whose observability backend went away must not also lose the local
// account of why its Platform will not start (ADR 0060, ADR 0128).
func TestAnUnreachableCollectorDoesNotCostTheFile(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	// A port nothing is listening on.
	tel := OpenTelemetry(Config{
		StateDir:     dir,
		BootID:       "boot-1",
		OTLPEndpoint: "http://127.0.0.1:1",
	}, &console)

	tel.Error(componentChild, "is not coming up and is still being retried",
		String("child", "platform"))
	path := tel.Path()
	_ = tel.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(data, []byte("is not coming up")) {
		t.Fatal("the record did not reach the file while the collector was unreachable")
	}
	if !strings.Contains(console.String(), "is not coming up") {
		t.Fatal("the record did not reach the console either")
	}
}

// And a collector that is up receives them, so the wiring is real rather than
// merely configured.
func TestAReachableCollectorReceivesRecords(t *testing.T) {
	received := make(chan string, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	tel := OpenTelemetry(Config{
		StateDir:     t.TempDir(),
		BootID:       "boot-1",
		OTLPEndpoint: collector.URL,
	}, nil)
	tel.Info(componentChild, "started", String("child", "platform"))
	// Shutdown flushes the batch, which is why this needs no sleep.
	if err := tel.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case path := <-received:
		// The OTLP/HTTP logs path, so this is the protocol rather than a POST
		// that happened to arrive somewhere.
		if path != "/v1/logs" {
			t.Fatalf("the collector was called at %q, want /v1/logs", path)
		}
	default:
		t.Fatal("nothing reached the collector")
	}
}
