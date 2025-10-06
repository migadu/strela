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

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Version information, injected at build time
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// initLogger creates a logger that writes to stdout/stderr (systemd-compatible)
func initLogger(logCfg *config.LoggingConfig) (*zap.Logger, error) {
	// Parse log level
	var level zapcore.Level
	switch strings.ToLower(logCfg.Level) {
	case "debug":
		level = zapcore.DebugLevel
	case "info":
		level = zapcore.InfoLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	default:
		level = zapcore.InfoLevel
	}

	// Create encoder based on format
	var encoder zapcore.Encoder
	if strings.ToLower(logCfg.Format) == "json" {
		// JSON format - good for systemd with structured logging
		encoderConfig := zap.NewProductionEncoderConfig()
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		// Console format - good for human readability
		encoderConfig := zap.NewProductionEncoderConfig()
		encoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05")
		encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		encoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	// Create core that writes to stdout
	core := zapcore.NewCore(
		encoder,
		zapcore.AddSync(os.Stdout),
		level,
	)

	// Create logger with caller info and stack traces on errors
	logger := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))

	return logger, nil
}

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
	defer logger.Sync()

	logger.Info("starting fune SMTP delivery service",
		zap.String("version", version),
		zap.String("commit", commit),
		zap.String("build_date", date),
		zap.String("log_level", tempCfg.Logging.Level),
		zap.String("log_format", tempCfg.Logging.Format))

	// Load reloadable configuration
	reloadableCfg, err := config.NewReloadableConfig("config.toml", logger)
	if err != nil {
		logger.Fatal("failed to load reloadable config", zap.Error(err))
	}

	cfg := reloadableCfg.Get()

	// Write PID file
	pid := os.Getpid()
	err = os.WriteFile(cfg.Server.PIDFile, []byte(strconv.Itoa(pid)+"\n"), 0644)
	if err != nil {
		logger.Fatal("failed to write PID file", zap.String("pid_file", cfg.Server.PIDFile), zap.Error(err))
	}
	defer os.Remove(cfg.Server.PIDFile)

	logger.Info("PID file written", zap.String("pid_file", cfg.Server.PIDFile), zap.Int("pid", pid))

	// Log configuration
	logger.Info("configuration loaded",
		zap.String("database", cfg.Server.DatabasePath),
		zap.Int("workers", cfg.Queue.WorkerCount),
		zap.Int("source_ips", len(cfg.Delivery.SourceIPs)),
		zap.String("ip_selection", cfg.Delivery.IPSelection),
		zap.String("callback_url", cfg.Callbacks.WebhookURL),
		zap.Int("max_message_age_hours", cfg.Delivery.MaxMessageAgeHours))

	// Initialize metrics (if enabled)
	var m *metrics.Metrics
	if !cfg.HTTP.MetricsEnabled {
		logger.Warn("metrics are DISABLED")
	} else {
		m = metrics.NewMetrics()
		logger.Info("metrics initialized", zap.String("path", cfg.HTTP.MetricsPath))
	}

	// Initialize gossip service (if enabled)
	gossipCfg := &gossip.Config{
		Enabled:        cfg.Gossip.Enabled,
		BindPort:       cfg.Gossip.BindPort,
		JoinAddresses:  cfg.Gossip.JoinAddresses,
		NodeID:         cfg.Gossip.NodeID,
		IdempotencyTTL: time.Duration(cfg.HTTP.IdempotencyTTLHours) * time.Hour,
	}

	g, err := gossip.NewGossip(gossipCfg, logger)
	if err != nil {
		logger.Fatal("failed to initialize gossip", zap.Error(err))
	}
	if g != nil {
		defer g.Shutdown()
		logger.Info("gossip service initialized",
			zap.Bool("enabled", cfg.Gossip.Enabled),
			zap.Int("bind_port", cfg.Gossip.BindPort),
			zap.Strings("join_addresses", cfg.Gossip.JoinAddresses))
	}

	// Initialize TLS Manager for auto-certificates
	tlsManager, err := tlsmanager.NewManager(&cfg.TLS, g, logger)
	if err != nil {
		logger.Fatal("failed to initialize TLS manager", zap.Error(err))
	}

	// Track server start time for uptime reporting
	serverStartTime := time.Now()

	// Initialize queue
	q, err := queue.NewQueue(cfg.Server.DatabasePath, logger)
	if err != nil {
		logger.Fatal("failed to initialize queue", zap.Error(err))
	}

	// Wire metrics to queue
	if m != nil {
		q.SetMetrics(m)
	}

	logger.Info("queue initialized", zap.String("database", cfg.Server.DatabasePath))

	// Initialize MX lookup with DNS resolver configuration
	mxLookup := delivery.NewMXLookup(q, &cfg.Delivery, logger)

	// Initialize deliverer
	deliverer := delivery.NewDeliverer(&cfg.Delivery, mxLookup, logger, &cfg.Reputation)

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
	retryScheduler := delivery.NewRetryScheduler(&cfg.Delivery)

	// Initialize callback handler
	callbackHandler := callback.NewCallbackHandler(q, &cfg.Callbacks, logger)

	// Wire metrics to callback handler
	if m != nil {
		callbackHandler.SetMetrics(m)
	}

	callbackHandler.Start()
	defer callbackHandler.Stop()

	logger.Info("callback handler started")

	// Initialize worker
	w := worker.NewWorker(q, deliverer, retryScheduler, callbackHandler, &cfg.Delivery, &cfg.Queue, logger)
	w.Start(cfg.Queue.WorkerCount)
	defer w.Stop()

	logger.Info("background workers started", zap.Int("count", cfg.Queue.WorkerCount))

	// Start background metrics updater for queue depth
	if m != nil {
		recovery.SafeGo(logger, "queue metrics updater", func() {
			ticker := time.NewTicker(10 * time.Second) // Update queue metrics every 10 seconds
			defer ticker.Stop()

			for range ticker.C {
				if err := q.UpdateQueueMetrics(); err != nil {
					logger.Error("failed to update queue metrics", zap.Error(err))
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
			deleted, err := q.CleanupTerminalMessages(cfg.HTTP.IdempotencyTTLHours)
			if err != nil {
				logger.Error("failed to cleanup terminal messages", zap.Error(err))
			} else if deleted > 0 {
				logger.Info("cleaned up terminal messages",
					zap.Int64("deleted_count", deleted),
					zap.Int("idempotency_ttl_hours", cfg.HTTP.IdempotencyTTLHours))
			}
		}
	})
	logger.Info("terminal message cleanup job started",
		zap.Int("idempotency_ttl_hours", cfg.HTTP.IdempotencyTTLHours))

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
	httpHandler := handler.NewQueueMessageHandler(q, &cfg.Delivery, &cfg.HTTP, deliverer.GetCircuitBreaker(), logger)

	// Wire gossip service to HTTP handler
	if g != nil {
		httpHandler.SetGossip(g)
	}

	// Wrap with metrics middleware if enabled
	var finalHandler http.Handler = httpHandler
	if m != nil {
		finalHandler = handler.MetricsMiddleware(httpHandler, m)
	}

	// Setup HTTP router
	mux := http.NewServeMux()
	mux.Handle("/", finalHandler)

	// Add metrics endpoint if enabled
	if m != nil && cfg.HTTP.MetricsPath != "" {
		mux.Handle(cfg.HTTP.MetricsPath, promhttp.Handler())
		logger.Info("metrics endpoint registered", zap.String("path", cfg.HTTP.MetricsPath))
	}

	// Add cluster status endpoint if gossip is enabled
	if g != nil {
		clusterHandler := handler.NewClusterStatusHandler(g, logger)
		mux.Handle("/admin/cluster/status", clusterHandler)
		logger.Info("cluster status endpoint registered", zap.String("path", "/admin/cluster/status"))
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
					logger.Error("failed to get queue depth for gossip", zap.Error(err))
					queueDepth = 0 // Use 0 on error
				}

				// Get active workers (we don't track this dynamically yet, use configured count)
				activeWorkers := cfg.Queue.WorkerCount

				// Calculate uptime
				uptime := time.Since(serverStartTime)

				// Broadcast status to cluster
				if err := g.BroadcastNodeStatus(queueDepth, activeWorkers, uptime); err != nil {
					logger.Error("failed to broadcast node status", zap.Error(err))
				} else {
					logger.Debug("broadcast node status to cluster",
						zap.Int64("queue_depth", queueDepth),
						zap.Int("active_workers", activeWorkers),
						zap.Duration("uptime", uptime))
				}
			}
		})
		logger.Info("gossip status broadcaster started")
	}

	// Setup HTTP server with TLS certificate hot reload support
	server := &http.Server{
		Addr:         cfg.HTTP.Listen,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.HTTP.ReadTimeoutSecs) * time.Second,
		WriteTimeout: time.Duration(cfg.HTTP.WriteTimeoutSecs) * time.Second,
		IdleTimeout:  time.Duration(cfg.HTTP.IdleTimeoutSecs) * time.Second,
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
						logger.Fatal("HTTP-01 challenge server failed - port 80 required for Let's Encrypt",
							zap.Error(err),
							zap.String("hint", "ensure port 80 is available and not in use by another service"),
							zap.String("help", "check with: sudo lsof -i :80"))
					}
				})

				// Give the HTTP server a moment to start and potentially fail fast
				time.Sleep(100 * time.Millisecond)

				logger.Info("TLS configured with letsencrypt provider",
					zap.Strings("domains", cfg.TLS.LetsEncrypt.Domains))
			} else {
				logger.Fatal("TLS provider is letsencrypt, but manager failed to initialize")
			}
		case "file":
			fallthrough
		default:
			// Validate TLS configuration
			if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
				logger.Fatal("TLS enabled with file provider, but cert_file or key_file not specified")
			}

			// Setup TLS config with GetCertificate callback for hot reload
			server.TLSConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
				GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
					// Load certificate from disk on each connection
					// This allows hot reload when certificate files are updated
					tlsConfig, err := reloadableCfg.LoadTLSConfig()
					if err != nil {
						logger.Error("failed to load TLS certificate", zap.Error(err))
						return nil, err
					}
					if len(tlsConfig.Certificates) == 0 {
						return nil, nil
					}
					return &tlsConfig.Certificates[0], nil
				},
			}

			logger.Info("TLS configured with file provider and hot reload support",
				zap.String("cert_file", cfg.TLS.CertFile),
				zap.String("key_file", cfg.TLS.KeyFile))
		}
	}

	// Start HTTP server in goroutine
	recovery.SafeGo(logger, "HTTP server", func() {
		// Log server configuration
		if cfg.HTTP.AuthToken != "" {
			logger.Info("starting HTTP server",
				zap.String("listen", cfg.HTTP.Listen),
				zap.Bool("auth_enabled", true),
				zap.Bool("tls_enabled", cfg.TLS.Enabled))
		} else {
			logger.Warn("starting HTTP server WITHOUT authentication",
				zap.String("listen", cfg.HTTP.Listen),
				zap.Bool("auth_enabled", false),
				zap.Bool("tls_enabled", cfg.TLS.Enabled))
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
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	})

	// Register reload callbacks
	reloadableCfg.RegisterReloadCallback(func(newCfg *config.Config) error {
		// Reload delivery configuration
		if err := deliverer.ReloadConfig(&newCfg.Delivery); err != nil {
			return err
		}

		// Reload MX lookup DNS configuration
		if err := mxLookup.ReloadConfig(&newCfg.Delivery); err != nil {
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
				logger.Error("failed to reload configuration", zap.Error(err))
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
		logger.Error("HTTP server shutdown error", zap.Error(err))
	}

	// Shutdown HTTP challenge server (if running)
	if httpChallengeServer != nil {
		logger.Info("shutting down HTTP-01 challenge server...")
		if err := httpChallengeServer.Shutdown(ctx); err != nil {
			logger.Error("HTTP challenge server shutdown error", zap.Error(err))
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
