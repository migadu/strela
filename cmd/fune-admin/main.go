// Package main implements fune-admin, the administration CLI for fune SMTP delivery service.
//
// fune-admin provides command-line access to operational data and management functions
// for fune-server. It directly queries the SQLite database for most operations and
// communicates with the server's HTTP API for runtime operations.
//
// Available commands:
//   - queue: Display queue statistics (total, queued, sending, delivered, bounces)
//   - queue-domains: Show top 20 domains by message count with status breakdown
//   - queue-senders: Show top 20 senders by message count with status breakdown
//   - throughput: Display delivery throughput over various time windows
//   - failures: Show last 20 delivery failures with SMTP codes and error messages
//   - callbacks: Display callback queue statistics
//   - reputation: Show IP reputation status for all tracked source IPs
//   - database: Show database health metrics (size, fragmentation, cache hit rate)
//   - health: Query server's comprehensive health endpoint (requires running server)
//   - config: Display current configuration from config.toml
//   - reload: Send SIGHUP signal to server for configuration hot reload
//   - version: Show version information
//
// Command-line flags:
//   - -config: Path to configuration file (default: config.toml)
//   - -json: Output results in JSON format for programmatic consumption
//   - -server: Server address for health command (default: http://localhost:8080)
//
// Database access:
// Most commands open the SQLite database in read-only mode for safety.
// The admin tool uses the same database as the server and is safe to run
// while the server is running due to SQLite's WAL mode concurrent read support.
//
// Usage examples:
//
//	fune-admin queue                          # Show queue statistics
//	fune-admin throughput -json               # Get throughput in JSON format
//	fune-admin queue-domains                  # View messages grouped by domain
//	fune-admin reputation                     # Check IP reputation status
//	fune-admin config -config /etc/fune/config.toml  # View production config
//	fune-admin reload -config /etc/fune/config.toml  # Reload server config
//	fune-admin health -server https://fune.example.com  # Remote health check
//
// Exit codes:
//   - 0: Success
//   - 1: Error (configuration, database, or command execution failure)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"fune/internal/admin"
	"fune/internal/config"
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

// usage provides comprehensive help text for the fune-admin CLI tool.
const usage = `fune-admin - Administration tool for fune SMTP delivery service

Usage:
  fune-admin [command] [flags]

Commands:
  queue           Show queue statistics (total, queued, sending, delivered, bounces, age)
  queue-domains   Show queue grouped by domain (top 20 with status breakdown)
  queue-senders   Show queue grouped by sender (top 20 with status breakdown)
  throughput      Show delivery throughput statistics (1h, 6h, 24h, 7d windows)
  failures        Show recent delivery failures (last 20 with SMTP codes and errors)
  callbacks       Show callback queue statistics (pending, completed)
  reputation      Show IP reputation status (degraded IPs, failure counts, recovery times)
  database        Show database statistics and health (size, fragmentation, cache hit rate)
  health          Show comprehensive health status (cluster, queue, circuit breaker, system)
  config          Show current configuration (all sections with values)
  reload          Reload fune-server configuration (sends SIGHUP signal)
  version         Show version information (version, commit, build date)
  help            Show this help message

Flags:
  -config string   Path to config file (default: config.toml)
  -json            Output in JSON format for programmatic consumption
  -server string   Server address for health command (default: http://localhost:8080)

Examples:
  fune-admin queue                                    # Display current queue status
  fune-admin throughput -json                         # Get throughput metrics in JSON
  fune-admin queue-domains                            # View messages grouped by domain
  fune-admin reputation                               # Check IP reputation tracking
  fune-admin database                                 # Analyze database health
  fune-admin config -config /etc/fune/config.toml     # View production configuration
  fune-admin reload -config /etc/fune/config.toml     # Hot reload server config
  fune-admin health -server https://fune.example.com  # Remote health check with TLS
`

