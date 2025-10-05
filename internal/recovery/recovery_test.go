package recovery

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestRecoverPanic(t *testing.T) {
	// Create a logger that writes to a buffer
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.DebugLevel)
	logger := zap.New(core)

	// Trigger a panic and recover
	func() {
		defer RecoverPanic(logger, "test context")
		panic("test panic")
	}()

	// Check that panic was logged
	logOutput := buf.String()
	if !strings.Contains(logOutput, "panic recovered") {
		t.Errorf("Expected 'panic recovered' in log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "test context") {
		t.Errorf("Expected 'test context' in log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "test panic") {
		t.Errorf("Expected 'test panic' in log, got: %s", logOutput)
	}
}

func TestSafeGo(t *testing.T) {
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.DebugLevel)
	logger := zap.New(core)

	var wg sync.WaitGroup
	wg.Add(1)

	// Start a goroutine that will panic
	SafeGo(logger, "safe goroutine", func() {
		defer wg.Done()
		panic("goroutine panic")
	})

	wg.Wait()
	time.Sleep(100 * time.Millisecond) // Give logger time to flush

	// Check that panic was logged
	logOutput := buf.String()
	if !strings.Contains(logOutput, "panic recovered") {
		t.Errorf("Expected 'panic recovered' in log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "safe goroutine") {
		t.Errorf("Expected 'safe goroutine' in log, got: %s", logOutput)
	}
}

func TestRecoverPanicWithCallback(t *testing.T) {
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.DebugLevel)
	logger := zap.New(core)

	callbackCalled := false
	var panicValue interface{}

	func() {
		defer RecoverPanicWithCallback(logger, "callback test", func(p interface{}) {
			callbackCalled = true
			panicValue = p
		})
		panic("callback panic")
	}()

	if !callbackCalled {
		t.Error("Expected callback to be called")
	}
	if panicValue != "callback panic" {
		t.Errorf("Expected panic value 'callback panic', got: %v", panicValue)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "panic recovered") {
		t.Errorf("Expected 'panic recovered' in log, got: %s", logOutput)
	}
}

func TestRecoverPanicWithCallbackPanic(t *testing.T) {
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.DebugLevel)
	logger := zap.New(core)

	// Test that a panic in the callback itself is handled
	func() {
		defer RecoverPanicWithCallback(logger, "callback panic test", func(p interface{}) {
			panic("panic in callback")
		})
		panic("original panic")
	}()

	logOutput := buf.String()
	if !strings.Contains(logOutput, "panic recovered") {
		t.Errorf("Expected 'panic recovered' in log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "panic in panic handler") {
		t.Errorf("Expected 'panic in panic handler' in log, got: %s", logOutput)
	}
}

func TestNoPanic(t *testing.T) {
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.DebugLevel)
	logger := zap.New(core)

	// Test that normal execution doesn't log anything
	func() {
		defer RecoverPanic(logger, "no panic")
		// Normal execution
	}()

	logOutput := buf.String()
	if strings.Contains(logOutput, "panic recovered") {
		t.Errorf("Expected no panic log, but got: %s", logOutput)
	}
}
