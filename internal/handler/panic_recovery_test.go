package handler

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPanicRecoveryMiddleware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	tests := []struct {
		name           string
		handler        http.HandlerFunc
		expectPanic    bool
		expectedStatus int
		expectedBody   string
	}{
		{
			name: "normal handler execution",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"status":"ok"}`))
			},
			expectPanic:    false,
			expectedStatus: http.StatusOK,
			expectedBody:   `{"status":"ok"}`,
		},
		{
			name: "panic with string",
			handler: func(w http.ResponseWriter, r *http.Request) {
				panic("test panic message")
			},
			expectPanic:    true,
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   "Internal Server Error",
		},
		{
			name: "panic with error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				panic(http.ErrAbortHandler)
			},
			expectPanic:    true,
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   "Internal Server Error",
		},
		{
			name: "panic with nil",
			handler: func(w http.ResponseWriter, r *http.Request) {
				var ptr *int
				_ = *ptr // nil pointer dereference
			},
			expectPanic:    true,
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   "Internal Server Error",
		},
		{
			name: "panic after partial response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("partial"))
				panic("panic after write")
			},
			expectPanic: true,
			// Status code can't be changed after WriteHeader is called
			// The panic is still recovered, but HTTP status remains 200
			expectedStatus: http.StatusOK,
			expectedBody:   "partial",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Wrap handler with panic recovery
			wrappedHandler := PanicRecoveryMiddleware(tt.handler, logger)

			req := httptest.NewRequest(http.MethodPost, "/test", nil)
			rec := httptest.NewRecorder()

			// Execute handler (should not panic due to middleware)
			wrappedHandler.ServeHTTP(rec, req)

			// Check status code
			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rec.Code)
			}

			// Check response body
			body := strings.TrimSpace(rec.Body.String())
			if !strings.Contains(body, tt.expectedBody) {
				t.Errorf("expected body to contain %q, got %q", tt.expectedBody, body)
			}
		})
	}
}

func TestPanicRecoveryMiddleware_DoesNotPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("this should be recovered")
	})

	wrappedHandler := PanicRecoveryMiddleware(panicHandler, logger)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	// This should NOT panic - if it does, the test will fail
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic was not recovered by middleware: %v", r)
		}
	}()

	wrappedHandler.ServeHTTP(rec, req)

	// Should return 500
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}
}

func TestPanicRecoveryMiddleware_PreservesNormalErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	errorHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})

	wrappedHandler := PanicRecoveryMiddleware(errorHandler, logger)

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	rec := httptest.NewRecorder()

	wrappedHandler.ServeHTTP(rec, req)

	// Should preserve the 400 error
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	body := strings.TrimSpace(rec.Body.String())
	if !strings.Contains(body, "bad request") {
		t.Errorf("expected body to contain 'bad request', got %q", body)
	}
}
