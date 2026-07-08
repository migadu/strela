package dkim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// generateTestKeyPEM creates a PKCS#1 PEM-encoded RSA private key for tests.
func generateTestKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return string(pem.EncodeToMemory(block))
}

// newTestKeyStore builds an rspamd-style key directory layout:
// dir/<selector>/<domain>.key files plus a selectors.map.
func newTestKeyStore(t *testing.T, keys map[string]string, mapLines []string, fallbackSelector string) (*KeyStore, string) {
	t.Helper()
	dir := t.TempDir()

	keyPEM := generateTestKeyPEM(t)
	for domain, selector := range keys {
		selDir := filepath.Join(dir, selector)
		if err := os.MkdirAll(selDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(selDir, domain+".key"), []byte(keyPEM), 0o644); err != nil {
			t.Fatalf("write key: %v", err)
		}
	}

	mapPath := ""
	if mapLines != nil {
		mapPath = filepath.Join(dir, "selectors.map")
		if err := os.WriteFile(mapPath, []byte(strings.Join(mapLines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write map: %v", err)
		}
	}

	ks, err := NewKeyStore(dir, mapPath, fallbackSelector, 0, slog.Default())
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	return ks, dir
}

func TestKeyStoreLookupViaSelectorMap(t *testing.T) {
	ks, _ := newTestKeyStore(t,
		map[string]string{"example.com": "key1"},
		[]string{"example.com key1"},
		"")

	keyPEM, selector, found := ks.Lookup("example.com")
	if !found {
		t.Fatal("expected key to be found")
	}
	if selector != "key1" {
		t.Errorf("expected selector key1, got %q", selector)
	}
	if !strings.Contains(keyPEM, "RSA PRIVATE KEY") {
		t.Error("expected PEM key content")
	}
}

func TestKeyStoreLookupIsCaseInsensitive(t *testing.T) {
	ks, _ := newTestKeyStore(t,
		map[string]string{"example.com": "key1"},
		[]string{"EXAMPLE.com key1"},
		"")

	if _, _, found := ks.Lookup("Example.COM"); !found {
		t.Fatal("expected case-insensitive lookup to find key")
	}
}

func TestKeyStoreFallbackSelector(t *testing.T) {
	ks, _ := newTestKeyStore(t,
		map[string]string{"fallback.org": "default"},
		[]string{"other.com key1"},
		"default")

	_, selector, found := ks.Lookup("fallback.org")
	if !found {
		t.Fatal("expected key via fallback selector")
	}
	if selector != "default" {
		t.Errorf("expected selector default, got %q", selector)
	}
}

func TestKeyStoreUnknownDomain(t *testing.T) {
	ks, _ := newTestKeyStore(t,
		map[string]string{"example.com": "key1"},
		[]string{"example.com key1"},
		"")

	if _, _, found := ks.Lookup("unknown.com"); found {
		t.Fatal("expected no key for unknown domain")
	}
}

func TestKeyStoreDomainInMapButFileMissing(t *testing.T) {
	ks, _ := newTestKeyStore(t,
		map[string]string{"example.com": "key1"},
		[]string{"example.com key1", "missing.com key1"},
		"")

	if _, _, found := ks.Lookup("missing.com"); found {
		t.Fatal("expected no key when file is missing")
	}
	// Second lookup exercises the negative cache path.
	if _, _, found := ks.Lookup("missing.com"); found {
		t.Fatal("expected negative cache to also report not found")
	}
}

func TestKeyStoreInvalidKeyFile(t *testing.T) {
	ks, dir := newTestKeyStore(t,
		map[string]string{"example.com": "key1"},
		[]string{"example.com key1", "broken.com key1"},
		"")

	if err := os.WriteFile(filepath.Join(dir, "key1", "broken.com.key"), []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("write broken key: %v", err)
	}

	if _, _, found := ks.Lookup("broken.com"); found {
		t.Fatal("expected unparseable key to be treated as not found")
	}
}

func TestKeyStoreRejectsPathTraversal(t *testing.T) {
	ks, _ := newTestKeyStore(t,
		map[string]string{"example.com": "key1"},
		[]string{"example.com key1"},
		"key1")

	for _, domain := range []string{"../key1/example.com", "a/b.com", `a\b.com`, "..", ""} {
		if _, _, found := ks.Lookup(domain); found {
			t.Errorf("expected lookup of %q to be rejected", domain)
		}
	}
}

func TestKeyStoreSelectorMapReload(t *testing.T) {
	ks, dir := newTestKeyStore(t,
		map[string]string{"example.com": "key1"},
		[]string{"example.com key1"},
		"")

	// Also create the key under key2 so the domain resolves after the map moves it.
	keyPEM := generateTestKeyPEM(t)
	if err := os.MkdirAll(filepath.Join(dir, "key2"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key2", "example.com.key"), []byte(keyPEM), 0o644); err != nil {
		t.Fatalf("write key: %v", err)
	}

	if _, selector, _ := ks.Lookup("example.com"); selector != "key1" {
		t.Fatalf("expected selector key1 before reload, got %q", selector)
	}

	// Rewrite the map with a newer mtime and force the throttle window open.
	mapPath := filepath.Join(dir, "selectors.map")
	if err := os.WriteFile(mapPath, []byte("example.com key2\n"), 0o644); err != nil {
		t.Fatalf("rewrite map: %v", err)
	}
	newTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(mapPath, newTime, newTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ks.mapMu.Lock()
	ks.nextMapCheck = time.Time{}
	ks.mapMu.Unlock()

	_, selector, found := ks.Lookup("example.com")
	if !found {
		t.Fatal("expected key after reload")
	}
	if selector != "key2" {
		t.Errorf("expected selector key2 after reload, got %q", selector)
	}
}

func TestKeyStoreMapParsingSkipsCommentsAndBlanks(t *testing.T) {
	ks, _ := newTestKeyStore(t,
		map[string]string{"example.com": "key1"},
		[]string{"# comment", "", "malformed-line", "example.com key1"},
		"")

	if _, _, found := ks.Lookup("example.com"); !found {
		t.Fatal("expected key despite comments and malformed lines in map")
	}
	if _, _, found := ks.Lookup("malformed-line"); found {
		t.Fatal("malformed map line must not produce an entry")
	}
}

func TestNewKeyStoreErrors(t *testing.T) {
	if _, err := NewKeyStore("/nonexistent-dir-for-test", "", "default", 0, slog.Default()); err == nil {
		t.Error("expected error for missing key directory")
	}

	dir := t.TempDir()
	if _, err := NewKeyStore(dir, filepath.Join(dir, "no-such.map"), "default", 0, slog.Default()); err == nil {
		t.Error("expected error for missing selector map")
	}
}

func TestKeyStoreCacheEviction(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "key1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	keyPEM := generateTestKeyPEM(t)
	domains := []string{"a.com", "b.com", "c.com", "d.com"}
	var mapLines []string
	for _, domain := range domains {
		if err := os.WriteFile(filepath.Join(dir, "key1", domain+".key"), []byte(keyPEM), 0o644); err != nil {
			t.Fatalf("write key: %v", err)
		}
		mapLines = append(mapLines, domain+" key1")
	}
	mapPath := filepath.Join(dir, "selectors.map")
	if err := os.WriteFile(mapPath, []byte(strings.Join(mapLines, "\n")), 0o644); err != nil {
		t.Fatalf("write map: %v", err)
	}

	ks, err := NewKeyStore(dir, mapPath, "", 2, slog.Default())
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}

	// Fill past the cap; every lookup must still succeed.
	for _, domain := range domains {
		if _, _, found := ks.Lookup(domain); !found {
			t.Errorf("expected key for %s", domain)
		}
	}

	ks.cacheMu.Lock()
	size := len(ks.cache)
	ks.cacheMu.Unlock()
	if size > 2 {
		t.Errorf("expected cache size <= 2 after eviction, got %d", size)
	}
}

func TestKeyStoreEvictionPrefersNegativeEntries(t *testing.T) {
	ks, _ := newTestKeyStore(t,
		map[string]string{"hot.com": "key1"},
		[]string{"hot.com key1"},
		"default")
	ks.cacheSize = 3

	if _, _, found := ks.Lookup("hot.com"); !found {
		t.Fatal("expected key for hot.com")
	}

	// Burst of unknown domains (negative entries via the fallback selector)
	// overflows the cache; the positive entry must survive.
	for _, domain := range []string{"u1.com", "u2.com", "u3.com", "u4.com"} {
		if _, _, found := ks.Lookup(domain); found {
			t.Errorf("expected no key for %s", domain)
		}
	}

	ks.cacheMu.Lock()
	entry, ok := ks.cache["key1/hot.com"]
	ks.cacheMu.Unlock()
	if !ok || !entry.found {
		t.Error("expected positive entry for hot.com to survive negative-entry eviction")
	}
}

func TestExtractFromHeaderDomain(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "simple from header",
			message: "From: user@Example.COM\r\nTo: rcpt@other.com\r\nSubject: hi\r\n\r\nbody\r\n",
			want:    "example.com",
		},
		{
			name:    "display name",
			message: "From: \"Some User\" <user@example.com>\r\nSubject: hi\r\n\r\nbody\r\n",
			want:    "example.com",
		},
		{
			name:    "missing from header",
			message: "To: rcpt@other.com\r\nSubject: hi\r\n\r\nbody\r\n",
			want:    "",
		},
		{
			name:    "not a message",
			message: "garbage",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractFromHeaderDomain([]byte(tt.message)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
