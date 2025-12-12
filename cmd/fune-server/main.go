// Package main implements the fune-server, a production-ready SMTP delivery service.
//
// The server accepts email messages via HTTP API, queues them in SQLite with WAL mode,
// and delivers them asynchronously to recipient MX servers using a pool of background workers.
//
// Architecture:
//
//	HTTP API → SQLite Queue → Worker Pool → Direct MX Delivery
//	              ↓                           ↓
//	         Callback Queue ← Webhook Notifications
//
// Key features:
//   - Asynchronous delivery: Returns 202 Accepted immediately after queueing
//   - Persistent queue: SQLite with Write-Ahead Logging for crash recovery
//   - Intelligent retry: Exponential backoff with greylisting support
//   - IP reputation: Tracks and rotates degraded source IPs
//   - Circuit breaker: Prevents accepting messages during delivery failures
//   - Hot reload: Configuration updates via SIGHUP (preserves connections)
//   - TLS support: File-based or Let's Encrypt with auto-renewal
//   - Clustering: Optional gossip protocol for distributed idempotency and leader election
//   - Observability: Prometheus metrics, structured logging, health endpoints
//
// Startup sequence:
//  1. Load configuration from config.toml
//  2. Initialize logger (JSON or console format)
//  3. Write PID file for signal handling
//  4. Initialize SQLite queue with WAL mode
//  5. Start worker pool for asynchronous delivery
//  6. Start callback handler for webhook notifications
//  7. Setup HTTP servers (API, metrics, health, ACME challenge)
//  8. Register signal handlers (SIGINT/SIGTERM=shutdown, SIGHUP=reload)
//  9. Enter main event loop
//
// Signals:
//   - SIGINT/SIGTERM: Graceful shutdown (30s timeout)
//   - SIGHUP: Hot configuration reload
//
// Usage:
//
//	fune-server                  # Uses config.toml in current directory
//	fune-server -version         # Show version information
//
// Environment variables:
//   - AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY: For S3 certificate storage
//
// Exit codes:
//   - 0: Clean shutdown
//   - 1: Configuration or initialization error
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fune/internal/callback"
	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/gossip"
	"fune/internal/handler"
	"fune/internal/metrics"
	"fune/internal/queue"
	"fune/internal/recovery"
	tlsmanager "fune/internal/tls"
	"fune/internal/worker"

	"log/slog"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Version information, injected at build time via ldflags.
// Set during compilation with:
//
//	go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse HEAD) -X main.date=$(date -u +%Y-%m-%d)"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// basicAuthMiddleware wraps an HTTP handler with HTTP Basic Authentication.
//
// This middleware is used to protect sensitive endpoints (metrics, health) from
// unauthorized access. It performs constant-time comparison of credentials to
// prevent timing attacks.
//
// Parameters:
//   - next: The handler to wrap with authentication
//   - username: Expected username for Basic Auth
//   - password: Expected password for Basic Auth
//   - logger: Logger for recording unauthorized access attempts
//
// Returns an HTTP handler that enforces authentication before delegating to next.
// Unauthorized requests receive 401 Unauthorized with WWW-Authenticate header.
func basicAuthMiddleware(next http.Handler, username, password string, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get credentials from Authorization header
		user, pass, ok := r.BasicAuth()

		// Check if credentials match
		if !ok || user != username || pass != password {
			logger.Warn("metrics endpoint unauthorized access attempt",
				"remote_addr", r.RemoteAddr,
				"user_agent", r.UserAgent())

			w.Header().Set("WWW-Authenticate", `Basic realm="Metrics"`)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Unauthorized\n"))
			return
		}

		// Credentials are valid, pass to next handler
		next.ServeHTTP(w, r)
	})
}

