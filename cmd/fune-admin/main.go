package main

import (
	"encoding/json"
	"flag"
	"fmt"
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
  queue         Show queue statistics
  queue-domains Show queue grouped by domain (top 20)
  queue-senders Show queue grouped by sender (top 20)
  throughput    Show delivery throughput statistics
  failures      Show recent delivery failures (last 20)
  callbacks     Show callback queue statistics
  config        Show current configuration
  reload        Reload fune-server configuration (sends SIGHUP)
  version       Show version information
  help          Show this help message

Flags:
  -db string       Path to database file (default: fune.db)
  -config string   Path to config file (default: config.toml)
  -json            Output in JSON format
  -pid string      Path to PID file for reload command (default: fune.pid)

Examples:
  fune-admin queue -db /var/lib/fune/fune.db
  fune-admin throughput -json
  fune-admin queue-domains
  fune-admin config -config /etc/fune/config.toml
  fune-admin reload -pid /var/run/fune.pid
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
	dbPath := fs.String("db", "fune.db", "Path to database file")
	configPath := fs.String("config", "config.toml", "Path to config file")
	pidPath := fs.String("pid", "fune.pid", "Path to PID file")
	jsonOutput := fs.Bool("json", false, "Output in JSON format")

	fs.Parse(os.Args[2:])

	// Execute command
	switch command {
	case "queue":
		err := showQueueStats(*dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "queue-domains":
		err := showDomainStats(*dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "queue-senders":
		err := showSenderStats(*dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "throughput":
		err := showThroughput(*dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "failures":
		err := showFailures(*dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "callbacks":
		err := showCallbacks(*dbPath, *jsonOutput)
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
		err := reloadServer(*pidPath)
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

	fmt.Println("\n[Queue]")
	fmt.Printf("  Database Path:       %s\n", cfg.Queue.DatabasePath)
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
	fmt.Printf("  Rate Limit Interval: %ds\n", cfg.Delivery.MinDeliveryIntervalSeconds)

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

	return nil
}

func reloadServer(pidPath string) error {
	// Read PID file
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("failed to read PID file %s: %w", pidPath, err)
	}

	pidStr := strings.TrimSpace(string(pidBytes))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return fmt.Errorf("invalid PID in file %s: %s", pidPath, pidStr)
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
