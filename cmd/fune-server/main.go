package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/handler"
	"fune/internal/metrics"
	"fune/internal/recovery"
	tlsmanager "fune/internal/tls"

	"log/slog"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func initLogger(logCfg *config.LoggingConfig) (*slog.Logger, error) {
	return config.NewLogger(*logCfg)
}

func basicAuthMiddleware(next http.Handler, username, password string, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			logger.Warn("metrics endpoint unauthorized access attempt",
				"remote_addr", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Basic realm="Metrics"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cmdVersion() {
	fmt.Printf("fune-server %s\n", version)
	fmt.Printf("  commit: %s\n", commit)
	fmt.Printf("  built:  %s\n", date)
}

func main() {
	configPath := flag.String("config", "config.toml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Show version information and exit")
	flag.Parse()

	if *showVersion {
		cmdVersion()
		os.Exit(0)
	}

	// Load config
	tempCfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	tempCfg.SetDefaults()

	logger, err := initLogger(&tempCfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}

	logger.Info("starting fune SMTP delivery service", "version", version, "config", *configPath)

	// Load reloadable config
	reloadableCfg, err := config.NewReloadableConfig(*configPath, logger)
	if err != nil {
		logger.Error("failed to load reloadable config", "error", err)
		os.Exit(1)
	}
	cfg := reloadableCfg.Get()

	// Metrics
	var m *metrics.Metrics
	if cfg.Metrics.Enabled {
		m = metrics.NewMetrics()
		logger.Info("metrics initialized", "path", cfg.Metrics.Path)
	}

	// Expand source IPs (CIDR subnets + backwards compatibility)
	expandedIPs, err := expandSourceIPsFromConfig(&cfg.Outbound, logger)
	if err != nil {
		logger.Error("failed to expand source IPs", "error", err)
		os.Exit(1)
	}

	logger.Info("source IPs configured",
		"ipv4_count", len(expandedIPs.IPv4),
		"ipv6_count", len(expandedIPs.IPv6),
		"prefer_ipv6", cfg.Outbound.PreferIPv6)

	// Delivery
	mxLookup := delivery.NewMXLookup(&cfg.DNS, logger)
	deliverer := delivery.NewDeliverer(&cfg.Outbound, expandedIPs, mxLookup, logger, &cfg.Reputation, &cfg.ARC, &cfg.SRS)
	if m != nil {
		deliverer.SetMetrics(m)
	}

	// Handler
	h := handler.NewHandler(&cfg, deliverer, logger)

	// Router
	mux := http.NewServeMux()

	// Routes
	var apiHandler http.Handler = http.HandlerFunc(h.HandleDeliver)

	if cfg.Inbound.MaxConcurrentRequests > 0 {
		apiHandler = handler.ConcurrencyLimitMiddleware(cfg.Inbound.MaxConcurrentRequests)(apiHandler)
	}
	var rateLimiter *handler.RateLimiter
	if cfg.Inbound.RateLimitEnabled {
		rateLimiter = handler.NewRateLimiter(cfg.Inbound.RateLimitRequestsPerIP, cfg.Inbound.RateLimitWindowSeconds, logger)
		apiHandler = rateLimiter.Middleware(apiHandler)
	}
	if m != nil {
		apiHandler = handler.MetricsMiddleware(apiHandler, m)
	}
	apiHandler = handler.SecurityHeadersMiddleware(apiHandler)
	// CRITICAL: Panic recovery must be the outermost middleware to catch all panics
	apiHandler = handler.PanicRecoveryMiddleware(apiHandler, logger)

	mux.Handle("/deliver", apiHandler)

	// Metrics Endpoint
	if m != nil && cfg.Metrics.Path != "" {
		metricsHandler := promhttp.Handler()
		if cfg.Metrics.Username != "" && cfg.Metrics.Password != "" {
			metricsHandler = basicAuthMiddleware(metricsHandler, cfg.Metrics.Username, cfg.Metrics.Password, logger)
		}
		mux.Handle(cfg.Metrics.Path, metricsHandler)
	}

	// Health Endpoint
	if cfg.Health.Enabled {
		healthHandler := handler.NewHealthHandler(deliverer, logger)
		var finalHealthHandler http.Handler = healthHandler
		if cfg.Health.Username != "" && cfg.Health.Password != "" {
			finalHealthHandler = basicAuthMiddleware(healthHandler, cfg.Health.Username, cfg.Health.Password, logger)
		}
		// Add panic recovery to health endpoint
		finalHealthHandler = handler.PanicRecoveryMiddleware(finalHealthHandler, logger)
		mux.Handle("/health", finalHealthHandler)
	}

	// TLS Manager (if ACME enabled)
	var tlsManager *tlsmanager.Manager
	if cfg.TLS.Enabled {
		// Initialize TLS with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tlsManager, err = tlsmanager.NewManager(ctx, &cfg.TLS, nil, logger) // No gossip
		cancel()
		if err != nil {
			logger.Error("failed to initialize TLS manager", "error", err)
			os.Exit(1)
		}
	}

	server := &http.Server{
		Addr:         cfg.Inbound.Listen,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Inbound.ReadTimeoutSecs) * time.Second,
		WriteTimeout: time.Duration(cfg.Inbound.WriteTimeoutSecs) * time.Second,
		IdleTimeout:  time.Duration(cfg.Inbound.IdleTimeoutSecs) * time.Second,
	}

	// Start server with panic recovery
	recovery.SafeGo(logger, "http-server", func() {
		logger.Info("HTTP server starting", "addr", cfg.Inbound.Listen)
		var err error
		if cfg.TLS.Enabled && cfg.TLS.Provider == "letsencrypt" && tlsManager != nil {
			server.TLSConfig = tlsManager.TLSConfig()
			err = server.ListenAndServeTLS("", "")
		} else if cfg.TLS.Enabled && cfg.TLS.Provider == "file" {
			err = server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	})

	// Register reload callbacks
	reloadableCfg.RegisterReloadCallback(func(newCfg *config.Config) error {
		//mxLookup.ReloadConfig(&newCfg.DNS) // signature changed
		//deliverer.ReloadConfig(&newCfg.Outbound)
		// No easy way to update handler config without locking in handler.
		return nil
	})

	// Signal handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down gracefully...")

	// 1. Stop accepting new HTTP requests
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
	logger.Info("HTTP server stopped")

	// 2. Stop rate limiter cleanup goroutine
	if rateLimiter != nil {
		rateLimiter.Stop()
		logger.Info("rate limiter stopped")
	}

	// 3. Stop deliverer (stops all background cleanup goroutines, closes connection pool)
	deliverer.Stop()

	logger.Info("graceful shutdown complete")
}

// expandSourceIPsFromConfig expands source IPs from config.
func expandSourceIPsFromConfig(cfg *config.OutboundConfig, logger *slog.Logger) (*config.ExpandedSourceIPs, error) {
	// Expand IPv4 and IPv6 separately
	expandedV4, err := config.ExpandSourceIPs(cfg.SourceIPsV4)
	if err != nil {
		return nil, fmt.Errorf("failed to expand source_ips_v4: %w", err)
	}

	expandedV6, err := config.ExpandSourceIPs(cfg.SourceIPsV6)
	if err != nil {
		return nil, fmt.Errorf("failed to expand source_ips_v6: %w", err)
	}

	// Combine results (they should only contain their respective IP versions)
	result := &config.ExpandedSourceIPs{
		IPv4: expandedV4.IPv4,
		IPv6: expandedV6.IPv6,
	}

	// Validate: ensure no IPv6 in v4 list and vice versa
	if len(expandedV4.IPv6) > 0 {
		logger.Warn("IPv6 addresses found in source_ips_v4, they will be ignored", "count", len(expandedV4.IPv6))
	}
	if len(expandedV6.IPv4) > 0 {
		logger.Warn("IPv4 addresses found in source_ips_v6, they will be ignored", "count", len(expandedV6.IPv4))
	}

	return result, nil
}
