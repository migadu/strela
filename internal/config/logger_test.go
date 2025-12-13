package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLogger_Defaults(t *testing.T) {
	cfg := LoggingConfig{}
	// SetDefaults would be called by LoadConfig
	if cfg.Output == "" {
		cfg.Output = "stderr"
	}
	if cfg.Format == "" {
		cfg.Format = "console"
	}
	if cfg.Level == "" {
		cfg.Level = "info"
	}

	logger, err := NewLogger(cfg)
	if err != nil {
		t.Fatalf("NewLogger() failed: %v", err)
	}
	if logger == nil {
		t.Fatal("NewLogger() returned nil logger")
	}
}

func TestNewLogger_AllLevels(t *testing.T) {
	tests := []struct {
		level    string
		wantErr  bool
		expected slog.Level
	}{
		{"debug", false, slog.LevelDebug},
		{"info", false, slog.LevelInfo},
		{"warn", false, slog.LevelWarn},
		{"warning", false, slog.LevelWarn},
		{"error", false, slog.LevelError},
		{"DEBUG", false, slog.LevelDebug},
		{"INFO", false, slog.LevelInfo},
		{"", false, slog.LevelInfo}, // empty defaults to info
		{"invalid", true, slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			cfg := LoggingConfig{
				Output: "stderr",
				Format: "console",
				Level:  tt.level,
			}

			logger, err := NewLogger(cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewLogger() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && logger == nil {
				t.Error("NewLogger() returned nil logger without error")
			}
		})
	}
}

func TestNewLogger_AllFormats(t *testing.T) {
	tests := []struct {
		format  string
		wantErr bool
	}{
		{"console", false},
		{"json", false},
		{"CONSOLE", false},
		{"JSON", false},
		{"", false}, // empty defaults to console
		{"invalid", true},
		{"xml", true},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			cfg := LoggingConfig{
				Output: "stderr",
				Format: tt.format,
				Level:  "info",
			}

			logger, err := NewLogger(cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewLogger() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && logger == nil {
				t.Error("NewLogger() returned nil logger without error")
			}
		})
	}
}

func TestNewLogger_AllOutputs(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{"stderr", "stderr", false},
		{"stdout", "stdout", false},
		{"syslog", "syslog", false},
		{"STDERR", "STDERR", false},
		{"STDOUT", "STDOUT", false},
		{"", "", false}, // empty defaults to stderr
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := LoggingConfig{
				Output: tt.output,
				Format: "console",
				Level:  "info",
			}

			logger, err := NewLogger(cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewLogger() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && logger == nil {
				t.Error("NewLogger() returned nil logger without error")
			}
		})
	}
}

func TestNewLogger_FileOutput(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	cfg := LoggingConfig{
		Output: logFile,
		Format: "json",
		Level:  "debug",
	}

	logger, err := NewLogger(cfg)
	if err != nil {
		t.Fatalf("NewLogger() failed: %v", err)
	}
	if logger == nil {
		t.Fatal("NewLogger() returned nil logger")
	}

	// Write a log message
	logger.Info("test message", "key", "value")

	// Verify file was created
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Errorf("log file was not created: %s", logFile)
	}

	// Read file content
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}

	// Verify content is JSON
	contentStr := string(content)
	if !strings.Contains(contentStr, "test message") {
		t.Errorf("log file does not contain expected message: %s", contentStr)
	}
	if !strings.Contains(contentStr, `"key":"value"`) {
		t.Errorf("log file does not contain expected JSON: %s", contentStr)
	}
}

func TestNewLogger_FileOutput_InvalidPath(t *testing.T) {
	cfg := LoggingConfig{
		Output: "/nonexistent/directory/test.log",
		Format: "console",
		Level:  "info",
	}

	logger, err := NewLogger(cfg)
	if err == nil {
		t.Error("NewLogger() should fail with invalid file path")
	}
	if logger != nil {
		t.Error("NewLogger() should return nil logger on error")
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected slog.Level
		wantErr  bool
	}{
		{"debug", slog.LevelDebug, false},
		{"DEBUG", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"INFO", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"WARN", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"WARNING", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"ERROR", slog.LevelError, false},
		{"", slog.LevelInfo, false}, // empty defaults to info
		{"invalid", slog.LevelInfo, true},
		{"trace", slog.LevelInfo, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			level, err := parseLogLevel(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseLogLevel(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if level != tt.expected {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.input, level, tt.expected)
			}
		})
	}
}

func TestGetLogWriter(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{"stderr", "stderr", false},
		{"stdout", "stdout", false},
		{"syslog", "syslog", false},
		{"STDERR", "STDERR", false},
		{"STDOUT", "STDOUT", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer, err := getLogWriter(tt.output)
			if (err != nil) != tt.wantErr {
				t.Errorf("getLogWriter(%q) error = %v, wantErr %v", tt.output, err, tt.wantErr)
				return
			}
			if !tt.wantErr && writer == nil {
				t.Error("getLogWriter() returned nil writer without error")
			}
		})
	}
}

func TestGetLogWriter_File(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	writer, err := getLogWriter(logFile)
	if err != nil {
		t.Fatalf("getLogWriter() failed: %v", err)
	}
	if writer == nil {
		t.Fatal("getLogWriter() returned nil writer")
	}

	// Write to the writer
	_, err = writer.Write([]byte("test message\n"))
	if err != nil {
		t.Fatalf("failed to write to log file: %v", err)
	}

	// Verify file exists and contains message
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(content), "test message") {
		t.Errorf("log file does not contain expected message: %s", content)
	}
}

func TestConfig_SetDefaults_Logging(t *testing.T) {
	cfg := &Config{
		Logging: LoggingConfig{},
	}

	cfg.SetDefaults()

	if cfg.Logging.Output != "stderr" {
		t.Errorf("expected default output 'stderr', got %q", cfg.Logging.Output)
	}
	if cfg.Logging.Format != "console" {
		t.Errorf("expected default format 'console', got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("expected default level 'info', got %q", cfg.Logging.Level)
	}
}

func TestConfig_SetDefaults_Logging_NoOverwrite(t *testing.T) {
	cfg := &Config{
		Logging: LoggingConfig{
			Output: "stdout",
			Format: "json",
			Level:  "debug",
		},
	}

	cfg.SetDefaults()

	if cfg.Logging.Output != "stdout" {
		t.Errorf("expected output 'stdout', got %q", cfg.Logging.Output)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("expected format 'json', got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("expected level 'debug', got %q", cfg.Logging.Level)
	}
}
