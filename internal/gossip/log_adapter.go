package gossip

import (
	"bytes"
	"log/slog"
	"strings"
)

// slogLogAdapter adapts slog logger to io.Writer interface for memberlist's
// logging output. Memberlist uses Go's standard log package, which writes to
// an io.Writer. This adapter parses memberlist's log messages and routes them
// to the appropriate slog log levels (DEBUG, INFO, WARN, ERROR).
type slogLogAdapter struct {
	logger *slog.Logger
	buf    bytes.Buffer
}

// Write implements io.Writer by parsing memberlist log output and routing
// to the appropriate slog log level based on log level prefixes ([DEBUG], [INFO], etc.).
func (s *slogLogAdapter) Write(p []byte) (n int, err error) {
	msg := string(p)
	msg = strings.TrimSpace(msg)

	if msg == "" {
		return len(p), nil
	}

	// Parse memberlist log levels
	if strings.Contains(msg, "[DEBUG]") {
		s.logger.Debug(strings.TrimPrefix(msg, "[DEBUG] "))
	} else if strings.Contains(msg, "[INFO]") {
		s.logger.Info(strings.TrimPrefix(msg, "[INFO] "))
	} else if strings.Contains(msg, "[WARN]") {
		s.logger.Warn(strings.TrimPrefix(msg, "[WARN] "))
	} else if strings.Contains(msg, "[ERROR]") {
		s.logger.Error(strings.TrimPrefix(msg, "[ERROR] "))
	} else {
		s.logger.Debug(msg)
	}

	return len(p), nil
}
