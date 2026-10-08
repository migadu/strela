package delivery

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"strela/internal/config"
)

// stallServer is a fake SMTP/LMTP server that counts accepted connections.
// With stall set it answers DATA with 354 and then stops reading, so a large
// body fills the TCP window and the client's write blocks.
type stallServer struct {
	addr    string
	port    int
	accepts atomic.Int64
}

func startStallServer(t *testing.T, stall bool) *stallServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); ln.Close() })

	s := &stallServer{addr: ln.Addr().String(), port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepts.Add(1)
			go s.serve(conn, stall, done)
		}
	}()
	return s
}

func (s *stallServer) serve(conn net.Conn, stall bool, done <-chan struct{}) {
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
			if stall {
				<-done
				return
			}
			for {
				l, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
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

// largeMsg is big enough to overflow loopback socket buffers, so writing it to
// a server that stopped reading blocks.
var largeMsg = append([]byte("Subject: big\r\n\r\n"),
	bytes.Repeat([]byte(strings.Repeat("x", 76)+"\r\n"), 500000)...)

// TestStalledBodyWriteReturnsAtDeadline verifies that an MX which stops reading
// the message body cannot hold a request past its delivery budget. go-smtp sets
// no deadline while the body is written, so only the ctx hook unblocks it.
func TestStalledBodyWriteReturnsAtDeadline(t *testing.T) {
	srv := startStallServer(t, true)
	d := newDialTestDeliverer(t, srv.port)
	t.Cleanup(d.Stop)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	start := time.Now()
	done := make(chan DeliveryResult, 1)
	go func() {
		done <- d.attemptDelivery(ctx, testLogger(), "trace", "a@example.com", "b@example.net", largeMsg,
			"127.0.0.1", []string{"127.0.0.1"}, "", false, config.ProtocolSMTP, nil, d.config)
	}()

	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("returned after %v, want close to the 2s budget", elapsed)
		}
		// The body was never fully sent, so retrying is safe: "timeout", not "unknown".
		if r.Status != "timeout" {
			t.Errorf("status = %q, want \"timeout\" (error: %s)", r.Status, r.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("delivery still blocked 10s after a 2s budget: stalled body write is not bounded by ctx")
	}
}

// TestFinishedRequestDoesNotClosePooledConnection verifies the ctx hook is
// released after a successful delivery: the handler cancels ctx when the request
// ends, and that must not close the connection just returned to the pool.
func TestFinishedRequestDoesNotClosePooledConnection(t *testing.T) {
	srv := startStallServer(t, false)
	d := newDialTestDeliverer(t, srv.port)
	t.Cleanup(d.Stop)

	deliver := func() DeliveryResult {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel() // like the handler: ctx always ends after the request
		return d.attemptDelivery(ctx, testLogger(), "trace", "a@example.com", "b@example.net", slowAckMsg,
			"127.0.0.1", []string{"127.0.0.1"}, "", false, config.ProtocolSMTP, nil, d.config)
	}

	if r := deliver(); r.Status != "delivered" {
		t.Fatalf("first delivery: status = %q, want delivered (error: %s)", r.Status, r.Error)
	}
	accepts := srv.accepts.Load()

	if r := deliver(); r.Status != "delivered" {
		t.Fatalf("second delivery: status = %q, want delivered (error: %s)", r.Status, r.Error)
	}
	if got := srv.accepts.Load(); got != accepts {
		t.Errorf("second delivery opened %d new connection(s); want the pooled one reused", got-accepts)
	}
}

// TestLMTPStalledBodyWriteReturnsAtDeadline verifies the LMTP path is bounded
// by the delivery budget, not only by lmtp_timeout_seconds.
func TestLMTPStalledBodyWriteReturnsAtDeadline(t *testing.T) {
	srv := startStallServer(t, true)
	conn, reader := lmtpHandshake(t, srv.addr)

	d := &Deliverer{config: &config.OutboundConfig{LMTPTimeoutSeconds: 60}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	start := time.Now()
	result := d.performLMTPTransaction(ctx, testLogger(), "trace", conn, reader,
		"a@example.com", "b@example.net", largeMsg, "localhost", "")

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("returned after %v, want close to the 2s budget (lmtp_timeout is 60s)", elapsed)
	}
	if result.Status != "timeout" {
		t.Errorf("status = %q, want \"timeout\" (error: %s)", result.Status, result.Error)
	}
}
