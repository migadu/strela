package recovery

import (
	"fmt"
	"runtime/debug"

	"go.uber.org/zap"
)

// RecoverPanic recovers from panics and logs them
// This should be called with defer at the start of every goroutine
func RecoverPanic(logger *zap.Logger, context string) {
	if r := recover(); r != nil {
		logger.Error("panic recovered",
			zap.String("context", context),
			zap.Any("panic", r),
			zap.String("stack", string(debug.Stack())))
	}
}

// SafeGo wraps a goroutine with panic recovery
func SafeGo(logger *zap.Logger, context string, fn func()) {
	go func() {
		defer RecoverPanic(logger, context)
		fn()
	}()
}

// RecoverPanicWithCallback recovers from panics and executes a callback
func RecoverPanicWithCallback(logger *zap.Logger, context string, onPanic func(interface{})) {
	if r := recover(); r != nil {
		logger.Error("panic recovered",
			zap.String("context", context),
			zap.Any("panic", r),
			zap.String("stack", string(debug.Stack())))

		if onPanic != nil {
			// Call the panic handler in a safe way (catch panics in the handler too)
			func() {
				defer func() {
					if r2 := recover(); r2 != nil {
						logger.Error("panic in panic handler",
							zap.String("context", context),
							zap.Any("secondary_panic", r2))
					}
				}()
				onPanic(r)
			}()
		}
	}
}

// HTTPPanicHandler returns an HTTP middleware that recovers from panics
func HTTPPanicHandler(logger *zap.Logger) func(next func()) func() {
	return func(next func()) func() {
		return func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("HTTP handler panic recovered",
						zap.Any("panic", r),
						zap.String("stack", string(debug.Stack())))

					// The panic is recovered, the handler will return 500 automatically
					// because the response wasn't written
					panic(fmt.Sprintf("HTTP handler panic: %v", r))
				}
			}()
			next()
		}
	}
}
