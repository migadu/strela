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

	"strela/internal/cluster"
	"strela/internal/config"
	"strela/internal/delivery"
	"strela/internal/handler"
	"strela/internal/metrics"
	"strela/internal/recovery"
	tlsmanager "strela/internal/tls"

	"log/slog"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func initLogger(logCfg *config.LoggingConfig) (*slog.Logger, *config.LogWriter, error) {
	return config.NewLogger(*logCfg)
}

func basicAuthMiddleware(next http.Handler, username, password string, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			logger.Warn("admin endpoint unauthorized access attempt",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Basic realm="Admin"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cmdVersion() {
	fmt.Printf("strela-server %s\n", version)
	fmt.Printf("  commit: %s\n", commit)
	fmt.Printf("  built:  %s\n", date)
}

func defaultConfigPath() string {
	for _, p := range []string{
		"/etc/strela/config.toml",
		"/usr/local/etc/strela/config.toml",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "config.toml"
}

func main() {
	configPath := flag.String("config", defaultConfigPath(), "Path to configuration file")
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

	logger, logWriter, err := initLogger(&tempCfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logWriter.Close()

	logger.Info(">>>>>>>____________________\\'-._  ")
	logger.Info(">>>>>>>                    //.-'   ")
	logger.Info("Strela SMTP delivery service", "version", version, "config", *configPath)

	// Warn if auth_token is not configured
	if tempCfg.Inbound.AuthToken == "" {
		logger.Warn("WARNING: no auth_token configured - API is unauthenticated! Set [inbound] auth_token in production.")
	}

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
		"smtp_ip_mode", cfg.Outbound.SMTPIPMode,
		"lmtp_ip_mode", cfg.Outbound.LMTPIPMode)

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

	// Middleware stack shared by all delivery routes
	var rateLimiter *handler.RateLimiter
	if cfg.Inbound.RateLimitEnabled {
		rateLimiter = handler.NewRateLimiter(cfg.Inbound.RateLimitRequestsPerIP, cfg.Inbound.RateLimitWindowSeconds, logger)
	}
	wrapDeliveryHandler := func(h http.Handler) http.Handler {
		wrapped := h
		if cfg.Inbound.MaxConcurrentRequests > 0 {
			wrapped = handler.ConcurrencyLimitMiddleware(cfg.Inbound.MaxConcurrentRequests)(wrapped)
		}
		if rateLimiter != nil {
			wrapped = rateLimiter.Middleware(wrapped)
		}
		if m != nil {
			wrapped = handler.MetricsMiddleware(wrapped, m)
		}
		wrapped = handler.SecurityHeadersMiddleware(wrapped)
		// CRITICAL: Panic recovery must be the outermost middleware to catch all panics
		wrapped = handler.PanicRecoveryMiddleware(wrapped, logger)
		return wrapped
	}

	// Routes
	mux.Handle("/deliver", wrapDeliveryHandler(http.HandlerFunc(h.HandleDeliver)))
	mux.Handle("/deliver/smtp", wrapDeliveryHandler(http.HandlerFunc(h.HandleDeliverSMTP)))
	mux.Handle("/deliver/lmtp", wrapDeliveryHandler(http.HandlerFunc(h.HandleDeliverLMTP)))

	// Initialize cluster for leader election (if enabled) — before admin server so health can report cluster status
	var clusterMgr *cluster.Cluster
	if cfg.Cluster.Enabled {
		// Decode secret key from base64
		var secretKey []byte
		secretKeyStr := os.Getenv("CLUSTER_SECRET_KEY")
		if secretKeyStr == "" {
			secretKeyStr = cfg.Cluster.SecretKey
		}
		if secretKeyStr != "" {
			secretKey, err = cluster.DecodeSecretKey(secretKeyStr)
			if err != nil {
				logger.Error("failed to decode cluster secret key", "error", err)
				os.Exit(1)
			}
		}

		nodeID := cfg.Cluster.NodeID
		if nodeID == "" {
			if h, hErr := os.Hostname(); hErr == nil {
				nodeID = h
			}
		}

		clusterMgr, err = cluster.NewCluster(cluster.Config{
			NodeName:  nodeID,
			BindAddr:  cfg.Cluster.GetBindAddr(),
			BindPort:  cfg.Cluster.GetBindPort(),
			Peers:     cfg.Cluster.Peers,
			SecretKey: secretKey,
			Logger:    logger,
		})
		if err != nil {
			logger.Error("failed to initialize cluster", "error", err)
			os.Exit(1)
		}
		defer clusterMgr.Shutdown()
	}

	// Admin server (health + metrics) on separate localhost-only listener
	var adminServer *http.Server
	if cfg.Admin.Enabled {
		adminMux := http.NewServeMux()

		// Health Endpoint (with optional cluster info)
		if clusterMgr != nil {
			adminMux.Handle("/health", handler.NewHealthHandler(deliverer, logger, clusterMgr))
		} else {
			adminMux.Handle("/health", handler.NewHealthHandler(deliverer, logger))
		}

		// Metrics Endpoint (served on admin port alongside health)
		if m != nil && cfg.Metrics.Path != "" {
			adminMux.Handle(cfg.Metrics.Path, promhttp.Handler())
		}

		// Apply admin-level auth and panic recovery to all admin endpoints
		var adminHandler http.Handler = adminMux
		if cfg.Admin.Username != "" && cfg.Admin.Password != "" {
			adminHandler = basicAuthMiddleware(adminMux, cfg.Admin.Username, cfg.Admin.Password, logger)
		}
		adminHandler = handler.PanicRecoveryMiddleware(adminHandler, logger)

		adminServer = &http.Server{
			Addr:         cfg.Admin.Bind,
			Handler:      adminHandler,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  30 * time.Second,
			ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
		}

		recovery.SafeGo(logger, "admin-server", func() {
			logger.Info("admin server starting (health + metrics)", "addr", cfg.Admin.Bind)
			if err := adminServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("admin server failed", "error", err)
				os.Exit(1)
			}
		})
	}

	// TLS Manager (if ACME enabled)
	var tlsManager *tlsmanager.Manager
	var challengeServer *http.Server
	if cfg.TLS.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if clusterMgr != nil {
			// Cluster enabled: only leader can request new certs from Let's Encrypt
			tlsManager, err = tlsmanager.NewManager(ctx, &cfg.TLS, logger, clusterMgr.IsLeader)
		} else {
			// Single-instance mode
			tlsManager, err = tlsmanager.NewManager(ctx, &cfg.TLS, logger)
		}
		cancel()
		if err != nil {
			logger.Error("failed to initialize TLS manager", "error", err)
			os.Exit(1)
		}

		// Start HTTP-01 challenge server on port 80 (required for Let's Encrypt certificate issuance and renewal)
		challengeServer = &http.Server{
			Addr:         ":80",
			Handler:      tlsManager.HTTPHandler(nil),
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
			IdleTimeout:  30 * time.Second,
			ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
		}

		recovery.SafeGo(logger, "acme-http-challenge", func() {
			logger.Info("ACME HTTP-01 challenge server starting", "addr", ":80")
			if err := challengeServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("ACME challenge server failed", "error", err)
			}
		})
	}

	server := &http.Server{
		Addr:         cfg.Inbound.Listen,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Inbound.ReadTimeoutSecs) * time.Second,
		WriteTimeout: time.Duration(cfg.Inbound.WriteTimeoutSecs) * time.Second,
		IdleTimeout:  time.Duration(cfg.Inbound.IdleTimeoutSecs) * time.Second,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelDebug), // TLS handshake errors at debug level
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

	// SIGHUP handler: reopen log file and reload config
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	recovery.SafeGo(logger, "sighup-handler", func() {
		for range sighup {
			logger.Info("received SIGHUP, reopening log file and reloading config")
			if err := logWriter.Reopen(); err != nil {
				logger.Error("failed to reopen log file", "error", err)
			}
			if err := reloadableCfg.Reload(); err != nil {
				logger.Error("failed to reload config", "error", err)
			}
		}
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

	// 1b. Stop admin server (health + metrics)
	if adminServer != nil {
		if err := adminServer.Shutdown(ctx); err != nil {
			logger.Error("admin server shutdown error", "error", err)
		}
		logger.Info("admin server stopped")
	}

	// 1c. Stop ACME HTTP-01 challenge server
	if challengeServer != nil {
		if err := challengeServer.Shutdown(ctx); err != nil {
			logger.Error("ACME challenge server shutdown error", "error", err)
		}
		logger.Info("ACME challenge server stopped")
	}

	// 2. Stop rate limiter cleanup goroutine
	if rateLimiter != nil {
		rateLimiter.Stop()
		logger.Info("rate limiter stopped")
	}

	// 3. Stop TLS manager (stops cert sync worker)
	if tlsManager != nil {
		tlsManager.Stop()
		logger.Info("TLS manager stopped")
	}

	// 4. Stop deliverer (stops all background cleanup goroutines, closes connection pool)
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
