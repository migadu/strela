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
	"syscall"
	"time"

	"fune/internal/callback"
	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/handler"
	"fune/internal/metrics"
	"fune/internal/queue"
	"fune/internal/recovery"
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
	// Initialize console logger with human-readable format
	loggerConfig := zap.NewDevelopmentConfig()
	loggerConfig.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05")
	loggerConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder

	logger, err := loggerConfig.Build()
	if err != nil {
		panic("failed to initialize logger: " + err.Error())
	}
	defer logger.Sync()

	logger.Info("starting fune SMTP delivery service",
		zap.String("version", version),
		zap.String("commit", commit),
		zap.String("build_date", date))

	// Load reloadable configuration
	reloadableCfg, err := config.NewReloadableConfig("config.toml", logger)
	if err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
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
		zap.String("database", cfg.Queue.DatabasePath),
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

	// Initialize queue
	q, err := queue.NewQueue(cfg.Queue.DatabasePath, logger)
	if err != nil {
		logger.Fatal("failed to initialize queue", zap.Error(err))
	}

	// Wire metrics to queue
	if m != nil {
		q.SetMetrics(m)
	}

	logger.Info("queue initialized", zap.String("database", cfg.Queue.DatabasePath))

	// Initialize MX lookup with DNS resolver configuration
	mxLookup := delivery.NewMXLookup(q, &cfg.Delivery, logger)

	// Initialize deliverer
	deliverer := delivery.NewDeliverer(&cfg.Delivery, mxLookup, logger)

	// Wire metrics to deliverer
	if m != nil {
		deliverer.SetMetrics(m)
	}

	// Wire metrics to circuit breaker (if enabled)
	if m != nil && deliverer.GetCircuitBreaker() != nil {
		deliverer.GetCircuitBreaker().SetMetrics(m)
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

	// Initialize HTTP handler with circuit breaker reference
	httpHandler := handler.NewQueueMessageHandler(q, &cfg.Delivery, &cfg.HTTP, deliverer.GetCircuitBreaker(), logger)

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

	// Setup HTTP server with TLS certificate hot reload support
	server := &http.Server{
		Addr:         cfg.HTTP.Listen,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.HTTP.ReadTimeoutSecs) * time.Second,
		WriteTimeout: time.Duration(cfg.HTTP.WriteTimeoutSecs) * time.Second,
		IdleTimeout:  time.Duration(cfg.HTTP.IdleTimeoutSecs) * time.Second,
	}

	// Configure TLS with certificate hot reload
	if cfg.HTTP.TLSEnabled {
		// Validate TLS configuration
		if cfg.HTTP.TLSCertFile == "" || cfg.HTTP.TLSKeyFile == "" {
			logger.Fatal("TLS enabled but cert_file or key_file not specified")
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

		logger.Info("TLS configured with hot reload support",
			zap.String("cert_file", cfg.HTTP.TLSCertFile),
			zap.String("key_file", cfg.HTTP.TLSKeyFile))
	}

	// Start HTTP server in goroutine
	recovery.SafeGo(logger, "HTTP server", func() {
		// Log server configuration
		if cfg.HTTP.AuthToken != "" {
			logger.Info("starting HTTP server",
				zap.String("listen", cfg.HTTP.Listen),
				zap.Bool("auth_enabled", true),
				zap.Bool("tls_enabled", cfg.HTTP.TLSEnabled))
		} else {
			logger.Warn("starting HTTP server WITHOUT authentication",
				zap.String("listen", cfg.HTTP.Listen),
				zap.Bool("auth_enabled", false),
				zap.Bool("tls_enabled", cfg.HTTP.TLSEnabled))
		}

		// Start server with or without TLS
		var err error
		if cfg.HTTP.TLSEnabled {
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

	// Shutdown HTTP server
	logger.Info("shutting down HTTP server...")
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
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
