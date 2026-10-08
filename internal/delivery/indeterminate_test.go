package delivery

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"strela/internal/config"
)

// slowAckServer is a fake SMTP/LMTP server that accepts every command and
// counts received message bodies. For bodies selected by delayBody it waits
// replyDelay before sending the final 250, simulating an MX that scans a
// message before acknowledging it (and still accepts it).
type slowAckServer struct {
	addr   string
	port   int
	bodies atomic.Int64
}

func startSlowAckServer(t *testing.T, replyDelay time.Duration, delayBody func(n int64) bool) *slowAckServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	s := &slowAckServer{addr: ln.Addr().String(), port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, replyDelay, delayBody)
		}
	}()
	return s
}

func (s *slowAckServer) serve(conn net.Conn, replyDelay time.Duration, delayBody func(n int64) bool) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	conn.Write([]byte("220 fake ESMTP\r\n"))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			conn.Write([]byte("250-fake\r\n250 8BITMIME\r\n"))
		case cmd == "DATA":
			conn.Write([]byte("354 go ahead\r\n"))
			for {
				l, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
			}
			if n := s.bodies.Add(1); delayBody(n) {
				time.Sleep(replyDelay)
			}
			conn.Write([]byte("250 2.0.0 OK queued\r\n"))
		case cmd == "QUIT":
			conn.Write([]byte("221 bye\r\n"))
			return
		default:
			conn.Write([]byte("250 OK\r\n"))
		}
	}
}

func newSlowAckDeliverer(t *testing.T, cfg *config.OutboundConfig, sourceIPs []string) *Deliverer {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mxLookup := NewMXLookup(&config.DNSConfig{TimeoutSeconds: 1}, logger)
	d := NewDeliverer(cfg, &config.ExpandedSourceIPs{IPv4: sourceIPs}, mxLookup, logger, &config.ReputationConfig{}, nil, nil)
	t.Cleanup(d.Stop)
	return d
}

var slowAckMsg = []byte("From: a@example.com\r\nTo: b@example.net\r\nSubject: x\r\n\r\nhi\r\n")

// TestUnknownNotResentToNextSourceIP reproduces the production incident: the MX
// accepts the message but its reply to the final "." arrives after the wait
// expires. The result is "unknown" and must be returned to the caller - not
// retried from the next source IP, which would deliver a duplicate, and not
// later overwritten by a "timeout" (which tells the caller retrying is safe).
func TestUnknownNotResentToNextSourceIP(t *testing.T) {
	srv := startSlowAckServer(t, 2*time.Second, func(int64) bool { return true })
	d := newSlowAckDeliverer(t, &config.OutboundConfig{
		DefaultSMTPDestination:        srv.addr,
		SMTPIPMode:                    config.IPModeIPv4,
		HelloHostname:                 "test.example.com",
		ConnectionTimeoutSeconds:      5,
		BannerTimeoutSeconds:          5,
		HandshakeTimeoutSeconds:       5,
		SMTPTimeoutSeconds:            5,
		DataTerminationTimeoutSeconds: 1,
		MaxTotalDeliverySeconds:       30,
	}, []string{"127.0.0.1", "127.0.0.1", "127.0.0.1"})

	result := d.DeliverMessage(t.Context(), "a@example.com", "b@example.net", slowAckMsg,
		"smtp", "", "", "", false, "", "", "", nil)

	if result.Status != "unknown" {
		t.Errorf("status = %q, want \"unknown\" (error: %s)", result.Status, result.Error)
	}
	if got := srv.bodies.Load(); got != 1 {
		t.Errorf("MX received %d copies of the message, want exactly 1", got)
	}
}

// TestUnknownOnPooledConnectionNotResent verifies that an "unknown" outcome on a
// reused connection is not retried on a fresh connection: the body was fully
// sent on the pooled connection, so that fallback would send a duplicate.
func TestUnknownOnPooledConnectionNotResent(t *testing.T) {
	// First message is acknowledged immediately (connection gets pooled); the
	// second one, sent on the pooled connection, is acknowledged too late.
	srv := startSlowAckServer(t, 2*time.Second, func(n int64) bool { return n == 2 })
	d := newDialTestDeliverer(t, srv.port)
	d.config.DataTerminationTimeoutSeconds = 1
	t.Cleanup(d.Stop)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	deliver := func() DeliveryResult {
		return d.attemptDelivery(ctx, testLogger(), "trace", "a@example.com", "b@example.net", slowAckMsg,
			"127.0.0.1", []string{"127.0.0.1"}, "", false, config.ProtocolSMTP, nil, d.config)
	}

	if r := deliver(); r.Status != "delivered" {
		t.Fatalf("first delivery: status = %q, want delivered (error: %s)", r.Status, r.Error)
	}
	if r := deliver(); r.Status != "unknown" {
		t.Errorf("second delivery: status = %q, want \"unknown\" (error: %s)", r.Status, r.Error)
	}
	if got := srv.bodies.Load(); got != 2 {
		t.Errorf("MX received %d bodies for 2 messages, want 2 (duplicate sent)", got)
	}
}

