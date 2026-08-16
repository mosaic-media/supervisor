// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors

package supervisor

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/log"
	logsdk "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// The Supervisor's own telemetry (supervisor#5), on OpenTelemetry (sdk#8).
//
// There is a whole class of failure where the process that would normally report
// is the process that is broken — a migration that will not run, a database that
// is not there, a Generation that starts and immediately dies — and the
// Supervisor is the process that survives all of it and the one that caused the
// transition. This is where it writes that down.
//
// The record format is OpenTelemetry's, which neither process owns (sdk#8), so
// the file is ordinary OTLP JSON that any collector reads. There is no
// Mosaic-authored format here to keep in step with the Platform's.
//
// The file exporter is the unconditional one, and it needs nothing running.
// supervisor#5 rejected exporting over OTLP because an exporter needs a live
// collector — the same aliveness assumption, relocated — so the collector is an
// operator's opt-in extra beside the file and never a replacement for it, its
// exporter is batched so an export never sits on the goroutine that recorded a
// child's exit, and failing to build it warns rather than refusing to boot.
// Nothing here may block startup on something answering.
//
// Nothing here is fatal and nothing here blocks. A Supervisor that failed to
// start a child, and then failed to start because it could not write down that
// it had failed to start a child, would be the machinery defeating the thing it
// is for. A nil *Telemetry discards.

// serviceName is what the Supervisor calls itself, matching the Platform's
// "mosaic-platform" so a merged read tells them apart by a resource attribute
// rather than by which file a line came out of.
const serviceName = "mosaic-supervisor"

// bootAttribute names the boot id shared with the children (supervisor#5), which is
// what stitches three processes' records into one timeline.
//
// A Mosaic-namespaced key because OpenTelemetry has no convention for it: a boot
// is not service.instance.id (that names a process, and three processes share
// one boot) and not a trace id (a boot is not a request).
const bootAttribute = "mosaic.boot.id"

// The components a record can be attributed to.
//
// One list rather than a literal at each call site, because component is the key
// somebody filters on: a spelling invented where it was written is a filter that
// matches nothing and reports no error for it.
const (
	componentChild      = "child"
	componentGeneration = "generation"
	componentTelemetry  = "telemetry"
)

// componentAttribute is the key the constants above are written under.
const componentAttribute = "component"

// telemetryDirName and telemetryFileName place the log beside the Platform's.
//
// The Platform writes logs/mosaic-platform.log relative to its working
// directory, and in the shipped image that directory is the state directory, so
// the two land side by side and collecting both is a directory read rather than
// a search. The state directory rather than the working directory because this
// one must survive a reboot: a record written during a boot that then failed is
// exactly the one worth keeping.
const (
	telemetryDirName  = "logs"
	telemetryFileName = "mosaic-supervisor.log"
)

// maxLogBytes is where the file rotates, and one previous file is kept.
//
// The Supervisor's records are lifecycle facts rather than per-request ones — a
// handful per boot, a few dozen an hour from a child that is crash-looping — so
// 8MiB is months of ordinary running and hours of the worst case. Keeping one
// previous file is what stops a crash loop from being able to erase the boot
// that preceded it, which is the record that says what changed; two files is the
// smallest number that can hold both, and it bounds the whole thing at 16MiB
// without needing a retention policy anybody configures.
const maxLogBytes = 8 << 20

// Level is a record's severity. It is the Supervisor's own small enum rather
// than log.Severity used directly, because the configuration surface is a word
// an operator writes in an environment variable and OTel's scale has 24 values.
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

// String renders the level for the console and for a record's severity text.
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

