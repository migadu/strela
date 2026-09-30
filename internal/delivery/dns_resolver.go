package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"strela/internal/config"
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

// netResolver returns the resolver to use for a lookup of name: a Go resolver
// that dials the configured DNS servers, or the system's default resolver when
// no custom resolvers are configured.
func (d *DNSResolver) netResolver(name string) *net.Resolver {
	if len(d.resolvers) == 0 {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.dialResolver(ctx, network, name)
		},
	}
}

// dialResolver connects to one of the configured DNS servers, starting from the
// round-robin position and moving on to the next server if one cannot be reached.
// UDP is tried first, with TCP as fallback if the UDP dial fails. When the Go
// resolver asks for TCP (it does so to retry a truncated UDP response), only TCP
// is dialed so the retry can fetch the full answer.
func (d *DNSResolver) dialResolver(ctx context.Context, network, name string) (net.Conn, error) {
	wantTCP := strings.HasPrefix(network, "tcp")

	// Get starting index using round-robin
	startIdx := int(d.currentIdx.Add(1) % uint32(len(d.resolvers)))

	var lastErr error
	for i := 0; i < len(d.resolvers); i++ {
		idx := (startIdx + i) % len(d.resolvers)
		customResolver := d.resolvers[idx]

		d.logger.Debug("attempting DNS resolver",
			"resolver", customResolver,
			"name", name,
			"resolver_index", idx,
			"network", network)

		dialer := &net.Dialer{
			Timeout: d.timeout,
		}

		if !wantTCP {
			// Try UDP first (faster, lower overhead)
			conn, err := dialer.DialContext(ctx, "udp", customResolver)
			if err == nil {
				d.logger.Debug("connected to DNS resolver via UDP",
					"resolver", customResolver,
					"resolver_index", idx)
				return conn, nil
			}

			d.logger.Debug("UDP DNS failed, trying TCP",
				"resolver", customResolver,
				"error", err)
		}

		conn, err := dialer.DialContext(ctx, "tcp", customResolver)
		if err != nil {
			d.logger.Warn("DNS resolver failed",
				"resolver", customResolver,
				"resolver_index", idx,
				"network", network,
				"error", err)
			lastErr = err
			continue
		}

		d.logger.Debug("connected to DNS resolver via TCP",
			"resolver", customResolver,
			"resolver_index", idx)
		return conn, nil
	}

	return nil, fmt.Errorf("all DNS resolvers failed, last error: %w", lastErr)
}

// resolverError corrects the server named in a DNS error. With custom resolvers
// the Go resolver still names a server from the system's resolv.conf in its
// errors, although the query was sent to one of the configured resolvers.
func (d *DNSResolver) resolverError(err error) error {
	var dnsErr *net.DNSError
	if len(d.resolvers) == 0 || !errors.As(err, &dnsErr) {
		return err
	}
	fixed := *dnsErr
	fixed.Server = strings.Join(d.resolvers, ",")
	return &fixed
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

	// Perform MX lookup
	startTime := time.Now()
	mxRecords, err := d.netResolver(domain).LookupMX(timeoutCtx, domain)
	duration := time.Since(startTime)

	if err != nil {
		err = d.resolverError(err)
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

	addrs, err := d.netResolver(host).LookupHost(timeoutCtx, host)
	if err != nil {
		return nil, fmt.Errorf("host lookup failed: %w", d.resolverError(err))
	}

	return addrs, nil
}

// LookupTXT performs TXT record lookup with timeout and custom resolver support.
// Like LookupMX and LookupHost, it goes through the configured DNS servers (or the
// system's default resolver when none are configured). It is used for DKIM record
// validation so that those lookups honor the [dns] resolvers setting.
func (d *DNSResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	records, err := d.netResolver(name).LookupTXT(timeoutCtx, name)
	if err != nil {
		return nil, fmt.Errorf("TXT lookup failed: %w", d.resolverError(err))
	}

	return records, nil
}
