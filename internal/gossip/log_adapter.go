package gossip

import (
	"bytes"
	"strings"

	"go.uber.org/zap"
)

// zapLogAdapter adapts zap logger to io.Writer interface for memberlist
type zapLogAdapter struct {
	logger *zap.Logger
	buf    bytes.Buffer
}

// Write implements io.Writer
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