// severity maps onto OpenTelemetry's scale.
func (l Level) severity() log.Severity {
	switch l {
	case LevelDebug:
		return log.SeverityDebug
	case LevelWarn:
		return log.SeverityWarn
	case LevelError:
		return log.SeverityError
	default:
		return log.SeverityInfo
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

// redaction is the Platform's classification vocabulary (platform#34), reduced to
// what the Supervisor can actually produce.
//
// The zero value is unclassified, and that is the whole design. A Field built as
// a struct literal rather than through a constructor has not been classified by
// anybody, so it is redacted on the way out — a field somebody forgot to think
// about is dropped, not leaked. OpenTelemetry has no notion of classification,
// so the conversion to an attribute is the last place the rule can be applied,
// and Field.attribute applies it to every field on every path.
//
// There is no Identifier class here, and its absence is deliberate rather than
// pending. Identifier exists to answer "is this the same subject as before"
// without recording who the subject is, and the Supervisor has no subjects: it
// never sees a user, a session or a request.
type redaction int

const (
	redactUnclassified redaction = iota
	redactNone
	redactSensitive
	redactSecret
)

// redactedPlaceholder replaces any value not explicitly marked safe.
const redactedPlaceholder = "[REDACTED]"

// Field is one structured value on a record. Its class is unexported so the
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

// Int builds a verbatim Field for a count, a size or an exit code.
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

// attribute converts f, re-applying redaction so an unclassified literal fails
// closed.
func (f Field) attribute() attribute.KeyValue {
	value := f.Value
	if f.class != redactNone {
		value = redactNow(value)
	}
	switch v := value.(type) {
	case nil:
		return attribute.String(f.Key, "")
	case string:
		return attribute.String(f.Key, v)
	case bool:
		return attribute.Bool(f.Key, v)
	case int:
		return attribute.Int(f.Key, v)
	case int64:
		return attribute.Int64(f.Key, v)
	case float64:
		return attribute.Float64(f.Key, v)
	default:
		return attribute.String(f.Key, fmt.Sprint(v))
	}
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

// Telemetry records what the Supervisor does. Safe for concurrent use; a nil
// *Telemetry discards.
type Telemetry struct {
	provider *logsdk.LoggerProvider
	logger   log.Logger
	rotating *rotatingFile
	min      Level
	clock    func() time.Time
}

// OpenTelemetry prepares the Supervisor's telemetry against cfg, narrating to
// console.
//
// It cannot fail. A state directory that cannot be written is a degraded log and
// not a reason to refuse to boot — the console still carries everything, and the
// failure to open is itself the first thing said on it. An empty state directory
// is console-only by design: that is what a test gets, and what a Supervisor
// with nowhere durable to write gets.
func OpenTelemetry(cfg Config, console io.Writer) *Telemetry {
	t := &Telemetry{min: cfg.LogLevel, clock: time.Now}

	var processors []logsdk.LoggerProviderOption
	if console != nil {
		// The console is an exporter like any other, so a person at a terminal
		// and a file being read afterwards see the same records rather than one
		// being a subset of the other.
		processors = append(processors, logsdk.WithProcessor(
			logsdk.NewSimpleProcessor(&consoleExporter{out: console})))
	}

	var openErr, otlpErr error
	var openPath string
	if cfg.StateDir != "" {
		dir := filepath.Join(cfg.StateDir, telemetryDirName)
		openPath = filepath.Join(dir, telemetryFileName)
		rotating, err := openRotatingFile(dir, openPath, maxLogBytes)
		if err != nil {
			openErr = err
		} else {
			// stdoutlog writes OTLP-shaped JSON, one record per line, which is
			// the point of the change: the file is readable by tooling nobody
			// here had to write.
			exporter, expErr := stdoutlog.New(stdoutlog.WithWriter(rotating))
			if expErr != nil {
				openErr = expErr
				_ = rotating.Close()
			} else {
				t.rotating = rotating
				processors = append(processors, logsdk.WithProcessor(
					logsdk.NewSimpleProcessor(exporter)))
			}
		}
	}

	// Additive, and never a replacement for the file. A collector that is down,
	// unreachable or misconfigured must cost records in the collector and none
	// on disk — an install whose observability backend went away must not also
	// lose the local account of why its Platform will not start.
	//
	// Batched rather than simple, unlike the two local exporters: this one is
	// over a network, so an export must not sit on the goroutine that recorded a
	// child's exit. The queue drops when it fills, which is the right direction
	// for a component that must not stall.
	if cfg.OTLPEndpoint != "" {
		exporter, err := otlploghttp.New(context.Background(),
			otlploghttp.WithEndpointURL(otlpLogsURL(cfg.OTLPEndpoint)))
		if err != nil {
			otlpErr = err
		} else {
			processors = append(processors, logsdk.WithProcessor(
				logsdk.NewBatchProcessor(exporter)))
		}
	}

	options := append(processors, logsdk.WithResource(resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.instance.id", NewID()),
		attribute.String(bootAttribute, cfg.BootID),
	)))
	t.provider = logsdk.NewLoggerProvider(options...)
	t.logger = t.provider.Logger(serviceName)

	if openErr != nil {
		t.Warn(componentTelemetry, "records are console-only", Err(openErr))
	}
	if otlpErr != nil {
		// A warning rather than a refusal to boot. An operator who asked for a
		// collector and did not get one needs to be told, and a Supervisor that
		// would not start because its telemetry export could not be configured
		// would be the machinery defeating the thing it is for.
		t.Warn(componentTelemetry, "the configured collector could not be reached; "+
			"records are local only", Err(otlpErr), String("endpoint", cfg.OTLPEndpoint))
	} else if cfg.OTLPEndpoint != "" {
		// Said at boot, because "is anything actually going to the collector"
		// is the first question asked when nothing arrives at the other end.
		t.Info(componentTelemetry, "also exporting to a collector",
			String("endpoint", cfg.OTLPEndpoint))
	}
	return t
}

// NewTelemetry builds a console-only Telemetry. It is what a component with
// nowhere durable to write gets, and what a test that wants to read back what
// the Supervisor said uses.
func NewTelemetry(console io.Writer, min Level) *Telemetry {
	return OpenTelemetry(Config{LogLevel: min}, console)
}

// Close flushes and releases the pipeline. A Telemetry with no file, or none at
// all, closes cleanly — the shutdown path must not have to know which it got.
func (t *Telemetry) Close() error {
	if t == nil || t.provider == nil {
		return nil
	}
	err := t.provider.Shutdown(context.Background())
	if t.rotating != nil {
		if closeErr := t.rotating.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

// Path is where records are written, empty when they are console-only.
func (t *Telemetry) Path() string {
	if t == nil || t.rotating == nil {
		return ""
	}
	return t.rotating.path
}

// Printf narrates at info level with no component and no fields.
//
// It exists because most of what the Supervisor says is a sentence rather than a
// fact with a shape, and because it matches the func(string, ...any) the whole
// package already passes around.
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

// Event emits one record. Below the configured level it does nothing, which is
// the only filtering there is: no sampling and no per-component rules, so
// nothing can quietly discard the one record that mattered.
func (t *Telemetry) Event(level Level, component, message string, fields ...Field) {
	if t == nil || t.logger == nil || level < t.min {
		return
	}
	var record log.Record
	record.SetTimestamp(t.clock().UTC())
	record.SetSeverity(level.severity())
	// Set alongside the number because a human reading an exported record
	// should not have to know that 9 means info.
	record.SetSeverityText(level.String())
	record.SetBody(attribute.StringValue(message))
	if component != "" {
		record.AddAttributes(attribute.String(componentAttribute, component))
	}
	for _, f := range fields {
		record.AddAttributes(f.attribute())
	}
	t.logger.Emit(context.Background(), record)
}

// otlpLogsURL resolves what an operator configured into the URL the exporter
// wants.
//
// It takes a base URL, like OTEL_EXPORTER_OTLP_ENDPOINT, because that is the one
// people know: an operator writes http://collector:4318 and the signal path is
// appended. Passed straight through, the exporter would POST to "/" and a
// collector would answer 404 — a misconfiguration that looks exactly like the
// collector being wrong rather than the URL being incomplete. A URL that already
// carries a path is left alone, so somebody fronting a collector at
// /otlp/v1/logs is not overruled.
func otlpLogsURL(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Path != "" && parsed.Path != "/" {
		return endpoint
	}
	parsed.Path = otlpLogsPath
	return parsed.String()
}

// otlpLogsPath is the OTLP/HTTP path for log records, fixed by the protocol.
const otlpLogsPath = "/v1/logs"

// consoleExporter renders records for a person at a terminal.
//
// It is an OTel exporter rather than a second logging path, so the console and
// the file carry the same records and neither can drift from the other. The
// rendering is the only thing that differs, and it differs because JSON on a
// terminal costs more legibility than the structure buys.
type consoleExporter struct {
	mu  sync.Mutex
	out io.Writer
}

// Export renders "mosaic-supervisor: component: message key=value".
//
// The service prefix is the same one the children's output carries, and for the
// same reason: three processes share one terminal and a line with nothing to say
// which said it is a line an operator cannot use. Info carries no level word, so
// the ordinary narration reads as a sentence; anything above it does, because
// that is the line worth spotting.
func (e *consoleExporter) Export(_ context.Context, records []logsdk.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range records {
		_, _ = io.WriteString(e.out, renderConsole(&records[i]))
	}
	return nil
}

func (e *consoleExporter) Shutdown(context.Context) error   { return nil }
func (e *consoleExporter) ForceFlush(context.Context) error { return nil }

// renderConsole builds one human-readable line.
//
// Fields are sorted, so two runs of the same code put the same field in the same
// place and an eye scanning a terminal finds it there. The component is lifted
// out of the attributes and into the prefix, since it is what a reader groups by
// rather than one fact among several.
func renderConsole(record *logsdk.Record) string {
	var component string
	var fields []string
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == componentAttribute {
			component = kv.Value.Emit()
			return true
		}
		fields = append(fields, string(kv.Key)+"="+kv.Value.Emit())
		return true
	})
	sort.Strings(fields)

	var b strings.Builder
	b.WriteString(serviceName)
	b.WriteString(": ")
	if text := record.SeverityText(); text != LevelInfo.String() {
		b.WriteString(strings.ToUpper(text))
		b.WriteByte(' ')
	}
	if component != "" {
		b.WriteString(component)
		b.WriteString(": ")
	}
	b.WriteString(record.Body().Emit())
	for _, f := range fields {
		b.WriteByte(' ')
		b.WriteString(f)
	}
	b.WriteByte('\n')
	return b.String()
}

// rotatingFile is the writer behind the file exporter: append, and start a fresh
// file once the current one passes its cap, keeping exactly one previous.
//
// It is the Supervisor's own rather than a dependency because rotation is the
// whole of supervisor#5's retention policy and a library for it would be a larger
// surface than the thing it does.
type rotatingFile struct {
	mu      sync.Mutex
	dir     string
	path    string
	limit   int64
	file    *os.File
	written int64
}

// openRotatingFile prepares the directory and attaches the live file, resuming
// its size so a restart does not reset the rotation budget.
func openRotatingFile(dir, path string, limit int64) (*rotatingFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &rotatingFile{dir: dir, path: path, limit: limit}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	size := int64(0)
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	r.file, r.written = f, size
	return nil
}

// Write appends p, rotating first if it would take the file past its cap.
//
// It never reports an error: this is on the path a record takes, and the only
// destination is a file whose failure there is nowhere useful to report. A file
// that has gone — a full disk, a state directory unmounted under a running
// process — detaches, so the console remains the whole record rather than every
// subsequent write failing the same way.
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return len(p), nil
	}
	if r.written+int64(len(p)) > r.limit {
		r.rotate()
	}
	n, err := r.file.Write(p)
	r.written += int64(n)
	if err != nil {
		_ = r.file.Close()
		r.file = nil
	}
	return len(p), nil
}

// rotate moves the live file aside and starts a fresh one, keeping exactly one
// previous. A failure to rotate leaves the current file attached and over its
// cap, which is a log that is too big rather than a log that stopped — the right
// way round for the component whose job is not to break.
func (r *rotatingFile) rotate() {
	_ = r.file.Close()
	r.file = nil
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		if openErr := r.open(); openErr != nil {
			r.file = nil
		}
		return
	}
	if err := r.open(); err != nil {
		r.file = nil
	}
}

// Close releases the live file.
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
