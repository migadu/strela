package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// LogWriter wraps a log output destination and supports reopening for log rotation.
// When the output is a file, Reopen closes the old handle and opens a new one so
// that newsyslog (or similar) rotation works correctly after SIGHUP.
type LogWriter struct {
	mu   sync.Mutex
	path string   // empty for stdout/stderr
	file *os.File // current output file (or os.Stdout/os.Stderr)
}

func (w *LogWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Write(p)
}

// Reopen closes and reopens the log file. No-op for stdout/stderr.
func (w *LogWriter) Reopen() error {
	if w.path == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	newFile, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("reopen log file %s: %w", w.path, err)
	}
	w.file.Close()
	w.file = newFile
	return nil
}

func (w *LogWriter) Close() error {
	if w.path == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// NewLogger creates a configured slog.Logger based on the LoggingConfig.
func NewLogger(cfg LoggingConfig) (*slog.Logger, *LogWriter, error) {
	level, err := parseLogLevel(cfg.Level)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid log level: %w", err)
	}

	writer, err := newLogWriter(cfg.Output)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create log writer: %w", err)
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level: level,
	}

	switch strings.ToLower(cfg.Format) {
	case "json":
		handler = slog.NewJSONHandler(writer, opts)
	case "console", "":
		handler = slog.NewTextHandler(writer, opts)
	default:
		writer.Close()
		return nil, nil, fmt.Errorf("invalid log format: %s (must be 'console' or 'json')", cfg.Format)
	}

	return slog.New(handler), writer, nil
}

func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown level: %s (must be debug, info, warn, or error)", level)
	}
}

func newLogWriter(output string) (*LogWriter, error) {
	switch strings.ToLower(output) {
	case "stderr", "":
		return &LogWriter{file: os.Stderr}, nil
	case "stdout":
		return &LogWriter{file: os.Stdout}, nil
	case "syslog":
		return &LogWriter{file: os.Stderr}, nil
	default:
		file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file %s: %w", output, err)
		}
		return &LogWriter{path: output, file: file}, nil
	}
}