// TestSlowDataReplyWithinBudgetIsDelivered verifies that the wait for the reply
// to the final "." uses data_termination_timeout_seconds rather than the much
// shorter smtp_timeout_seconds, so a slow-but-accepting MX yields "delivered".
func TestSlowDataReplyWithinBudgetIsDelivered(t *testing.T) {
	srv := startSlowAckServer(t, 2*time.Second, func(int64) bool { return true })
	d := newSlowAckDeliverer(t, &config.OutboundConfig{
		DefaultSMTPDestination:        srv.addr,
		HelloHostname:                 "test.example.com",
		ConnectionTimeoutSeconds:      5,
		BannerTimeoutSeconds:          5,
		HandshakeTimeoutSeconds:       5,
		SMTPTimeoutSeconds:            1, // shorter than the MX's reply delay
		DataTerminationTimeoutSeconds: 10,
		MaxTotalDeliverySeconds:       30,
	}, nil)

	result := d.DeliverMessage(t.Context(), "a@example.com", "b@example.net", slowAckMsg,
		"smtp", "", "", "", false, "", "", "", nil)

	if result.Status != "delivered" || result.SMTPCode != 250 {
		t.Errorf("status = %q code = %d, want delivered/250 (error: %s)", result.Status, result.SMTPCode, result.Error)
	}
	if got := srv.bodies.Load(); got != 1 {
		t.Errorf("MX received %d copies of the message, want exactly 1", got)
	}
}

// TestLMTPMissingFinalReplyIsUnknown verifies the LMTP path follows the same
// contract: a lost reply after the body was fully sent is "unknown", not a
// retry-safe "timeout".
func TestLMTPMissingFinalReplyIsUnknown(t *testing.T) {
	srv := startSlowAckServer(t, 2*time.Second, func(int64) bool { return true })
	conn, reader := lmtpHandshake(t, srv.addr)

	d := &Deliverer{config: &config.OutboundConfig{LMTPTimeoutSeconds: 1}}
	result := d.performLMTPTransaction(t.Context(), testLogger(), "trace", conn, reader,
		"a@example.com", "b@example.net", slowAckMsg, "localhost", "")

	if result.Status != "unknown" {
		t.Errorf("status = %q, want \"unknown\" (error: %s)", result.Status, result.Error)
	}
}

func TestDataTerminationTimeout(t *testing.T) {
	cfg := &config.OutboundConfig{DataTerminationTimeoutSeconds: 600}

	if got := dataTerminationTimeout(context.Background(), cfg, time.Minute); got != 600*time.Second {
		t.Errorf("no deadline: got %v, want 600s", got)
	}
	if got := dataTerminationTimeout(context.Background(), &config.OutboundConfig{}, time.Minute); got != time.Minute {
		t.Errorf("unset config: got %v, want fallback 1m", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if got := dataTerminationTimeout(ctx, cfg, time.Minute); got > 120*time.Second || got < 119*time.Second {
		t.Errorf("120s left: got %v, want ~120s (capped by deadline, not scaled)", got)
	}

	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	if got := dataTerminationTimeout(short, cfg, time.Minute); got != minSMTPTimeout {
		t.Errorf("1s left: got %v, want floor %v", got, minSMTPTimeout)
	}
}

func TestIsFinalResult(t *testing.T) {
	for status, want := range map[string]bool{
		"delivered":   true,
		"hard_bounce": true,
		"temp_fail":   true,
		"unknown":     true,
		"timeout":     false,
		"error":       false,
		"":            false,
	} {
		if got := isFinalResult(status); got != want {
			t.Errorf("isFinalResult(%q) = %v, want %v", status, got, want)
		}
	}
}
