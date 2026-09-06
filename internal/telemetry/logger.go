package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	defaultLogger *slog.Logger
	mu            sync.RWMutex
)

func init() {
	// Fallback text logger to stderr before explicit initialization.
	defaultLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// MultiHandler multiplexes log records across multiple slog.Handler instances.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler creates a handler broadcasting to all provided handlers.
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

// Enabled returns true if any handler accepts the given level.
func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle sends the record to all configured handlers.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// WithAttrs returns a new MultiHandler with attributes added to all child handlers.
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: newHandlers}
}

// WithGroup returns a new MultiHandler with group added to all child handlers.
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: newHandlers}
}

// Config specifies options for telemetry initialization.
type Config struct {
	ConsoleWriter io.Writer
	LogDir        string
	RunID         string
	Verbose       bool
}

// shortTime renders log timestamps as clock time (15:04:05) on the
// human-readable surfaces (console, .log file); the JSONL machine copy keeps
// full precision.
func shortTime(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey && len(groups) == 0 {
		if t, ok := a.Value.Any().(time.Time); ok {
			a.Value = slog.StringValue(t.Format("15:04:05"))
		}
	}
	return a
}

// Init configures the global logger and optional file logging.
// Every run writes three surfaces: console (live human view), a JSONL file
// (machine copy for the audit/tuning loop), and a .log text file (human
// twin). Returns a cleanup function that closes log files.
func Init(cfg Config) (func(), error) {
	mu.Lock()
	defer mu.Unlock()

	var closers []io.Closer
	var handlers []slog.Handler

	consoleLevel := slog.LevelInfo
	if cfg.Verbose {
		consoleLevel = slog.LevelDebug
	}

	consoleOut := cfg.ConsoleWriter
	if consoleOut == nil {
		consoleOut = os.Stderr
	}

	consoleHandler := slog.NewTextHandler(consoleOut, &slog.HandlerOptions{
		Level:       consoleLevel,
		ReplaceAttr: shortTime,
	})
	handlers = append(handlers, consoleHandler)

	if cfg.LogDir != "" && cfg.RunID != "" {
		if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
			return func() {}, err
		}

		jsonFile, err := os.OpenFile(filepath.Join(cfg.LogDir, "run-"+cfg.RunID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return func() {}, err
		}
		closers = append(closers, jsonFile)

		jsonHandler := slog.NewJSONHandler(jsonFile, &slog.HandlerOptions{
			Level: slog.LevelDebug, // machine copy always captures full debug detail
		})
		handlers = append(handlers, jsonHandler)

		textFile, err := os.OpenFile(filepath.Join(cfg.LogDir, "run-"+cfg.RunID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return func() {}, err
		}
		closers = append(closers, textFile)

		textHandler := slog.NewTextHandler(textFile, &slog.HandlerOptions{
			Level:       slog.LevelDebug,
			ReplaceAttr: shortTime,
		})
		handlers = append(handlers, textHandler)
	}

	composite := NewMultiHandler(handlers...)
	defaultLogger = slog.New(composite)

	cleanup := func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}

	return cleanup, nil
}

// Log returns the logger enriched with run_id from context if present.
func Log(ctx context.Context) *slog.Logger {
	mu.RLock()
	l := defaultLogger
	mu.RUnlock()

	if ctx == nil {
		return l
	}

	runID := RunIDFromContext(ctx)
	if runID != "" {
		return l.With(slog.String("run_id", runID))
	}
	return l
}