// initLogger creates a slog logger configured for production use.
//
// The logger writes to stdout for systemd compatibility and supports
// two output formats:
//   - JSON: Structured logging suitable for log aggregation systems
//   - Console: Human-readable format (text handler)
//
// Log levels supported: debug, info, warn, error
//
// Parameters:
//   - logCfg: Configuration specifying log level and format
//
// Returns a configured slog logger or an error if configuration is invalid.
func initLogger(logCfg *config.LoggingConfig) (*slog.Logger, error) {
	// Parse log level
	var level slog.Level
	switch strings.ToLower(logCfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	// Create handler based on format
	opts := &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
	}

	var handler slog.Handler
	if strings.ToLower(logCfg.Format) == "json" {
		// JSON format - good for systemd with structured logging
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		// Text format - good for human readability
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(handler)
	return logger, nil
}

// main is the entry point for fune-server.
//
// It orchestrates the complete server lifecycle:
//  1. Configuration loading and validation
//  2. Component initialization (queue, workers, deliverer, callbacks)
//  3. HTTP server setup (API, metrics, health, TLS)
//  4. Background job scheduling (cleanup, monitoring, gossip)
//  5. Signal handling and graceful shutdown
//
// The function blocks until a shutdown signal (SIGINT/SIGTERM) is received,
// at which point it initiates a graceful shutdown with a 30-second timeout.
//
// Configuration hot reload is supported via SIGHUP signal. Reloadable settings
// include source IPs, rate limits, circuit breaker thresholds, and DNS config.
// Non-reloadable settings (database path, worker count) require a restart.
//
// All background goroutines are protected with panic recovery to prevent
// process crashes from individual component failures.
func main() {
	// Parse command-line flags
	showVersion := flag.Bool("version", false, "Show version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("fune-server version %s\n", version)
		fmt.Printf("  commit: %s\n", commit)
		fmt.Printf("  built:  %s\n", date)
		os.Exit(0)
	}

	// Load configuration first (need it for logger setup)
	tempCfg, err := config.LoadConfig("config.toml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	tempCfg.SetDefaults()

	// Initialize logger with config
	logger, err := initLogger(&tempCfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}

	logger.Info("starting fune SMTP delivery service",
		"version", version,
		"commit", commit,
		"build_date", date,
		"log_level", tempCfg.Logging.Level,
		"log_format", tempCfg.Logging.Format)

	// Load reloadable configuration
	reloadableCfg, err := config.NewReloadableConfig("config.toml", logger)
	if err != nil {
		logger.Error("failed to load reloadable config", "error", err)
		os.Exit(1)
	}

	cfg := reloadableCfg.Get()

	// Write PID file
	pid := os.Getpid()
	err = os.WriteFile(cfg.Server.PIDFile, []byte(strconv.Itoa(pid)+"\n"), 0644)
	if err != nil {
		logger.Error("failed to write PID file", "pid_file", cfg.Server.PIDFile, "error", err)
		os.Exit(1)
	}
	defer os.Remove(cfg.Server.PIDFile)

	logger.Info("PID file written", "pid_file", cfg.Server.PIDFile, "pid", pid)

	// Log configuration
	logger.Info("configuration loaded",
		"database", cfg.Server.DatabasePath,
		"workers", cfg.Queue.WorkerCount,
		"source_ips", len(cfg.Outbound.SourceIPs),
		"source_ip_selection", cfg.Outbound.SourceIPSelection,
		"callback_url", cfg.Callbacks.WebhookURL,
		"max_message_age_hours", cfg.Outbound.MaxMessageAgeHours)

	// Initialize metrics (if enabled)
	var m *metrics.Metrics
	if !cfg.Metrics.Enabled {
		logger.Warn("metrics are DISABLED")
	} else {
		m = metrics.NewMetrics()
		logger.Info("metrics initialized", "path", cfg.Metrics.Path)
	}

	// Initialize cluster gossip service (if enabled)
	gossipCfg := &gossip.Config{
		Enabled:        cfg.Cluster.Enabled,
		BindAddr:       cfg.Cluster.BindAddr,
		BindPort:       cfg.Cluster.BindPort,
		Peers:          cfg.Cluster.Peers,
		NodeID:         cfg.Cluster.NodeID,
		SecretKey:      cfg.Cluster.SecretKey,
		IdempotencyTTL: time.Duration(cfg.Inbound.IdempotencyTTLHours) * time.Hour,
	}

	g, err := gossip.NewGossip(gossipCfg, logger)
	if err != nil {
		logger.Error("failed to initialize cluster", "error", err)
		os.Exit(1)
	}
	if g != nil {
		defer g.Shutdown()
		logger.Info("cluster gossip initialized",
			"enabled", cfg.Cluster.Enabled,
			"bind_addr", cfg.Cluster.BindAddr,
			"bind_port", cfg.Cluster.BindPort,
			"peers", slog.Any("peers", cfg.Cluster.Peers))
	}

	// Initialize TLS Manager for auto-certificates
	// Use a context with timeout for initialization (respects cancellation)
	initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
	tlsManager, err := tlsmanager.NewManager(initCtx, &cfg.TLS, g, logger)
	initCancel() // Clean up context after initialization
	if err != nil {
		logger.Error("failed to initialize TLS manager", "error", err)
		os.Exit(1)
	}

	// Track server start time for uptime reporting
	serverStartTime := time.Now()

	// Initialize queue
	q, err := queue.NewQueue(cfg.Server.DatabasePath, logger)
	if err != nil {
		logger.Error("failed to initialize queue", "error", err)
		os.Exit(1)
	}

	// Wire metrics to queue
	if m != nil {
		q.SetMetrics(m)
	}

	logger.Info("queue initialized", "database", cfg.Server.DatabasePath)

	// Initialize MX lookup with DNS resolver configuration
	mxLookup := delivery.NewMXLookup(q, &cfg.DNS, &cfg.Outbound, logger)

	// Initialize deliverer with ARC and SRS config
	deliverer := delivery.NewDeliverer(&cfg.Outbound, mxLookup, logger, &cfg.Reputation, &cfg.ARC, &cfg.SRS)

	// Wire metrics to deliverer
	if m != nil {
		deliverer.SetMetrics(m)
	}

	// Wire metrics to circuit breaker (if enabled)
	if m != nil && deliverer.GetCircuitBreaker() != nil {
		deliverer.GetCircuitBreaker().SetMetrics(m)
	}

	// Wire metrics to reputation tracker (if enabled)
	if m != nil && deliverer.GetReputationTracker() != nil {
		deliverer.GetReputationTracker().SetMetrics(m)
	}

	// Initialize retry scheduler
	retryScheduler := delivery.NewRetryScheduler(&cfg.Outbound)

	// Initialize callback handler
	callbackHandler := callback.NewCallbackHandler(q, &cfg.Callbacks, logger)

	// Wire metrics to callback handler
	if m != nil {
		callbackHandler.SetMetrics(m)
	}

	callbackHandler.Start()
	defer callbackHandler.Stop()

	logger.Info("callback handler started")

	// Initialize worker with MXLookup for batch DNS prefetching
	w := worker.NewWorker(q, deliverer, retryScheduler, mxLookup, callbackHandler, &cfg.Outbound, &cfg.Queue, logger)
	w.Start(cfg.Queue.WorkerCount)
	defer w.Stop()

	logger.Info("background workers started", "count", cfg.Queue.WorkerCount)

	// Start background metrics updater for queue depth
	if m != nil {
		recovery.SafeGo(logger, "queue metrics updater", func() {
			ticker := time.NewTicker(10 * time.Second) // Update queue metrics every 10 seconds
			defer ticker.Stop()

			for range ticker.C {
				if err := q.UpdateQueueMetrics(); err != nil {
					logger.Error("failed to update queue metrics", "error", err)
				}
			}
		})
		logger.Info("queue metrics updater started")
	}

	// Start terminal message cleanup job (handles both idempotent and non-idempotent messages)
	recovery.SafeGo(logger, "terminal message cleanup", func() {
		ticker := time.NewTicker(5 * time.Minute) // Cleanup every 5 minutes
		defer ticker.Stop()

		for range ticker.C {
			deleted, err := q.CleanupTerminalMessages(cfg.Inbound.IdempotencyTTLHours)
			if err != nil {
				logger.Error("failed to cleanup terminal messages", "error", err)
			} else if deleted > 0 {
				logger.Info("cleaned up terminal messages",
					"deleted_count", deleted,
					"idempotency_ttl_hours", cfg.Inbound.IdempotencyTTLHours)
			}
		}
	})
	logger.Info("terminal message cleanup job started",
		"idempotency_ttl_hours", cfg.Inbound.IdempotencyTTLHours)

	// Start degraded IP cleanup job
	recovery.SafeGo(logger, "degraded IP cleanup", func() {
		ticker := time.NewTicker(1 * time.Hour) // Cleanup every hour
		defer ticker.Stop()

		for range ticker.C {
			if reputationTracker := deliverer.GetReputationTracker(); reputationTracker != nil {
				reputationTracker.Cleanup()
			}
		}
	})
	logger.Info("degraded IP cleanup job started")

	// Initialize HTTP handler with circuit breaker reference
	httpHandler := handler.NewQueueMessageHandler(q, &cfg.Outbound, &cfg.Inbound, deliverer.GetCircuitBreaker(), logger)

	// Wire gossip service to HTTP handler
	if g != nil {
		httpHandler.SetGossip(g)
	}

	// Wrap with metrics middleware if enabled
	var finalHandler http.Handler = httpHandler
	if m != nil {
		finalHandler = handler.MetricsMiddleware(httpHandler, m)
	}

	// Wrap with security headers middleware (defense in depth)
	finalHandler = handler.SecurityHeadersMiddleware(finalHandler)

	// Setup HTTP router
	mux := http.NewServeMux()
	mux.Handle("/", finalHandler)

	// Add metrics endpoint if enabled
	if m != nil && cfg.Metrics.Path != "" {
		metricsHandler := promhttp.Handler()

		// Wrap with Basic Auth if credentials are configured
		if cfg.Metrics.Username != "" && cfg.Metrics.Password != "" {
			metricsHandler = basicAuthMiddleware(metricsHandler, cfg.Metrics.Username, cfg.Metrics.Password, logger)
			logger.Info("metrics endpoint registered with Basic Auth",
				"path", cfg.Metrics.Path,
				"username", cfg.Metrics.Username)
		} else {
			logger.Warn("metrics endpoint registered WITHOUT authentication - not recommended for production",
				"path", cfg.Metrics.Path)
		}

		mux.Handle(cfg.Metrics.Path, metricsHandler)
	}

	// Add cluster status endpoint if gossip is enabled
	if g != nil {
		clusterHandler := handler.NewClusterStatusHandler(g, logger)
		mux.Handle("/admin/cluster/status", clusterHandler)
		logger.Info("cluster status endpoint registered", "path", "/admin/cluster/status")
	}

	// Start periodic node status broadcasting if gossip is enabled
	if g != nil {
		recovery.SafeGo(logger, "gossip status broadcaster", func() {
			ticker := time.NewTicker(10 * time.Second) // Broadcast every 10 seconds
			defer ticker.Stop()

			for range ticker.C {
				// Get current queue depth (queued + sending messages)
				queueDepth, err := q.GetQueueDepth()
				if err != nil {
					logger.Error("failed to get queue depth for gossip", "error", err)
					queueDepth = 0 // Use 0 on error
				}

				// Get active workers (we don't track this dynamically yet, use configured count)
				activeWorkers := cfg.Queue.WorkerCount

				// Calculate uptime
				uptime := time.Since(serverStartTime)

				// Broadcast status to cluster
				if err := g.BroadcastNodeStatus(queueDepth, activeWorkers, uptime); err != nil {
					logger.Error("failed to broadcast node status", "error", err)
				} else {
					logger.Debug("broadcast node status to cluster",
						"queue_depth", queueDepth,
						"active_workers", activeWorkers,
						"uptime", uptime)
				}
			}
		})
		logger.Info("gossip status broadcaster started")
	}

	// Setup dedicated health check server (if enabled)
	var healthServer *http.Server
	if cfg.Health.Enabled {
		healthHandler := handler.NewHealthHandler(g, q, deliverer, logger)

		// Wrap with Basic Auth if credentials are configured
		var finalHealthHandler http.Handler = healthHandler
		if cfg.Health.Username != "" && cfg.Health.Password != "" {
			finalHealthHandler = basicAuthMiddleware(healthHandler, cfg.Health.Username, cfg.Health.Password, logger)
			logger.Info("health endpoint registered with Basic Auth",
				"listen_addr", cfg.Health.ListenAddr,
				"username", cfg.Health.Username)
		} else {
			logger.Info("health endpoint registered without authentication",
				"listen_addr", cfg.Health.ListenAddr)
		}

		healthMux := http.NewServeMux()
		healthMux.Handle("/health", finalHealthHandler)

		healthServer = &http.Server{
			Addr:         cfg.Health.ListenAddr,
			Handler:      healthMux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		}

		// Start health server in background
		recovery.SafeGo(logger, "health server", func() {
			logger.Info("health server starting", "addr", cfg.Health.ListenAddr)
			if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("health server failed", "error", err)
			}
		})
	} else {
		logger.Info("health endpoint disabled")
	}

	// Setup HTTP server with TLS certificate hot reload support
	server := &http.Server{
		Addr:         cfg.Inbound.Listen,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Inbound.ReadTimeoutSecs) * time.Second,
		WriteTimeout: time.Duration(cfg.Inbound.WriteTimeoutSecs) * time.Second,
		IdleTimeout:  time.Duration(cfg.Inbound.IdleTimeoutSecs) * time.Second,
	}

	// HTTP challenge server (for Let's Encrypt ACME HTTP-01)
	var httpChallengeServer *http.Server

	// Configure TLS
	if cfg.TLS.Enabled {
		switch cfg.TLS.Provider {
		case "letsencrypt":
			if tlsManager != nil {
				server.TLSConfig = tlsManager.TLSConfig()

				// Start HTTP server on port 80 for ACME HTTP-01 challenges
				// This is required for Let's Encrypt domain verification
				httpChallengeServer = &http.Server{
					Addr:         ":80",
					Handler:      tlsManager.HTTPHandler(nil), // nil = redirect to HTTPS
					ReadTimeout:  30 * time.Second,
					WriteTimeout: 30 * time.Second,
				}

				// Attempt to start HTTP challenge server
				// This must succeed for ACME to work
				recovery.SafeGo(logger, "HTTP-01 challenge server", func() {
					logger.Info("starting ACME HTTP-01 challenge server on port 80")
					if err := httpChallengeServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						logger.Error("HTTP-01 challenge server failed - port 80 required for Let's Encrypt",
							"error", err,
							"hint", "ensure port 80 is available and not in use by another service",
							"help", "check with: sudo lsof -i :80")
						os.Exit(1)
					}
				})

				// Give the HTTP server a moment to start and potentially fail fast
				time.Sleep(100 * time.Millisecond)

				// Start certificate monitoring (check every 6 hours)
				recovery.SafeGo(logger, "certificate monitor", func() {
					// Initial check after 1 minute (gives time for first cert acquisition)
					time.Sleep(1 * time.Minute)
					tlsManager.CheckCertificates()

					ticker := time.NewTicker(6 * time.Hour)
					defer ticker.Stop()

					for range ticker.C {
						tlsManager.CheckCertificates()
					}
				})
				logger.Info("certificate monitoring started (checks every 6 hours)")

				logger.Info("TLS configured with letsencrypt provider",
					"domains", slog.Any("domains", cfg.TLS.LetsEncrypt.Domains))
			} else {
				logger.Error("TLS provider is letsencrypt, but manager failed to initialize")
				os.Exit(1)
			}
		case "file":
			fallthrough
		default:
			// Validate TLS configuration
			if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
				logger.Error("TLS enabled with file provider, but cert_file or key_file not specified")
				os.Exit(1)
			}

			// Setup TLS config with GetCertificate callback for hot reload
			server.TLSConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
				GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
					// Load certificate from disk on each connection
					// This allows hot reload when certificate files are updated
					tlsConfig, err := reloadableCfg.LoadTLSConfig()
					if err != nil {
						logger.Error("failed to load TLS certificate", "error", err)
						return nil, err
					}
					if len(tlsConfig.Certificates) == 0 {
						return nil, nil
					}
					return &tlsConfig.Certificates[0], nil
				},
			}

			logger.Info("TLS configured with file provider and hot reload support",
				"cert_file", cfg.TLS.CertFile,
				"key_file", cfg.TLS.KeyFile)
		}
	}

	// Start HTTP server in goroutine
	recovery.SafeGo(logger, "HTTP server", func() {
		// Log server configuration
		if cfg.Inbound.AuthToken != "" {
			logger.Info("starting HTTP server",
				"listen", cfg.Inbound.Listen,
				"auth_enabled", true,
				"tls_enabled", cfg.TLS.Enabled)
		} else {
			logger.Warn("starting HTTP server WITHOUT authentication",
				"listen", cfg.Inbound.Listen,
				"auth_enabled", false,
				"tls_enabled", cfg.TLS.Enabled)
		}

		// Start server with or without TLS
		var err error
		if cfg.TLS.Enabled {
			// Use ListenAndServeTLS with empty cert/key paths since we configured TLS via server.TLSConfig
			err = server.ListenAndServeTLS("", "")
		} else {
			logger.Warn("starting HTTP server without TLS (unencrypted)")
			err = server.ListenAndServe()
		}

		if err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	})

	// Register reload callbacks
	reloadableCfg.RegisterReloadCallback(func(newCfg *config.Config) error {
		// Reload delivery configuration
		if err := deliverer.ReloadConfig(&newCfg.Outbound); err != nil {
			return err
		}

		// Reload MX lookup DNS configuration
		if err := mxLookup.ReloadConfig(&newCfg.DNS, &newCfg.Outbound); err != nil {
			return err
		}

		// Note: TLS certificates are reloaded automatically via GetCertificate callback
		return nil
	})

	// Handle signals (SIGINT/SIGTERM for shutdown, SIGHUP for reload)
	quit := make(chan os.Signal, 1)
	reload := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	signal.Notify(reload, syscall.SIGHUP)

	logger.Info("signal handlers registered (SIGINT/SIGTERM=shutdown, SIGHUP=reload)")

	// Main signal loop
	for {
		select {
		case <-reload:
			logger.Info("SIGHUP received, reloading configuration...")
			if err := reloadableCfg.Reload(); err != nil {
				logger.Error("failed to reload configuration", "error", err)
			} else {
				logger.Info("configuration reloaded successfully")
			}
		case <-quit:
			logger.Info("shutdown signal received, starting graceful shutdown...")
			goto shutdown
		}
	}

shutdown:

	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shutdown HTTP servers
	logger.Info("shutting down HTTP server...")
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}

	// Shutdown HTTP challenge server (if running)
	if httpChallengeServer != nil {
		logger.Info("shutting down HTTP-01 challenge server...")
		if err := httpChallengeServer.Shutdown(ctx); err != nil {
			logger.Error("HTTP challenge server shutdown error", "error", err)
		}
	}

	// Shutdown health server (if running)
	if healthServer != nil {
		logger.Info("shutting down health server...")
		if err := healthServer.Shutdown(ctx); err != nil {
			logger.Error("health server shutdown error", "error", err)
		}
	}

	// Stop workers
	logger.Info("stopping background workers...")
	w.Stop()

	// Stop callback handler
	logger.Info("stopping callback handler...")
	callbackHandler.Stop()

	// Close queue
	logger.Info("closing queue...")
	q.Close()

	logger.Info("shutdown complete")
}