// main is the entry point for fune-admin CLI tool.
//
// It parses command-line arguments, loads configuration if needed, and dispatches
// to the appropriate command handler. Most commands query the SQLite database
// directly, while the health command communicates with the running server via HTTP.
//
// Command execution flow:
//  1. Parse command from os.Args[1]
//  2. Setup flag set for command-specific options
//  3. Load configuration (for database path)
//  4. Execute command handler
//  5. Exit with appropriate status code
//
// The function supports JSON output mode via -json flag for all commands,
// enabling integration with monitoring and automation tools.
func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}

	command := os.Args[1]

	// Handle help command
	if command == "help" || command == "-h" || command == "--help" {
		fmt.Print(usage)
		os.Exit(0)
	}

	// Create flag set
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	configPath := fs.String("config", "config.toml", "Path to config file")
	jsonOutput := fs.Bool("json", false, "Output in JSON format")
	serverAddr := fs.String("server", "http://localhost:8080", "Server address for cluster-status")

	fs.Parse(os.Args[2:])

	// Load config to get database path
	var dbPath string
	if command != "version" && command != "cluster-status" && command != "health" {
		cfg, err := config.LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
		dbPath = cfg.Server.DatabasePath
	}

	// Execute command
	switch command {
	case "queue":
		err := showQueueStats(dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "queue-domains":
		err := showDomainStats(dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "queue-senders":
		err := showSenderStats(dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "throughput":
		err := showThroughput(dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "failures":
		err := showFailures(dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "callbacks":
		err := showCallbacks(dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "reputation":
		err := showReputation(dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "database":
		err := showDatabase(dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "health":
		err := showHealth(*serverAddr, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "config":
		err := showConfig(*configPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "reload":
		err := reloadServer(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "version":
		fmt.Printf("fune-admin version %s\n", version)
		fmt.Printf("  commit: %s\n", commit)
		fmt.Printf("  built:  %s\n", date)

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		fmt.Print(usage)
		os.Exit(1)
	}
}

// showQueueStats displays current queue statistics including message counts by status.
//
// Queries the SQLite database for aggregate queue data:
//   - Total messages across all states
//   - Breakdown by status (queued, sending, delivered, bounces, expired)
//   - Age of oldest queued message (for detecting stalled deliveries)
//
// Parameters:
//   - dbPath: Path to SQLite database file
//   - jsonOutput: If true, output JSON instead of formatted text
//
// Returns an error if database access fails or query execution fails.
func showQueueStats(dbPath string, jsonOutput bool) error {
	db, err := admin.NewAdminDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.GetQueueStats()
	if err != nil {
		return err
	}

	if jsonOutput {
		return outputJSON(stats)
	}

	fmt.Println("Queue Statistics")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("Total messages:      %d\n", stats.Total)
	fmt.Printf("Queued:              %d\n", stats.Queued)
	fmt.Printf("Sending:             %d\n", stats.Sending)
	fmt.Printf("Delivered:           %d\n", stats.Delivered)
	fmt.Printf("Hard bounces:        %d\n", stats.HardBounce)
	fmt.Printf("Temp expired:        %d\n", stats.TempExpired)
	fmt.Printf("Expired:             %d\n", stats.Expired)

	if stats.OldestQueued != nil {
		age := time.Since(*stats.OldestQueued)
		fmt.Printf("Oldest queued:       %s (age: %s)\n", stats.OldestQueued.Format(time.RFC3339), formatDuration(age))
	}

	return nil
}

// showDomainStats displays queue statistics grouped by recipient domain.
//
// Shows the top 20 domains by message count with breakdown of queued, sending,
// and failed messages per domain. This helps identify which domains have
// large message volumes or delivery issues.
//
// Parameters:
//   - dbPath: Path to SQLite database file
//   - jsonOutput: If true, output JSON instead of tabular format
//
// Returns an error if database access fails or query execution fails.
func showDomainStats(dbPath string, jsonOutput bool) error {
	db, err := admin.NewAdminDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.GetDomainStats(20)
	if err != nil {
		return err
	}

	if jsonOutput {
		return outputJSON(stats)
	}

	fmt.Println("Queue by Domain (Top 20)")
	fmt.Println(strings.Repeat("=", 80))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DOMAIN\tTOTAL\tQUEUED\tSENDING\tFAILED")
	fmt.Fprintln(w, strings.Repeat("-", 80))

	for _, s := range stats {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\n",
			s.Domain, s.Count, s.Queued, s.Sending, s.Failed)
	}

	w.Flush()
	return nil
}

// showSenderStats displays queue statistics grouped by sender email address.
//
// Shows the top 20 senders by message count with breakdown of queued, sending,
// and failed messages per sender. This helps identify which senders generate
// the most traffic or have problematic sending patterns.
//
// Parameters:
//   - dbPath: Path to SQLite database file
//   - jsonOutput: If true, output JSON instead of tabular format
//
// Returns an error if database access fails or query execution fails.
func showSenderStats(dbPath string, jsonOutput bool) error {
	db, err := admin.NewAdminDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.GetSenderStats(20)
	if err != nil {
		return err
	}

	if jsonOutput {
		return outputJSON(stats)
	}

	fmt.Println("Queue by Sender (Top 20)")
	fmt.Println(strings.Repeat("=", 80))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SENDER\tTOTAL\tQUEUED\tSENDING\tFAILED")
	fmt.Fprintln(w, strings.Repeat("-", 80))

	for _, s := range stats {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\n",
			s.FromAddr, s.Count, s.Queued, s.Sending, s.Failed)
	}

	w.Flush()
	return nil
}

// showThroughput displays delivery throughput statistics over multiple time windows.
//
// Queries the delivery_attempts table to show:
//   - Last 1 hour: Recent delivery activity
//   - Last 6 hours: Short-term trends
//   - Last 24 hours: Daily delivery volume
//   - Last 7 days: Weekly delivery patterns
//   - Total attempts: Historical cumulative count
//   - Success rate: Percentage of successful deliveries
//
// Parameters:
//   - dbPath: Path to SQLite database file
//   - jsonOutput: If true, output JSON instead of formatted text
//
// Returns an error if database access fails or query execution fails.
func showThroughput(dbPath string, jsonOutput bool) error {
	db, err := admin.NewAdminDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.GetThroughputStats()
	if err != nil {
		return err
	}

	if jsonOutput {
		return outputJSON(stats)
	}

	fmt.Println("Delivery Throughput")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("Last 1 hour:         %d attempts\n", stats.Last1Hour)
	fmt.Printf("Last 6 hours:        %d attempts\n", stats.Last6Hours)
	fmt.Printf("Last 24 hours:       %d attempts\n", stats.Last24Hours)
	fmt.Printf("Last 7 days:         %d attempts\n", stats.Last7Days)
	fmt.Printf("Total attempts:      %d\n", stats.TotalAttempts)
	fmt.Printf("Success rate:        %.2f%%\n", stats.SuccessRate)

	return nil
}

// showFailures displays the most recent delivery failures with error details.
//
// Shows the last 20 failed delivery attempts from the delivery_attempts table,
// including:
//   - Timestamp of failure
//   - Message ID
//   - MX hostname
//   - SMTP response code
//   - Error category (temporary, permanent, network, etc.)
//   - Error message (truncated for display)
//
// This is useful for diagnosing delivery issues and identifying problematic
// recipient domains or network problems.
//
// Parameters:
//   - dbPath: Path to SQLite database file
//   - jsonOutput: If true, output JSON instead of tabular format
//
// Returns an error if database access fails or query execution fails.
func showFailures(dbPath string, jsonOutput bool) error {
	db, err := admin.NewAdminDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	failures, err := db.GetRecentFailures(20)
	if err != nil {
		return err
	}

	if jsonOutput {
		return outputJSON(failures)
	}

	fmt.Println("Recent Delivery Failures (Last 20)")
	fmt.Println(strings.Repeat("=", 100))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tMESSAGE ID\tMX HOST\tCODE\tCATEGORY\tERROR")
	fmt.Fprintln(w, strings.Repeat("-", 100))

	for _, f := range failures {
		timeStr := f.AttemptedAt.Format("15:04:05")
		msgID := truncate(f.MessageID, 20)
		mxHost := truncate(f.MXHost, 25)
		errorMsg := truncate(firstLine(f.Error), 30)
		if errorMsg == "" && f.SMTPResponse != "" {
			errorMsg = truncate(firstLine(f.SMTPResponse), 30)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			timeStr, msgID, mxHost, f.SMTPCode, f.ErrorCategory, errorMsg)
	}

	w.Flush()
	return nil
}

// showCallbacks displays webhook callback queue statistics.
//
// Shows the status of the callback queue including:
//   - Total callbacks generated
//   - Pending callbacks (not yet delivered)
//   - Completed callbacks (successfully delivered)
//
// Callbacks are webhook notifications sent to configured endpoints when
// delivery events occur (success, failure, etc.).
//
// Parameters:
//   - dbPath: Path to SQLite database file
//   - jsonOutput: If true, output JSON instead of formatted text
//
// Returns an error if database access fails or query execution fails.
func showCallbacks(dbPath string, jsonOutput bool) error {
	db, err := admin.NewAdminDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.GetCallbackStats()
	if err != nil {
		return err
	}

	if jsonOutput {
		return outputJSON(stats)
	}

	fmt.Println("Callback Queue Statistics")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("Total callbacks:     %d\n", stats.Total)
	fmt.Printf("Pending:             %d\n", stats.Pending)
	fmt.Printf("Completed:           %d\n", stats.Completed)

	return nil
}

// showDatabase displays comprehensive database health metrics.
//
// Analyzes the SQLite database to show:
//
// Storage metrics:
//   - Database file size in MB
//   - WAL (Write-Ahead Log) size in MB
//   - Page size and count
//
// Performance metrics:
//   - Active database connections
//   - Cache hit ratio (should be >80%)
//   - Fragmentation ratio (warns if >30%)
//   - WAL checkpoint count
//
// Queue depth:
//   - Current queued messages
//   - Current sending messages
//   - Total active workload
//
// Health recommendations:
//   - Suggests VACUUM if fragmentation is high
//   - Warns about low cache hit rates
//   - Alerts on large database sizes
//
// Parameters:
//   - dbPath: Path to SQLite database file
//   - jsonOutput: If true, output JSON instead of formatted text
//
// Returns an error if database access fails or stat queries fail.
func showDatabase(dbPath string, jsonOutput bool) error {
	db, err := admin.NewAdminDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// Get database file stats using the internal queue package
	// We'll need to open a queue connection temporarily
	queueDB, err := admin.GetDatabaseStats(dbPath)
	if err != nil {
		return fmt.Errorf("failed to get database stats: %w", err)
	}

	if jsonOutput {
		return outputJSON(queueDB)
	}

	fmt.Println("Database Statistics")
	fmt.Println(strings.Repeat("=", 90))
	fmt.Println()

	fmt.Println("Storage:")
	fmt.Printf("  Database Size:     %.2f MB\n", float64(queueDB.SizeBytes)/1024/1024)
	fmt.Printf("  WAL Size:          %.2f MB\n", float64(queueDB.WALSizeBytes)/1024/1024)
	fmt.Printf("  Total Size:        %.2f MB\n", float64(queueDB.SizeBytes+queueDB.WALSizeBytes)/1024/1024)
	fmt.Printf("  Page Size:         %d bytes\n", queueDB.PageSize)
	fmt.Printf("  Page Count:        %d\n", queueDB.PageCount)
	fmt.Println()

	fmt.Println("Performance:")
	fmt.Printf("  Active Connections: %d\n", queueDB.Connections)
	fmt.Printf("  Cache Hit Rate:     %.1f%%", queueDB.CacheHitRatio*100)
	if queueDB.CacheHitRatio < 0.8 {
		fmt.Printf(" ⚠️  LOW")
	}
	fmt.Println()
	fmt.Printf("  Fragmentation:      %.1f%%", queueDB.FragmentRatio*100)
	if queueDB.FragmentRatio > 0.3 {
		fmt.Printf(" ⚠️  HIGH (consider VACUUM)")
	}
	fmt.Println()
	fmt.Printf("  WAL Checkpoints:    %d\n", queueDB.WALCheckpoints)
	fmt.Println()

	fmt.Println("Queue Depth:")
	fmt.Printf("  Queued Messages:    %d\n", queueDB.QueuedMessages)
	fmt.Printf("  Sending Messages:   %d\n", queueDB.SendingMessages)
	fmt.Printf("  Total Active:       %d\n", queueDB.QueuedMessages+queueDB.SendingMessages)
	fmt.Println()

	// Health recommendations
	fmt.Println("Recommendations:")
	hasRecommendations := false

	if queueDB.FragmentRatio > 0.3 {
		fmt.Println("  ⚠️  High fragmentation detected. Run 'sqlite3 queue.db \"VACUUM;\"' to compact database")
		hasRecommendations = true
	}
	if queueDB.CacheHitRatio < 0.8 && queueDB.CacheHitRatio > 0 {
		fmt.Println("  ⚠️  Low cache hit rate. Consider increasing SQLite cache_size")
		hasRecommendations = true
	}
	if float64(queueDB.SizeBytes)/1024/1024 > 5000 { // 5GB
		fmt.Println("  ⚠️  Large database size. Consider archiving old messages")
		hasRecommendations = true
	}
	if !hasRecommendations {
		fmt.Println("  ✓ Database health looks good")
	}

	return nil
}

// showReputation displays IP reputation tracking status for all source IPs.
//
// Shows reputation information from the degraded_ips table including:
//   - Source IP address
//   - Status (healthy or degraded)
//   - Failure count
//   - Time since degradation (if applicable)
//   - Last delivery attempt time
//   - SMTP code and response that triggered degradation
//
// Degraded IPs are automatically removed from the rotation pool and retried
// after the configured delay (default: 48 hours). This helps maintain sender
// reputation by avoiding IPs that have been flagged by recipient servers.
//
// Parameters:
//   - dbPath: Path to SQLite database file
//   - jsonOutput: If true, output JSON instead of tabular format
//
// Returns an error if database access fails or query execution fails.
func showReputation(dbPath string, jsonOutput bool) error {
	db, err := admin.NewAdminDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.GetIPReputationStats()
	if err != nil {
		return err
	}

	if jsonOutput {
		return outputJSON(stats)
	}

	if len(stats) == 0 {
		fmt.Println("IP Reputation Status")
		fmt.Println(strings.Repeat("=", 100))
		fmt.Println("No IP reputation data available")
		fmt.Println("\nNote: IP reputation tracking may be disabled in config or no IPs have been tracked yet.")
		return nil
	}

	fmt.Println("IP Reputation Status")
	fmt.Println(strings.Repeat("=", 100))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SOURCE IP\tSTATUS\tFAILURES\tDEGRADED SINCE\tLAST ATTEMPT\tCODE\tRESPONSE")
	fmt.Fprintln(w, strings.Repeat("-", 100))

	for _, info := range stats {
		statusDisplay := info.Status
		if info.Status == "degraded" {
			statusDisplay = "⚠ DEGRADED"
		} else {
			statusDisplay = "✓ healthy"
		}

		degradedSince := "-"
		if info.DegradedAt != nil {
			age := time.Since(*info.DegradedAt)
			degradedSince = formatDuration(age) + " ago"
		}

		lastAttempt := "-"
		if info.LastAttemptAt != nil {
			age := time.Since(*info.LastAttemptAt)
			lastAttempt = formatDuration(age) + " ago"
		}

		response := truncate(firstLine(info.SMTPResponse), 35)
		if response == "" {
			response = "-"
		}

		codeStr := "-"
		if info.SMTPCode > 0 {
			codeStr = fmt.Sprintf("%d", info.SMTPCode)
		}

		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			info.SourceIP, statusDisplay, info.FailureCount,
			degradedSince, lastAttempt, codeStr, response)
	}

	w.Flush()

	// Summary
	degradedCount := 0
	for _, info := range stats {
		if info.Status == "degraded" {
			degradedCount++
		}
	}

	fmt.Printf("\nSummary: %d total IPs tracked, %d degraded, %d healthy\n",
		len(stats), degradedCount, len(stats)-degradedCount)

	return nil
}

// showConfig displays the current server configuration from config.toml.
//
// Loads and pretty-prints the configuration file showing all sections:
//   - [HTTP] - Inbound HTTP API settings
//   - [TLS] - TLS configuration (file-based or Let's Encrypt)
//   - [Server] - Server runtime settings (database, PID file)
//   - [Queue] - Queue and worker pool settings
//   - [Delivery] - SMTP delivery configuration
//   - [Circuit Breaker] - Circuit breaker thresholds
//   - [Callbacks] - Webhook notification settings
//   - [IP Reputation] - IP tracking and rotation settings
//
// This is useful for verifying configuration values in production and
// troubleshooting configuration-related issues.
//
// Parameters:
//   - configPath: Path to configuration file
//   - jsonOutput: If true, output raw config as JSON
//
// Returns an error if config file cannot be loaded or parsed.
func showConfig(configPath string, jsonOutput bool) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if jsonOutput {
		return outputJSON(cfg)
	}

	// Pretty print config
	fmt.Println("Configuration")
	fmt.Println(strings.Repeat("=", 50))

	fmt.Println("\n[HTTP]")
	fmt.Printf("  Listen:              %s\n", cfg.Inbound.Listen)
	fmt.Printf("  Max Body Size:       %d bytes\n", cfg.Inbound.MaxBodySizeBytes)
	fmt.Printf("  Read Timeout:        %ds\n", cfg.Inbound.ReadTimeoutSecs)
	fmt.Printf("  Write Timeout:       %ds\n", cfg.Inbound.WriteTimeoutSecs)
	fmt.Printf("  Metrics Enabled:     %t\n", cfg.Metrics.Enabled)
	fmt.Printf("  Metrics Path:        %s\n", cfg.Metrics.Path)

	fmt.Println("\n[TLS]")
	fmt.Printf("  Enabled:             %t\n", cfg.TLS.Enabled)
	if cfg.TLS.Enabled {
		fmt.Printf("  Provider:            %s\n", cfg.TLS.Provider)
		if cfg.TLS.Provider == "letsencrypt" {
			fmt.Printf("  Email:               %s\n", cfg.TLS.LetsEncrypt.Email)
			fmt.Printf("  Domains:             %v\n", cfg.TLS.LetsEncrypt.Domains)
			fmt.Printf("  Storage:             %s\n", cfg.TLS.LetsEncrypt.StorageProvider)
			if cfg.TLS.LetsEncrypt.StorageProvider == "s3" {
				fmt.Printf("  S3 Bucket:           %s\n", cfg.TLS.LetsEncrypt.S3.Bucket)
				fmt.Printf("  S3 Region:           %s\n", cfg.TLS.LetsEncrypt.S3.Region)
			}
		} else {
			fmt.Printf("  Cert File:           %s\n", cfg.TLS.CertFile)
			fmt.Printf("  Key File:            %s\n", cfg.TLS.KeyFile)
		}
	}

	fmt.Println("\n[Server]")
	fmt.Printf("  Database Path:       %s\n", cfg.Server.DatabasePath)
	fmt.Printf("  PID File:            %s\n", cfg.Server.PIDFile)

	fmt.Println("\n[Queue]")
	fmt.Printf("  Worker Count:        %d\n", cfg.Queue.WorkerCount)
	fmt.Printf("  Batch Size:          %d\n", cfg.Queue.BatchSize)
	fmt.Printf("  Poll Interval:       %ds\n", cfg.Queue.PollIntervalSeconds)

	fmt.Println("\n[Delivery]")
	fmt.Printf("  Source IPs:          %s\n", strings.Join(cfg.Outbound.SourceIPs, ", "))
	fmt.Printf("  IP Selection:        %s\n", cfg.Outbound.SourceIPSelection)
	fmt.Printf("  Max Message Age:     %d hours\n", cfg.Outbound.MaxMessageAgeHours)
	fmt.Printf("  Connection Timeout:  %ds\n", cfg.Outbound.ConnectionTimeoutSeconds)
	fmt.Printf("  SMTP Timeout:        %ds\n", cfg.Outbound.SMTPTimeoutSeconds)
	fmt.Printf("  Initial Retry Delay: %ds\n", cfg.Outbound.InitialRetryDelaySeconds)
	fmt.Printf("  Max Retry Delay:     %ds\n", cfg.Outbound.MaxRetryDelaySeconds)
	fmt.Printf("  Backoff Multiplier:  %.1f\n", cfg.Outbound.BackoffMultiplier)
	fmt.Printf("  Per-Domain Interval: %ds\n", cfg.Outbound.PerDomainIntervalSeconds)
	fmt.Printf("  Per-Domain Retry:    %ds\n", cfg.Outbound.PerDomainRetrySeconds)

	fmt.Println("\n[Circuit Breaker]")
	fmt.Printf("  Enabled:             %t\n", cfg.Outbound.CircuitBreakerEnabled)
	fmt.Printf("  Failure Threshold:   %d\n", cfg.Outbound.CircuitBreakerFailureThreshold)
	fmt.Printf("  Success Threshold:   %d\n", cfg.Outbound.CircuitBreakerSuccessThreshold)
	fmt.Printf("  Open Timeout:        %ds\n", cfg.Outbound.CircuitBreakerOpenTimeoutSecs)

	fmt.Println("\n[Callbacks]")
	fmt.Printf("  Webhook URL:         %s\n", cfg.Callbacks.WebhookURL)
	fmt.Printf("  Timeout:             %ds\n", cfg.Callbacks.TimeoutSeconds)
	fmt.Printf("  Max Age:             %d hours\n", cfg.Callbacks.MaxCallbackAgeHours)
	fmt.Printf("  Initial Retry Delay: %ds\n", cfg.Callbacks.InitialRetryDelaySeconds)
	fmt.Printf("  Max Retry Delay:     %ds\n", cfg.Callbacks.MaxRetryDelaySeconds)
	fmt.Printf("  Backoff Multiplier:  %.1f\n", cfg.Callbacks.BackoffMultiplier)
	fmt.Printf("  Batch Size:          %d\n", cfg.Callbacks.BatchSize)

	fmt.Println("\n[IP Reputation]")
	fmt.Printf("  Enabled:             %t\n", cfg.Reputation.EnableIPTracking)
	if cfg.Reputation.EnableIPTracking {
		fmt.Printf("  Alert Webhook URL:   %s\n", cfg.Reputation.AlertWebhookURL)
		fmt.Printf("  Degraded Retry:      %d hours\n", cfg.Reputation.DegradedRetryHours)
		fmt.Printf("  Cleanup Interval:    %d hours\n", cfg.Reputation.DegradedIPCleanupHours)
		fmt.Printf("  Alert Timeout:       %ds\n", cfg.Reputation.AlertTimeoutSeconds)
	}

	return nil
}

// reloadServer triggers a configuration hot reload on the running fune-server.
//
// This function sends a SIGHUP signal to the server process, which triggers
// configuration reload without restarting the process or dropping connections.
//
// Hot reloadable settings include:
//   - Source IPs for delivery
//   - IP selection strategy
//   - Rate limit intervals
//   - Circuit breaker thresholds
//   - DNS resolver configuration
//   - TLS certificates (file-based only)
//
// Non-reloadable settings (require restart):
//   - Database path
//   - Listen address
//   - Worker count
//
// Process:
//  1. Load config to get PID file path
//  2. Read PID from PID file
//  3. Send SIGHUP signal to process
//
// Parameters:
//   - configPath: Path to configuration file (for locating PID file)
//
// Returns an error if PID file cannot be read, process not found, or signal fails.
func reloadServer(configPath string) error {
	// Load config to get PID file path
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Read PID file
	pidBytes, err := os.ReadFile(cfg.Server.PIDFile)
	if err != nil {
		return fmt.Errorf("failed to read PID file %s: %w", cfg.Server.PIDFile, err)
	}

	pidStr := strings.TrimSpace(string(pidBytes))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return fmt.Errorf("invalid PID in file %s: %s", cfg.Server.PIDFile, pidStr)
	}

	// Find the process
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}

	// Send SIGHUP signal
	err = process.Signal(syscall.SIGHUP)
	if err != nil {
		return fmt.Errorf("failed to send SIGHUP to process %d: %w", pid, err)
	}

	fmt.Printf("Successfully sent reload signal (SIGHUP) to fune-server (PID %d)\n", pid)
	return nil
}

// Helper functions

// outputJSON encodes data as pretty-printed JSON to stdout.
//
// Used by all commands when -json flag is provided. The JSON is indented
// with 2 spaces for readability and ends with a newline.
//
// Parameters:
//   - data: Any JSON-serializable data structure
//
// Returns an error if JSON encoding fails.
func outputJSON(data interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

// formatDuration converts a time.Duration to a human-readable string.
//
// Formats durations intelligently based on magnitude:
//   - < 1 minute: "45s"
//   - < 1 hour: "15m"
//   - < 24 hours: "2h 30m"
//   - >= 24 hours: "5d 12h"
//
// This is used for displaying age of messages, uptime, and time-since values
// in a compact, easy-to-read format.
//
// Parameters:
//   - d: Duration to format
//
// Returns a formatted string representation.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

// truncate shortens a string to maxLen characters, appending "..." if truncated.
//
// Used in tabular output to prevent long strings (error messages, message IDs)
// from breaking table formatting. The "..." suffix indicates truncation.
//
// Parameters:
//   - s: String to potentially truncate
//   - maxLen: Maximum length before truncation
//
// Returns the original string if len(s) <= maxLen, otherwise a truncated version.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// firstLine extracts the first line from a multi-line string.
//
// SMTP error messages and responses often contain multiple lines, but only
// the first line is typically needed for display in tabular output. This
// function splits on CR or LF and returns the first segment.
//
// Parameters:
//   - s: Multi-line string
//
// Returns the first line, or the entire string if no line breaks are found.
func firstLine(s string) string {
	idx := strings.IndexAny(s, "\r\n")
	if idx > 0 {
		return s[:idx]
	}
	return s
}

// ClusterNodeStatus represents the operational status of a cluster node.
//
// This structure is populated from the gossip protocol's node metadata
// and is used by the health command to display cluster-wide status.
type ClusterNodeStatus struct {
	NodeID        string `json:"node_id"`
	QueueDepth    int64  `json:"queue_depth"`
	ActiveWorkers int    `json:"active_workers"`
	Uptime        string `json:"uptime"`
	LastSeen      string `json:"last_seen"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// HealthResponse represents the complete health status from the server's /health endpoint.
//
// This structure matches the JSON response from fune-server's health handler
// and includes comprehensive system, queue, cluster, and circuit breaker status.
type HealthResponse struct {
	Status    string                `json:"status"`
	Timestamp string                `json:"timestamp"`
	Uptime    string                `json:"uptime"`
	Queue     HealthQueue           `json:"queue"`
	Database  *HealthDatabase       `json:"database,omitempty"`
	Cluster   *HealthCluster        `json:"cluster,omitempty"`
	Circuit   *HealthCircuitBreaker `json:"circuit_breaker,omitempty"`
	System    HealthSystem          `json:"system"`
}

// HealthQueue represents queue status within the health response.
type HealthQueue struct {
	Pending   int64 `json:"pending"`
	Active    int64 `json:"active"`
	Failed    int64 `json:"failed"`
	Delivered int64 `json:"delivered"`
	Total     int64 `json:"total"`
}

// HealthCluster represents cluster status within the health response.
//
// Includes gossip protocol information such as node count, leader identity,
// and per-node operational metrics.
type HealthCluster struct {
	Enabled   bool                         `json:"enabled"`
	NodeCount int                          `json:"node_count"`
	Leader    string                       `json:"leader,omitempty"`
	Nodes     map[string]ClusterNodeStatus `json:"nodes,omitempty"`
}

// HealthCircuitBreaker represents circuit breaker status within the health response.
//
// Circuit breaker states: closed (normal), open (failing), half-open (recovering).
type HealthCircuitBreaker struct {
	State         string `json:"state"`
	Failures      uint32 `json:"failures"`
	Successes     uint32 `json:"successes"`
	LastStateTime string `json:"last_state_time"`
}

// HealthDatabase represents database health metrics within the health response.
type HealthDatabase struct {
	SizeMB          float64 `json:"size_mb"`
	WALSizeMB       float64 `json:"wal_size_mb"`
	Connections     int     `json:"connections"`
	FragmentPercent float64 `json:"fragment_percent"`
	CacheHitPercent float64 `json:"cache_hit_percent"`
	QueuedMessages  int64   `json:"queued_messages"`
	SendingMessages int64   `json:"sending_messages"`
}

// HealthSystem represents system resource metrics within the health response.
type HealthSystem struct {
	GoVersion     string `json:"go_version"`
	Goroutines    int    `json:"goroutines"`
	MemoryMB      uint64 `json:"memory_mb"`
	MemoryAllocMB uint64 `json:"memory_alloc_mb"`
}

// showHealth queries the server's /health endpoint and displays comprehensive status.
//
// Unlike other commands that query the database directly, this command makes
// an HTTP GET request to the running server's health endpoint. This requires
// the server to be running and accessible.
//
// The health endpoint provides:
//   - Overall service status (healthy or degraded)
//   - Queue depth and message counts
//   - Database health metrics (size, fragmentation, cache hit rate)
//   - Cluster information (if gossip is enabled)
//   - Circuit breaker state
//   - System resources (memory, goroutines)
//
// This is the preferred command for monitoring and alerting integrations
// as it provides a single comprehensive view of server health.
//
// Parameters:
//   - serverAddr: Base URL of the fune-server (e.g., http://localhost:8080)
//   - jsonOutput: If true, output JSON instead of formatted text
//
// Returns an error if HTTP request fails or response cannot be parsed.
func showHealth(serverAddr string, jsonOutput bool) error {
	url := fmt.Sprintf("%s/health", serverAddr)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch health status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(body))
	}

	var health HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if jsonOutput {
		return outputJSON(health)
	}

	fmt.Println("Health Status")
	fmt.Println(strings.Repeat("=", 90))
	fmt.Printf("Status:    %s\n", strings.ToUpper(health.Status))
	fmt.Printf("Timestamp: %s\n", health.Timestamp)
	fmt.Printf("Uptime:    %s\n\n", health.Uptime)

	fmt.Println("Queue:")
	fmt.Printf("  Pending:   %d\n\n", health.Queue.Pending)

	if health.Database != nil {
		fmt.Println("Database:")
		fmt.Printf("  Size:           %.2f MB\n", health.Database.SizeMB)
		fmt.Printf("  WAL Size:       %.2f MB\n", health.Database.WALSizeMB)
		fmt.Printf("  Connections:    %d\n", health.Database.Connections)
		fmt.Printf("  Fragment:       %.1f%%", health.Database.FragmentPercent)
		if health.Database.FragmentPercent > 30 {
			fmt.Printf(" ⚠️  HIGH")
		}
		fmt.Println()
		fmt.Printf("  Cache Hit Rate: %.1f%%\n", health.Database.CacheHitPercent)
		fmt.Printf("  Queued:         %d messages\n", health.Database.QueuedMessages)
		fmt.Printf("  Sending:        %d messages\n\n", health.Database.SendingMessages)
	}

	if health.Cluster != nil {
		fmt.Println("Cluster:")
		fmt.Printf("  Enabled:    %t\n", health.Cluster.Enabled)
		if health.Cluster.Enabled {
			fmt.Printf("  Node Count: %d\n", health.Cluster.NodeCount)
			fmt.Printf("  Leader:     %s\n", health.Cluster.Leader)
			if len(health.Cluster.Nodes) > 0 {
				fmt.Println("\n  Nodes:")
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "  NODE ID\tQUEUE DEPTH\tACTIVE WORKERS\tUPTIME\tLAST SEEN")
				for _, node := range health.Cluster.Nodes {
					lastSeen, _ := time.Parse(time.RFC3339, node.LastSeen)
					lastSeenAgo := time.Since(lastSeen)
					fmt.Fprintf(w, "  %s\t%d\t%d\t%s\t%s ago\n",
						node.NodeID, node.QueueDepth, node.ActiveWorkers, node.Uptime, formatDuration(lastSeenAgo))
				}
				w.Flush()
			}
		}
		fmt.Println()
	}

	if health.Circuit != nil {
		fmt.Println("Circuit Breaker:")
		fmt.Printf("  State:          %s\n", strings.ToUpper(health.Circuit.State))
		fmt.Printf("  Failures:       %d\n", health.Circuit.Failures)
		fmt.Printf("  Successes:      %d\n", health.Circuit.Successes)
		fmt.Printf("  Last State At:  %s\n\n", health.Circuit.LastStateTime)
	}

	fmt.Println("System:")
	fmt.Printf("  Go Version:     %s\n", health.System.GoVersion)
	fmt.Printf("  Goroutines:     %d\n", health.System.Goroutines)
	fmt.Printf("  Memory (Total): %d MB\n", health.System.MemoryMB)
	fmt.Printf("  Memory (Alloc): %d MB\n", health.System.MemoryAllocMB)

	return nil
}
