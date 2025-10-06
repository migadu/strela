package delivery

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"fune/internal/config"

	"go.uber.org/zap"
)

// DNSResolver handles DNS queries with custom resolvers and caching
type DNSResolver struct {
	resolvers  []string
	timeout    time.Duration
	logger     *zap.Logger
	currentIdx atomic.Uint32 // Round-robin counter for resolver selection
}

// NewDNSResolver creates a new DNS resolver with custom configuration
func NewDNSResolver(cfg *config.DeliveryConfig, logger *zap.Logger) *DNSResolver {
	return &DNSResolver{
		resolvers: cfg.DNSResolvers,
		timeout:   time.Duration(cfg.DNSTimeoutSeconds) * time.Second,
		logger:    logger,
	}
}

// LookupMX performs MX record lookup with timeout
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
						zap.String("resolver", customResolver),
						zap.String("domain", domain),
						zap.Int("resolver_index", idx),
						zap.String("network", network))

					dialer := &net.Dialer{
						Timeout: d.timeout,
					}

					// Try UDP first (faster, lower overhead)
					conn, err := dialer.DialContext(ctx, "udp", customResolver)
					if err != nil {
						// UDP failed, try TCP
						d.logger.Debug("UDP DNS failed, trying TCP",
							zap.String("resolver", customResolver),
							zap.Error(err))

						conn, err = dialer.DialContext(ctx, "tcp", customResolver)
						if err != nil {
							d.logger.Warn("DNS resolver failed (both UDP and TCP)",
								zap.String("resolver", customResolver),
								zap.Int("resolver_index", idx),
								zap.Error(err))
							lastErr = err
							continue
						}

						d.logger.Debug("connected to DNS resolver via TCP",
							zap.String("resolver", customResolver),
							zap.Int("resolver_index", idx))
						return conn, nil
					}

					d.logger.Debug("connected to DNS resolver via UDP",
						zap.String("resolver", customResolver),
						zap.Int("resolver_index", idx))
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
			zap.String("domain", domain),
			zap.Duration("duration", duration),
			zap.Error(err))
		return nil, fmt.Errorf("MX lookup failed: %w", err)
	}

	d.logger.Debug("MX lookup successful",
		zap.String("domain", domain),
		zap.Int("records", len(mxRecords)),
		zap.Duration("duration", duration))

	return mxRecords, nil
}

// LookupHost performs A/AAAA record lookup with timeout (for future use)
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
						zap.String("resolver", customResolver),
						zap.String("host", host),
						zap.Int("resolver_index", idx))

					// Try UDP first, then TCP fallback
					conn, err := dialer.DialContext(ctx, "udp", customResolver)
					if err != nil {
						conn, err = dialer.DialContext(ctx, "tcp", customResolver)
						if err != nil {
							d.logger.Debug("DNS resolver failed for host lookup",
								zap.String("resolver", customResolver),
								zap.Int("resolver_index", idx),
								zap.Error(err))
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
