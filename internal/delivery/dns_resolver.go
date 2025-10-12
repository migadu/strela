package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"fune/internal/config"
)

// DNSResolver handles DNS queries with custom resolvers, round-robin distribution,
// and UDP-to-TCP fallback. It supports multiple DNS servers for redundancy and
// automatically falls back to TCP when UDP responses are truncated. The resolver
// uses an atomic counter for thread-safe round-robin selection across configured
// DNS servers.
type DNSResolver struct {
	resolvers  []string
	timeout    time.Duration
	logger     *slog.Logger
	currentIdx atomic.Uint32 // Round-robin counter for resolver selection
}

// NewDNSResolver creates a new DNS resolver with custom configuration.
// If no custom resolvers are specified in the config, the system's default resolver
// will be used. The timeout applies to individual DNS queries.
func NewDNSResolver(cfg *config.DNSConfig, logger *slog.Logger) *DNSResolver {
	return &DNSResolver{
		resolvers: cfg.Resolvers,
		timeout:   time.Duration(cfg.TimeoutSeconds) * time.Second,
		logger:    logger,
	}
}

// LookupMX performs MX record lookup with timeout and custom resolver support.
// It uses a round-robin strategy to distribute queries across configured DNS servers,
// trying each server with UDP first, then falling back to TCP if needed. If custom
// resolvers are not configured, it uses the system's default resolver. The method
// is context-aware and respects the configured timeout.
func (d *DNSResolver) LookupMX(ctx context.Context, domain string) ([]*net.MX, error) {
	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	var resolver *net.Resolver

	// Use custom resolvers if specified
	if len(d.resolvers) > 0 {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				// Get starting index using round-robin
				startIdx := int(d.currentIdx.Add(1) % uint32(len(d.resolvers)))

				// Try each resolver starting from round-robin position, with UDP first then TCP fallback
				var lastErr error
				for i := 0; i < len(d.resolvers); i++ {
					idx := (startIdx + i) % len(d.resolvers)
					customResolver := d.resolvers[idx]

					d.logger.Debug("attempting DNS resolver",
						"resolver", customResolver,
						"domain", domain,
						"resolver_index", idx,
						"network", network)

					dialer := &net.Dialer{
						Timeout: d.timeout,
					}

					// Try UDP first (faster, lower overhead)
					conn, err := dialer.DialContext(ctx, "udp", customResolver)
					if err != nil {
						// UDP failed, try TCP
						d.logger.Debug("UDP DNS failed, trying TCP",
							"resolver", customResolver,
							"error", err)

						conn, err = dialer.DialContext(ctx, "tcp", customResolver)
						if err != nil {
							d.logger.Warn("DNS resolver failed (both UDP and TCP)",
								"resolver", customResolver,
								"resolver_index", idx,
								"error", err)
							lastErr = err
							continue
						}

						d.logger.Debug("connected to DNS resolver via TCP",
							"resolver", customResolver,
							"resolver_index", idx)
						return conn, nil
					}

					d.logger.Debug("connected to DNS resolver via UDP",
						"resolver", customResolver,
						"resolver_index", idx)
					return conn, nil
				}

				return nil, fmt.Errorf("all DNS resolvers failed, last error: %w", lastErr)
			},
		}
	} else {
		// Use system default resolver
		resolver = net.DefaultResolver
	}

	// Perform MX lookup
	startTime := time.Now()
	mxRecords, err := resolver.LookupMX(timeoutCtx, domain)
	duration := time.Since(startTime)

	if err != nil {
		d.logger.Error("MX lookup failed",
			"domain", domain,
			"duration", duration,
			"error", err)
		return nil, fmt.Errorf("MX lookup failed: %w", err)
	}

	d.logger.Debug("MX lookup successful",
		"domain", domain,
		"records", len(mxRecords),
		"duration", duration)

	return mxRecords, nil
}

// LookupHost performs A/AAAA record lookup with timeout and custom resolver support.
// Similar to LookupMX, it uses round-robin distribution across DNS servers with UDP-to-TCP
// fallback. Returns a list of IP addresses (both IPv4 and IPv6) for the given hostname.
// This method is context-aware and respects the configured timeout.
func (d *DNSResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	var resolver *net.Resolver
	if len(d.resolvers) > 0 {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				// Get starting index using round-robin
				startIdx := int(d.currentIdx.Add(1) % uint32(len(d.resolvers)))

				var lastErr error
				for i := 0; i < len(d.resolvers); i++ {
					idx := (startIdx + i) % len(d.resolvers)
					customResolver := d.resolvers[idx]
					dialer := &net.Dialer{Timeout: d.timeout}

					d.logger.Debug("attempting DNS resolver for host lookup",
						"resolver", customResolver,
						"host", host,
						"resolver_index", idx)

					// Try UDP first, then TCP fallback
					conn, err := dialer.DialContext(ctx, "udp", customResolver)
					if err != nil {
						conn, err = dialer.DialContext(ctx, "tcp", customResolver)
						if err != nil {
							d.logger.Debug("DNS resolver failed for host lookup",
								"resolver", customResolver,
								"resolver_index", idx,
								"error", err)
							lastErr = err
							continue
						}
						return conn, nil
					}
					return conn, nil
				}
				return nil, fmt.Errorf("all DNS resolvers failed: %w", lastErr)
			},
		}
	} else {
		resolver = net.DefaultResolver
	}

	addrs, err := resolver.LookupHost(timeoutCtx, host)
	if err != nil {
		return nil, fmt.Errorf("host lookup failed: %w", err)
	}

	return addrs, nil
}
