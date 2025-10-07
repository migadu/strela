package gossip

import (
	"bytes"
	"strings"

	"go.uber.org/zap"
)

// zapLogAdapter adapts zap logger to io.Writer interface for memberlist's
// logging output. Memberlist uses Go's standard log package, which writes to
// an io.Writer. This adapter parses memberlist's log messages and routes them
// to the appropriate zap log levels (DEBUG, INFO, WARN, ERROR).
type zapLogAdapter struct {
	logger *zap.Logger
	buf    bytes.Buffer
}

// Write implements io.Writer by parsing memberlist log output and routing
// to the appropriate zap log level based on log level prefixes ([DEBUG], [INFO], etc.).
func (z *zapLogAdapter) Write(p []byte) (n int, err error) {
	msg := string(p)
	msg = strings.TrimSpace(msg)

	if msg == "" {
		return len(p), nil
	}

	// Parse memberlist log levels
	if strings.Contains(msg, "[DEBUG]") {
		z.logger.Debug(strings.TrimPrefix(msg, "[DEBUG] "))
	} else if strings.Contains(msg, "[INFO]") {
		z.logger.Info(strings.TrimPrefix(msg, "[INFO] "))
	} else if strings.Contains(msg, "[WARN]") {
		z.logger.Warn(strings.TrimPrefix(msg, "[WARN] "))
	} else if strings.Contains(msg, "[ERROR]") {
		z.logger.Error(strings.TrimPrefix(msg, "[ERROR] "))
	} else {
		z.logger.Debug(msg)
	}

	return len(p), nil
}
