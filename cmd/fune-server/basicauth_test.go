package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBasicAuthMiddleware_ValidCredentials(t *testing.T) {
	logger := slog.Default()
	called := false

	// Create test handler that sets a flag when called
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Wrap with Basic Auth
	authHandler := basicAuthMiddleware(testHandler, "admin", "secret", logger)

	// Create test request with valid credentials
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.SetBasicAuth("admin", "secret")
	w := httptest.NewRecorder()

	// Execute request
	authHandler.ServeHTTP(w, req)

	// Verify handler was called and response is OK
	if !called {
		t.Error("Handler should have been called with valid credentials")
	}
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
}

func TestBasicAuthMiddleware_InvalidCredentials(t *testing.T) {
	logger := slog.Default()
	called := false

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	authHandler := basicAuthMiddleware(testHandler, "admin", "secret", logger)

	// Test with wrong password
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.SetBasicAuth("admin", "wrongpassword")
	w := httptest.NewRecorder()

	authHandler.ServeHTTP(w, req)

	if called {
		t.Error("Handler should not have been called with invalid credentials")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") != `Basic realm="Metrics"` {
		t.Errorf("Expected WWW-Authenticate header, got %s", w.Header().Get("WWW-Authenticate"))
	}
}

func TestBasicAuthMiddleware_NoCredentials(t *testing.T) {
	logger := slog.Default()
	called := false

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	authHandler := basicAuthMiddleware(testHandler, "admin", "secret", logger)

	// Test without credentials
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()

	authHandler.ServeHTTP(w, req)

	if called {
		t.Error("Handler should not have been called without credentials")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}
}

func TestBasicAuthMiddleware_WrongUsername(t *testing.T) {
	logger := slog.Default()
	called := false

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	authHandler := basicAuthMiddleware(testHandler, "admin", "secret", logger)

	// Test with wrong username
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.SetBasicAuth("wronguser", "secret")
	w := httptest.NewRecorder()

	authHandler.ServeHTTP(w, req)

	if called {
		t.Error("Handler should not have been called with wrong username")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}
}
