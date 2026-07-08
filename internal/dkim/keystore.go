package dkim

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultKeyCacheSize  = 20000
	positiveCacheTTL     = 1 * time.Hour
	negativeCacheTTL     = 5 * time.Minute
	selectorMapCheckIntv = 1 * time.Minute
)

// KeyStore provides per-domain DKIM keys from an rspamd-style key directory:
// $key_directory/$selector/$domain.key, with a selector map file listing
// "domain selector" per line. Key files are loaded lazily on first use and
// cached with a TTL; the selector map is reloaded automatically when its
// mtime changes (checked at most once per minute).
type KeyStore struct {
	keyDir           string
	selectorMapPath  string
	fallbackSelector string
	cacheSize        int
	logger           *slog.Logger

	mapMu        sync.RWMutex
	selectors    map[string]string
	mapModTime   time.Time
	nextMapCheck time.Time

	cacheMu sync.Mutex
	cache   map[string]keyCacheEntry
}

type keyCacheEntry struct {
	keyPEM  string
	found   bool
	expires time.Time
}

// NewKeyStore creates a KeyStore rooted at keyDir. selectorMapPath may be
// empty, in which case every domain uses fallbackSelector. fallbackSelector
// may be empty, in which case domains missing from the selector map are not
// signed. Returns an error if keyDir is missing or the selector map cannot
// be loaded.
func NewKeyStore(keyDir, selectorMapPath, fallbackSelector string, cacheSize int, logger *slog.Logger) (*KeyStore, error) {
	info, err := os.Stat(keyDir)
	if err != nil {
		return nil, fmt.Errorf("DKIM key directory not accessible: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("DKIM key directory is not a directory: %s", keyDir)
	}

	if cacheSize <= 0 {
		cacheSize = defaultKeyCacheSize
	}

	ks := &KeyStore{
		keyDir:           keyDir,
		selectorMapPath:  selectorMapPath,
		fallbackSelector: fallbackSelector,
		cacheSize:        cacheSize,
		logger:           logger,
		selectors:        map[string]string{},
		cache:            map[string]keyCacheEntry{},
	}

	if selectorMapPath != "" {
		if err := ks.reloadSelectorMap(); err != nil {
			return nil, err
		}
	}

	return ks, nil
}

// Lookup returns the PEM private key and selector for a domain, or found=false
// if the domain has no key. Lookups never fail hard: I/O errors are logged and
// treated as "no key" so delivery proceeds unsigned.
func (ks *KeyStore) Lookup(domain string) (keyPEM, selector string, found bool) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" || !safePathComponent(domain) {
		return "", "", false
	}

	ks.maybeReloadSelectorMap()

	ks.mapMu.RLock()
	selector = ks.selectors[domain]
	ks.mapMu.RUnlock()
	if selector == "" {
		selector = ks.fallbackSelector
	}
	if selector == "" || !safePathComponent(selector) {
		return "", "", false
	}

	cacheKey := selector + "/" + domain
	now := time.Now()

	ks.cacheMu.Lock()
	if entry, ok := ks.cache[cacheKey]; ok && now.Before(entry.expires) {
		ks.cacheMu.Unlock()
		return entry.keyPEM, selector, entry.found
	}
	ks.cacheMu.Unlock()

	keyPath := filepath.Join(ks.keyDir, selector, domain+".key")
	data, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			ks.logger.Warn("failed to read DKIM key file", "path", keyPath, "error", err)
		}
		ks.storeCacheEntry(cacheKey, keyCacheEntry{expires: now.Add(negativeCacheTTL)})
		return "", "", false
	}

	// Reject unparseable keys here so signing failures surface at lookup time.
	if _, err := parsePrivateKey(string(data)); err != nil {
		ks.logger.Warn("invalid DKIM key file", "path", keyPath, "error", err)
		ks.storeCacheEntry(cacheKey, keyCacheEntry{expires: now.Add(negativeCacheTTL)})
		return "", "", false
	}

	keyPEM = string(data)
	ks.storeCacheEntry(cacheKey, keyCacheEntry{keyPEM: keyPEM, found: true, expires: now.Add(positiveCacheTTL)})
	return keyPEM, selector, true
}

