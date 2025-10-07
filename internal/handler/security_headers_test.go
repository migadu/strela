package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeadersMiddleware(t *testing.T) {
	// Create a test handler that just returns 200 OK
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Wrap with security headers middleware
	handler := SecurityHeadersMiddleware(testHandler)

	// Create test request
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	// Execute request
	handler.ServeHTTP(rec, req)

	// Verify response
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	// Verify all security headers are present
	expectedHeaders := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"X-XSS-Protection":        "1; mode=block",
		"Cache-Control":           "no-store, no-cache, must-revalidate, private",
		"Pragma":                  "no-cache",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
	}

	for header, expectedValue := range expectedHeaders {
		actualValue := rec.Header().Get(header)
		if actualValue != expectedValue {
			t.Errorf("header %s: expected %q, got %q", header, expectedValue, actualValue)
		}
	}
}

func TestSecurityHeadersMiddleware_DoesNotInterfereWithHandler(t *testing.T) {
	// Create a test handler that sets custom headers
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Custom-Header", "custom-value")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Wrap with security headers middleware
	handler := SecurityHeadersMiddleware(testHandler)

	// Create test request
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()

	// Execute request
	handler.ServeHTTP(rec, req)

	// Verify handler's custom headers are preserved
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}

	if rec.Header().Get("Custom-Header") != "custom-value" {
		t.Error("custom header was not preserved")
	}

	if rec.Header().Get("Content-Type") != "application/json" {
		t.Error("content-type header was not preserved")
	}

	// Verify security headers are still present
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security header was not set")
	}

	// Verify response body
	if rec.Body.String() != `{"status":"ok"}` {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}
