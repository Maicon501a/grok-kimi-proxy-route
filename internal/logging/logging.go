// Package logging provides a small, dependency-free, concurrency-safe leveled
// logger with structured key/value fields and secret masking. It is shared by
// the Wails desktop app, the headless proxy (cmd/proxy) and every provider
// integration so a single request can be traced end to end via a request id.
package logging

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level is a log severity. Higher values are more severe.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// ParseLevel maps a config string (debug|info|warn|error) to a Level.
// Unknown values fall back to info.
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

// sensitiveKeys are field names whose values are masked before output.
var sensitiveKeys = map[string]struct{}{
	"token":          {},
	"access_token":   {},
	"refresh_token":  {},
	"api_key":        {},
	"apikey":         {},
	"authorization":  {},
	"password":       {},
	"secret":         {},
	"google_refresh": {},
	"sk-kimi":        {},
}

// MaskSecret redacts a secret value, keeping only a short hint so logs stay
// correlatable without leaking the credential. Empty input yields "".
func MaskSecret(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "****"
	}
	return v[:4] + "…" + v[len(v)-4:]
}

func isSensitive(key string) bool {
	lk := strings.ToLower(key)
	if _, ok := sensitiveKeys[lk]; ok {
		return true
	}
	// Catch compound names such as "kimi_refresh_token" or "xai_api_key".
	for k := range sensitiveKeys {
		if strings.Contains(lk, k) {
			return true
		}
	}
	return false
}

// Logger is a leveled logger writing structured lines to out. All methods are
// safe for concurrent use.
type Logger struct {
	mu     sync.Mutex
	out    io.Writer
	level  Level
	fields map[string]any
	now    func() time.Time
}

// New returns a Logger writing to out at the given level.
func New(out io.Writer, level Level) *Logger {
	if out == nil {
		out = os.Stderr
	}
	return &Logger{out: out, level: level, fields: map[string]any{}, now: time.Now}
}

// SetOutput swaps the destination writer (e.g. a file) at runtime.
func (l *Logger) SetOutput(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w == nil {
		w = os.Stderr
	}
	l.out = w
}

// SetLevel changes the minimum severity emitted.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// GetLevel returns the current minimum severity.
func (l *Logger) GetLevel() Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// With returns a child logger that carries the given base fields on every line.
// The parent is unchanged. Fields are copied so later mutation is isolated.
func (l *Logger) With(kv ...any) *Logger {
	l.mu.Lock()
	merged := make(map[string]any, len(l.fields)+len(kv)/2)
	for k, v := range l.fields {
		merged[k] = v
	}
	l.mu.Unlock()
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		if key == "" {
			continue
		}
		merged[key] = kv[i+1]
	}
	return &Logger{out: l.out, level: l.level, fields: merged, now: l.now}
}

func (l *Logger) log(level Level, msg string, kv []any) {
	// The lock is held through the write so concurrent callers serialize on any
	// io.Writer (os.File, bytes.Buffer, …) — a single Write call is not atomic
	// for all writer implementations.
	l.mu.Lock()
	defer l.mu.Unlock()
	if level < l.level {
		return
	}

	fields := make(map[string]any, len(l.fields)+len(kv)/2)
	for k, v := range l.fields {
		fields[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		if key == "" {
			continue
		}
		fields[key] = kv[i+1]
	}

	var b strings.Builder
	b.WriteString(l.now().UTC().Format(time.RFC3339Nano))
	b.WriteByte(' ')
	b.WriteString(level.String())
	b.WriteByte(' ')
	b.WriteString(msg)

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(formatValue(k, fields[k]))
	}
	b.WriteByte('\n')

	_, _ = io.WriteString(l.out, b.String())
}

func formatValue(key string, v any) string {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case error:
		s = t.Error()
	default:
		s = fmt.Sprintf("%v", v)
	}
	if isSensitive(key) {
		s = MaskSecret(s)
	}
	if strings.ContainsAny(s, " \t\"") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// Debug/Info/Warn/Error emit a structured line with optional key/value pairs.
func (l *Logger) Debug(msg string, kv ...any) { l.log(LevelDebug, msg, kv) }
func (l *Logger) Info(msg string, kv ...any)  { l.log(LevelInfo, msg, kv) }
func (l *Logger) Warn(msg string, kv ...any)  { l.log(LevelWarn, msg, kv) }
func (l *Logger) Error(msg string, kv ...any) { l.log(LevelError, msg, kv) }

// ---- package-level default logger ----

var std = New(os.Stderr, LevelInfo)

// Default returns the process-wide logger.
func Default() *Logger { return std }

// SetOutput redirects the default logger (used by main entrypoints).
func SetOutput(w io.Writer) { std.SetOutput(w) }

// SetLevel changes the default logger severity.
func SetLevel(level Level) { std.SetLevel(level) }

// With returns a child of the default logger carrying the given fields.
func With(kv ...any) *Logger { return std.With(kv...) }

func Debug(msg string, kv ...any) { std.Debug(msg, kv...) }
func Info(msg string, kv ...any)  { std.Info(msg, kv...) }
func Warn(msg string, kv ...any)  { std.Warn(msg, kv...) }
func Error(msg string, kv ...any) { std.Error(msg, kv...) }

// ---- request id context plumbing ----

type ctxKey int

const requestIDKey ctxKey = 1

// WithRequestID stores a request id on ctx for downstream loggers.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request id stored on ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// FromContext returns a logger pre-populated with the request id from ctx (if
// any). Callers can chain .With(...) for more fields.
func FromContext(ctx context.Context) *Logger {
	if id := RequestIDFrom(ctx); id != "" {
		return std.With("req_id", id)
	}
	return std
}
