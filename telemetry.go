// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Supervisor's own telemetry (ADR 0060).
//
// **There is a whole class of failure where the process that would normally
// report is the process that is broken**, and it is the class an operator meets
// first: a migration that will not run, a database that is not there, a
// Generation that starts and immediately dies. The Platform's structured
// records, its Postgres store and its expert-mode viewer are all inside the
// thing that failed. The Supervisor is the process that survives all of it, and
// it is also the process that *caused* the transition — it selected the
// Generation, started it and watched it die.
//
// Until now it said all of that to stdout and nowhere else, so on a box where
// nobody is watching the console it said it to no one. This is the file.
//
// **Deliberately less than the Platform's, and the omissions are the decision.**
// No database, no OTel SDK, no collector, no exporter, no traces, no metrics,
// no query surface, no retention beyond size-capped rotation. Every one of those
// is something that can be unavailable at the moment it is needed, and the
// Supervisor's entire value here is being the thing that still works.
//
// **Nothing here is fatal and nothing here blocks.** A Supervisor that failed to
// start a child, and then failed to start *because* it could not write down that
// it had failed to start a child, would be the machinery defeating the thing it
// is for. A nil *Telemetry discards, so a component that was handed none is not
// a component that crashes.

// The record format is the Platform's, deliberately duplicated.
//
// ADR 0060 left this open — "either the Supervisor imports a small shared
// package or it duplicates a struct definition" — and named the duplication as a
// real hazard. It is duplicated, for the same reason the finding types in
// `spool.go` are: the Platform's telemetry package is `internal/`, the boundary
// is two published modules wide and that is not one of them, and a third
// published module carrying one struct would be a package to version, tag and
// keep in step for less code than this comment.
//
// What makes the duplication bounded rather than open-ended is the direction of
// the dependency. The Platform *reads* these files and the Supervisor never
// reads the Platform's, so only one side has to be tolerant — and JSON Lines
// with omitted empties is tolerant by construction: an unknown key is ignored, a
// missing key is empty. The hazard is not a file that cannot be parsed, it is a
// key quietly renamed. So the key set is pinned by a test that names every one
// of them, and that test is the whole guard.
//
// Three keys the Platform writes are absent rather than empty: `trace`, `span`
// and `module`. The Supervisor runs no traces (ADR 0060 says so) and links no
// Module, so writing those keys empty would be a claim it has them.
type telemetryEntry struct {
	Time      string         `json:"time"`
	Level     string         `json:"level"`
	Service   string         `json:"service,omitempty"`
	Instance  string         `json:"instance,omitempty"`
	Boot      string         `json:"boot,omitempty"`
	Component string         `json:"component,omitempty"`
	Message   string         `json:"message"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// serviceName is what the Supervisor calls itself in a record, matching the
// Platform's "mosaic-platform" so a merged read tells them apart by a field
// rather than by which file a line came out of.
const serviceName = "mosaic-supervisor"

// The components a record can be attributed to.
//
// One list rather than a literal at each call site, because `component` is the
// key somebody filters on: a spelling invented where it was written is a filter
// that matches nothing and reports no error for it.
const (
	componentChild      = "child"
	componentGeneration = "generation"
	componentTelemetry  = "telemetry"
)

// telemetryDirName and telemetryFileName place the log beside the Platform's.
//
// The Platform writes `logs/mosaic-platform.log` relative to its working
// directory, and in the shipped image that directory *is* the state directory —
// so the two land side by side and collecting both is a directory read rather
// than a search. The state directory rather than the working directory because
// this one must survive a reboot: a record written during a boot that then
// failed is exactly the one worth keeping.
const (
	telemetryDirName  = "logs"
	telemetryFileName = "mosaic-supervisor.log"
)

// maxLogBytes is where the file rotates, and one previous file is kept.
//
// The Supervisor's records are lifecycle facts rather than per-request ones —
// a handful per boot, a few dozen an hour from a child that is crash-looping —
// so 8MiB is months of ordinary running and hours of the worst case. Keeping one
// previous file is what stops a crash loop from being able to erase the boot
// that preceded it, which is the record that says what changed; two files is the
// smallest number that can hold both, and it bounds the whole thing at 16MiB
// without needing a retention policy to be a thing anybody configures.
const maxLogBytes = 8 << 20

// Level is a record's severity, with the same names and the same rendering as
// the Platform's so one reader sorts both.
type Level int

const (
	// LevelDebug is detail useful while diagnosing, off by default.
	LevelDebug Level = iota
	// LevelInfo is the normal narration of what the process is doing.
	LevelInfo
	// LevelWarn is a condition that did not stop the operation but should not
	// be normal.
	LevelWarn
	// LevelError is an operation that failed.
	LevelError
)

// String renders the level for a sink.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// ParseLevel resolves a configured level name, falling back to info. A
// misspelled level must not silence the one process that is still able to speak.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// redaction is the Platform's classification vocabulary (ADR 0056), reduced to
// what the Supervisor can actually produce.
//
// **The zero value is `unclassified`, and that is the whole design.** A Field
// built as a struct literal rather than through a constructor has not been
// classified by anybody, so it is redacted on the way out — a field somebody
// forgot to think about is dropped, not leaked. The class itself is never
// serialised, so it need not agree with any other component's spelling of it;
// only the behaviour has to.
//
// There is no Identifier class here, and its absence is deliberate rather than
// pending. Identifier exists to answer "is this the same subject as before"
// without recording who the subject is, and the Supervisor has no subjects: it
// never sees a user, a session or a request. Carrying a salted-digest path with
// nothing to digest would be a security mechanism nobody exercises.
type redaction int

const (
	redactUnclassified redaction = iota
	redactNone
	redactSensitive
	redactSecret
)

// redactedPlaceholder replaces any value not explicitly marked safe.
const redactedPlaceholder = "[REDACTED]"

// Field is one structured field of a record. Its class is unexported so the
// only way to produce a safe one is through a constructor below.
type Field struct {
	Key   string
	Value any
	class redaction
}

// String builds a Field written verbatim — component names, states, paths,
// versions, categories. Never use it for anything that could be a secret.
func String(key, value string) Field {
	return Field{Key: key, Value: value, class: redactNone}
}

// Int builds a verbatim Field for a count, a size or an exit code. Counts
// describe volume, not people.
func Int(key string, value int) Field {
	return Field{Key: key, Value: value, class: redactNone}
}

// Int64 builds a verbatim Field for a 64-bit count, size or offset.
func Int64(key string, value int64) Field {
	return Field{Key: key, Value: value, class: redactNone}
}

// Bool builds a verbatim Field for a flag.
func Bool(key string, value bool) Field {
	return Field{Key: key, Value: value, class: redactNone}
}

// Duration builds a verbatim Field for an elapsed time, rendered as the familiar
// "1.5s" rather than a nanosecond count so nobody reading a terminal has to
// divide.
func Duration(key string, value time.Duration) Field {
	return Field{Key: key, Value: value.Round(time.Millisecond).String(), class: redactNone}
}

// Err builds a verbatim Field carrying an error's message. Error text is
// authored by this binary and its dependencies rather than by a user, with the
// standing caveat that an error interpolating user input into its message has
// smuggled that input past this classification — which is a bug in the error.
func Err(err error) Field {
	if err == nil {
		return Field{Key: "error", Value: "", class: redactNone}
	}
	return Field{Key: "error", Value: err.Error(), class: redactNone}
}

// Sensitive builds a Field for a value that may carry personal or identifying
// data. The value is dropped here, at construction, so it is never buffered,
// never written and never present in a heap dump — a scrubber that ran at export
// could not offer that, because by then the value has already travelled.
func Sensitive(key string, value any) Field {
	return Field{Key: key, Value: redactNow(value), class: redactSensitive}
}

// Secret builds a Field for credential material. Dropped at construction, like
// Sensitive, and for the same reason.
func Secret(key string, value any) Field {
	return Field{Key: key, Value: redactNow(value), class: redactSecret}
}

// emitValue returns what a sink should write for f, re-applying redaction on the
// way out so an unclassified literal fails closed.
func (f Field) emitValue() any {
	if f.class == redactNone {
		return f.Value
	}
	return redactNow(f.Value)
}

// redactNow drops a classified value. An empty value has nothing to redact, so
// it stays empty rather than becoming a misleading "[REDACTED]".
func redactNow(value any) any {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok && s == "" {
		return ""
	}
	return redactedPlaceholder
}

// Telemetry writes the Supervisor's records to a file and narrates them on a
// console stream. Safe for concurrent use; a nil *Telemetry discards.
type Telemetry struct {
	mu      sync.Mutex
	console io.Writer
	file    *os.File
	path    string
	// written tracks the live file's size so rotation needs no stat per record.
	written int64
	min     Level
	// boot and instance name this process. The boot id is the Supervisor's own
	// and is handed to every child (ADR 0060), which is what stitches three
	// processes' records into one timeline; the instance distinguishes two runs
	// that somehow share one.
	boot     string
	instance string
	clock    func() time.Time
}

// OpenTelemetry prepares the Supervisor's telemetry against cfg, narrating to
// console.
//
// **It cannot fail.** A state directory that cannot be written is a degraded log
// and not a reason to refuse to boot — the console still carries everything, and
// the failure to open is itself the first thing said on it. An empty state
// directory is console-only by design: that is what a test gets, and what a
// Supervisor with nowhere durable to write gets.
func OpenTelemetry(cfg Config, console io.Writer) *Telemetry {
	t := &Telemetry{
		console:  console,
		min:      cfg.LogLevel,
		boot:     cfg.BootID,
		instance: NewID(),
		clock:    time.Now,
	}
	if cfg.StateDir == "" {
		return t
	}
	dir := filepath.Join(cfg.StateDir, telemetryDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Warn(componentTelemetry, "records are console-only", Err(err), String("path", dir))
		return t
	}
	t.path = filepath.Join(dir, telemetryFileName)
	if err := t.open(); err != nil {
		t.path = ""
		t.Warn(componentTelemetry, "records are console-only", Err(err), String("path", dir))
	}
	return t
}

// NewTelemetry builds a console-only Telemetry. It is what a component with
// nowhere durable to write gets, and what a test that wants to read back what
// the Supervisor said uses.
func NewTelemetry(console io.Writer, min Level) *Telemetry {
	return &Telemetry{console: console, min: min, instance: NewID(), clock: time.Now}
}

// open attaches the live file, resuming its size so a restart does not reset the
// rotation budget. The caller holds no lock: it is called from OpenTelemetry
// before the value is shared, and from rotate under the lock.
func (t *Telemetry) open() error {
	f, err := os.OpenFile(t.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	size := int64(0)
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	t.file, t.written = f, size
	return nil
}

// Close releases the file. A Telemetry with no file, or none at all, closes
// cleanly — the shutdown path must not have to know which it got.
func (t *Telemetry) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}

// Path is where records are written, empty when they are console-only. It is
// what the Platform will be told to read once there is a read path for it, and
// what a person is told to look at until then.
func (t *Telemetry) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

// Printf narrates at info level with no component and no fields.
//
// It exists because most of what the Supervisor says is a sentence rather than a
// fact with a shape, and because it matches the `func(string, ...any)` the whole
// package already passes around — so adopting this cost no call site a rewrite
// and, more to the point, nothing the Supervisor already said had to be dropped
// on the floor to get the structured records. Everything it says reaches the
// file; the events below simply reach it with fields attached.
func (t *Telemetry) Printf(format string, args ...any) {
	t.Event(LevelInfo, "", fmt.Sprintf(format, args...))
}

// Info, Warn and Error record one fact about a named component.
func (t *Telemetry) Info(component, message string, fields ...Field) {
	t.Event(LevelInfo, component, message, fields...)
}

// Warn records a condition that did not stop the operation.
func (t *Telemetry) Warn(component, message string, fields ...Field) {
	t.Event(LevelWarn, component, message, fields...)
}

// Error records an operation that failed.
func (t *Telemetry) Error(component, message string, fields ...Field) {
	t.Event(LevelError, component, message, fields...)
}

// Event writes one record to both destinations. Below the configured level it
// does nothing, which is the only filtering there is.
func (t *Telemetry) Event(level Level, component, message string, fields ...Field) {
	if t == nil || level < t.min {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// **Stamped under the lock, so the file's order is its timestamps' order.**
	// Read before it, two records written a microsecond apart from different
	// goroutines can land in the file in the opposite order to their times —
	// which is harmless until somebody merges this file with the Platform's by
	// sorting on time, and then it looks like the merge is wrong.
	now := t.clock().UTC()
	t.writeConsole(level, component, message, fields)
	t.writeFile(now, level, component, message, fields)
}

// writeConsole renders "mosaic-supervisor: component: message key=value".
//
// The service prefix is the same one the children's output carries, and for the
// same reason: three processes share one terminal and a line with nothing to say
// which said it is a line an operator cannot use. Info carries no level word, so
// the ordinary narration reads as a sentence; anything above it does, because
// that is the line worth spotting.
//
// Fields are sorted, so two runs of the same code put the same field in the same
// place and an eye scanning a terminal finds it there.
func (t *Telemetry) writeConsole(level Level, component, message string, fields []Field) {
	if t.console == nil {
		return
	}
	var b strings.Builder
	b.WriteString(serviceName)
	b.WriteString(": ")
	if level != LevelInfo {
		b.WriteString(strings.ToUpper(level.String()))
		b.WriteByte(' ')
	}
	if component != "" {
		b.WriteString(component)
		b.WriteString(": ")
	}
	b.WriteString(message)

	sorted := append([]Field(nil), fields...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	for _, f := range sorted {
		fmt.Fprintf(&b, " %s=%v", f.Key, f.emitValue())
	}
	b.WriteByte('\n')
	_, _ = io.WriteString(t.console, b.String())
}

// writeFile appends one JSON line, rotating first if this record would take the
// file past its cap.
//
// A record that cannot be marshalled is dropped rather than propagated: there is
// no caller in a position to handle a logging failure, and every call site would
// have to ignore it anyway.
func (t *Telemetry) writeFile(now time.Time, level Level, component, message string, fields []Field) {
	if t.file == nil {
		return
	}
	e := telemetryEntry{
		Time:      now.Format(time.RFC3339Nano),
		Level:     level.String(),
		Service:   serviceName,
		Instance:  t.instance,
		Boot:      t.boot,
		Component: component,
		Message:   message,
	}
	if len(fields) > 0 {
		e.Fields = make(map[string]any, len(fields))
		for _, f := range fields {
			e.Fields[f.Key] = f.emitValue()
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')

	if t.written+int64(len(line)) > maxLogBytes {
		t.rotate()
	}
	n, err := t.file.Write(line)
	t.written += int64(n)
	if err != nil {
		// The file has gone — a full disk, a state directory unmounted under a
		// running process. Drop it rather than retrying forever, and stop
		// pretending there is a file, so the console remains the whole record
		// instead of every subsequent write failing the same way.
		_ = t.file.Close()
		t.file, t.path = nil, ""
	}
}

// rotate moves the live file aside and starts a fresh one, keeping exactly one
// previous. A failure to rotate leaves the current file attached and over its
// cap, which is a log that is too big rather than a log that stopped — the right
// way round for the component whose job is not to break.
func (t *Telemetry) rotate() {
	if t.file == nil || t.path == "" {
		return
	}
	_ = t.file.Close()
	t.file = nil
	if err := os.Rename(t.path, t.path+".1"); err != nil {
		// Reattach to whatever is there rather than leaving the process silent.
		if openErr := t.open(); openErr != nil {
			t.path = ""
		}
		return
	}
	if err := t.open(); err != nil {
		t.path = ""
	}
}