// storeCacheEntry inserts a cache entry, evicting expired, then negative,
// then arbitrary entries when the cache is full.
func (ks *KeyStore) storeCacheEntry(key string, entry keyCacheEntry) {
	ks.cacheMu.Lock()
	defer ks.cacheMu.Unlock()

	if len(ks.cache) >= ks.cacheSize {
		now := time.Now()
		for k, e := range ks.cache {
			if now.After(e.expires) {
				delete(ks.cache, k)
			}
		}
		// Still full: drop negative entries before positive ones so a burst
		// of unknown From domains cannot evict hot signing keys.
		for k, e := range ks.cache {
			if len(ks.cache) < ks.cacheSize {
				break
			}
			if !e.found {
				delete(ks.cache, k)
			}
		}
		// Still full: drop arbitrary entries (map iteration order is random).
		for k := range ks.cache {
			if len(ks.cache) < ks.cacheSize {
				break
			}
			delete(ks.cache, k)
		}
	}

	ks.cache[key] = entry
}

// maybeReloadSelectorMap re-reads the selector map if its mtime changed,
// checking the file at most once per selectorMapCheckIntv.
func (ks *KeyStore) maybeReloadSelectorMap() {
	if ks.selectorMapPath == "" {
		return
	}

	now := time.Now()
	ks.mapMu.RLock()
	due := now.After(ks.nextMapCheck)
	ks.mapMu.RUnlock()
	if !due {
		return
	}

	ks.mapMu.Lock()
	if !now.After(ks.nextMapCheck) { // another goroutine won the race
		ks.mapMu.Unlock()
		return
	}
	ks.nextMapCheck = now.Add(selectorMapCheckIntv)
	modTime := ks.mapModTime
	ks.mapMu.Unlock()

	info, err := os.Stat(ks.selectorMapPath)
	if err != nil {
		ks.logger.Warn("failed to stat DKIM selector map", "path", ks.selectorMapPath, "error", err)
		return
	}
	if info.ModTime().Equal(modTime) {
		return
	}

	if err := ks.reloadSelectorMap(); err != nil {
		// Keep serving the previous map on reload failure.
		ks.logger.Error("failed to reload DKIM selector map", "path", ks.selectorMapPath, "error", err)
	}
}

// reloadSelectorMap parses the selector map file and swaps it in atomically.
// Lines are "domain selector" separated by whitespace; blank lines and
// #-comments are ignored (rspamd map format).
func (ks *KeyStore) reloadSelectorMap() error {
	f, err := os.Open(ks.selectorMapPath)
	if err != nil {
		return fmt.Errorf("failed to open DKIM selector map: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat DKIM selector map: %w", err)
	}

	selectors := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		selectors[strings.ToLower(fields[0])] = fields[1]
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read DKIM selector map: %w", err)
	}

	ks.mapMu.Lock()
	ks.selectors = selectors
	ks.mapModTime = info.ModTime()
	ks.mapMu.Unlock()

	ks.logger.Info("DKIM selector map loaded", "path", ks.selectorMapPath, "domains", len(selectors))
	return nil
}

// safePathComponent reports whether s is safe to use as a single path element
// (no separators or parent-directory references).
func safePathComponent(s string) bool {
	return s != "" && !strings.ContainsAny(s, "/\\") && !strings.Contains(s, "..")
}

// ExtractFromHeaderDomain returns the lowercased domain of the first address
// in the message's From header, or "" if the header is missing or unparseable.
// DKIM signing domains are keyed by the From header (DMARC alignment), not the
// envelope sender.
func ExtractFromHeaderDomain(message []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(message))
	if err != nil {
		return ""
	}
	addrs, err := msg.Header.AddressList("From")
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return ExtractDomainFromEmail(addrs[0].Address)
}
