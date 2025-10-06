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

// Version information, injected at build time
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `fune-admin - Administration tool for fune SMTP delivery service

Usage:
  fune-admin [command] [flags]

Commands:
  queue           Show queue statistics
  queue-domains   Show queue grouped by domain (top 20)
  queue-senders   Show queue grouped by sender (top 20)
  throughput      Show delivery throughput statistics
  failures        Show recent delivery failures (last 20)
  callbacks       Show callback queue statistics
  reputation      Show IP reputation status
  cluster-status  Show gossip cluster status
  config          Show current configuration
  reload          Reload fune-server configuration (sends SIGHUP)
  version         Show version information
  help            Show this help message

Flags:
  -config string   Path to config file (default: config.toml)
  -json            Output in JSON format
  -server string   Server address for cluster-status (default: http://localhost:8080)

Examples:
  fune-admin queue
  fune-admin throughput -json
  fune-admin queue-domains
  fune-admin reputation
  fune-admin cluster-status -server http://fune-1:8080
  fune-admin config -config /etc/fune/config.toml
  fune-admin reload -config /etc/fune/config.toml
`

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
	if command != "version" && command != "cluster-status" {
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

	case "cluster-status":
		err := showClusterStatus(*serverAddr, *jsonOutput)
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
	fmt.Printf("  Listen:              %s\n", cfg.HTTP.Listen)
	fmt.Printf("  TLS Enabled:         %t\n", cfg.HTTP.TLSEnabled)
	if cfg.HTTP.TLSEnabled {
		fmt.Printf("  TLS Cert:            %s\n", cfg.HTTP.TLSCertFile)
		fmt.Printf("  TLS Key:             %s\n", cfg.HTTP.TLSKeyFile)
	}
	fmt.Printf("  Max Body Size:       %d bytes\n", cfg.HTTP.MaxBodySizeBytes)
	fmt.Printf("  Read Timeout:        %ds\n", cfg.HTTP.ReadTimeoutSecs)
	fmt.Printf("  Write Timeout:       %ds\n", cfg.HTTP.WriteTimeoutSecs)
	fmt.Printf("  Metrics Enabled:     %t\n", cfg.HTTP.MetricsEnabled)
	fmt.Printf("  Metrics Path:        %s\n", cfg.HTTP.MetricsPath)

	fmt.Println("\n[Server]")
	fmt.Printf("  Database Path:       %s\n", cfg.Server.DatabasePath)
	fmt.Printf("  PID File:            %s\n", cfg.Server.PIDFile)

	fmt.Println("\n[Queue]")
	fmt.Printf("  Worker Count:        %d\n", cfg.Queue.WorkerCount)
	fmt.Printf("  Batch Size:          %d\n", cfg.Queue.BatchSize)
	fmt.Printf("  Poll Interval:       %ds\n", cfg.Queue.PollIntervalSeconds)

	fmt.Println("\n[Delivery]")
	fmt.Printf("  Source IPs:          %s\n", strings.Join(cfg.Delivery.SourceIPs, ", "))
	fmt.Printf("  IP Selection:        %s\n", cfg.Delivery.IPSelection)
	fmt.Printf("  Max Message Age:     %d hours\n", cfg.Delivery.MaxMessageAgeHours)
	fmt.Printf("  Connection Timeout:  %ds\n", cfg.Delivery.ConnectionTimeoutSeconds)
	fmt.Printf("  SMTP Timeout:        %ds\n", cfg.Delivery.SMTPTimeoutSeconds)
	fmt.Printf("  Initial Retry Delay: %ds\n", cfg.Delivery.InitialRetryDelaySeconds)
	fmt.Printf("  Max Retry Delay:     %ds\n", cfg.Delivery.MaxRetryDelaySeconds)
	fmt.Printf("  Backoff Multiplier:  %.1f\n", cfg.Delivery.BackoffMultiplier)
	fmt.Printf("  Per-Domain Interval: %ds\n", cfg.Delivery.PerDomainIntervalSeconds)
	fmt.Printf("  Per-Domain Retry:    %ds\n", cfg.Delivery.PerDomainRetrySeconds)

	fmt.Println("\n[Circuit Breaker]")
	fmt.Printf("  Enabled:             %t\n", cfg.Delivery.CircuitBreakerEnabled)
	fmt.Printf("  Failure Threshold:   %d\n", cfg.Delivery.CircuitBreakerFailureThreshold)
	fmt.Printf("  Success Threshold:   %d\n", cfg.Delivery.CircuitBreakerSuccessThreshold)
	fmt.Printf("  Open Timeout:        %ds\n", cfg.Delivery.CircuitBreakerOpenTimeoutSecs)

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

func outputJSON(data interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

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

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func firstLine(s string) string {
	idx := strings.IndexAny(s, "\r\n")
	if idx > 0 {
		return s[:idx]
	}
	return s
}

// ClusterNodeStatus represents the status of a cluster node
type ClusterNodeStatus struct {
	NodeID        string `json:"node_id"`
	QueueDepth    int64  `json:"queue_depth"`
	ActiveWorkers int    `json:"active_workers"`
	Uptime        string `json:"uptime"`
	LastSeen      string `json:"last_seen"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// ClusterStatusResponse represents the cluster status API response
type ClusterStatusResponse struct {
	Timestamp string                       `json:"timestamp"`
	Nodes     map[string]ClusterNodeStatus `json:"nodes"`
	NodeCount int                          `json:"node_count"`
}

func showClusterStatus(serverAddr string, jsonOutput bool) error {
	// Make HTTP request to cluster status endpoint
	url := fmt.Sprintf("%s/admin/cluster/status", serverAddr)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch cluster status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var clusterStatus ClusterStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&clusterStatus); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if jsonOutput {
		return outputJSON(clusterStatus)
	}

	// Display in human-readable format
	fmt.Println("Cluster Status")
	fmt.Println(strings.Repeat("=", 90))
	fmt.Printf("Timestamp: %s\n", clusterStatus.Timestamp)
	fmt.Printf("Node Count: %d\n\n", clusterStatus.NodeCount)

	if len(clusterStatus.Nodes) == 0 {
		fmt.Println("No nodes found in cluster (gossip may be disabled)")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE ID\tQUEUE DEPTH\tACTIVE WORKERS\tUPTIME\tLAST SEEN")
	fmt.Fprintln(w, strings.Repeat("-", 90))

	for _, node := range clusterStatus.Nodes {
		lastSeen, _ := time.Parse(time.RFC3339, node.LastSeen)
		lastSeenAgo := time.Since(lastSeen)

		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s ago\n",
			node.NodeID,
			node.QueueDepth,
			node.ActiveWorkers,
			node.Uptime,
			formatDuration(lastSeenAgo))
	}

	w.Flush()
	return nil
}
