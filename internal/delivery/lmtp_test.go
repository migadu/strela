package delivery

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// mockLMTPServerCapture is like mockLMTPServer but also captures the MAIL FROM
// and RCPT TO command lines for assertion.
func mockLMTPServerCapture(t *testing.T, ln net.Listener, responseCode int, mailFromCh, rcptToCh chan<- string, errCh chan<- error) {
	t.Helper()

	conn, err := ln.Accept()
	if err != nil {
		errCh <- err
		return
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	fmt.Fprintf(conn, "220 mock LMTP ready\r\n")

	line, err := reader.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("reading LHLO: %w", err)
		return
	}
	if !strings.HasPrefix(line, "LHLO ") {
		errCh <- fmt.Errorf("expected LHLO, got: %s", line)
		return
	}
	fmt.Fprintf(conn, "250 OK\r\n")

	line, err = reader.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("reading MAIL FROM: %w", err)
		return
	}
	mailFromCh <- strings.TrimRight(line, "\r\n")
	fmt.Fprintf(conn, "250 OK\r\n")

	line, err = reader.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("reading RCPT TO: %w", err)
		return
	}
	rcptToCh <- strings.TrimRight(line, "\r\n")
	fmt.Fprintf(conn, "250 OK\r\n")

	line, err = reader.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("reading DATA: %w", err)
		return
	}
	if !strings.HasPrefix(line, "DATA") {
		errCh <- fmt.Errorf("expected DATA, got: %s", line)
		return
	}
	fmt.Fprintf(conn, "354 Go ahead\r\n")

	// Drain body until lone ".\r\n"
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			errCh <- fmt.Errorf("reading body: %w", err)
			return
		}
		if strings.TrimRight(line, "\r\n") == "." {
			break
		}
	}

	fmt.Fprintf(conn, "%d 2.1.5 Delivered\r\n", responseCode)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader.ReadString('\n')
	errCh <- nil
}

// mockLMTPServer accepts one LMTP connection and runs through the full
// protocol: greeting → LHLO → MAIL FROM → RCPT TO → DATA → body → per-recipient response.
func mockLMTPServer(t *testing.T, ln net.Listener, responseCode int, bodyCh chan<- []byte, errCh chan<- error) {
	t.Helper()

	conn, err := ln.Accept()
	if err != nil {
		errCh <- err
		return
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Greeting
	fmt.Fprintf(conn, "220 mock LMTP ready\r\n")

	// LHLO
	line, err := reader.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("reading LHLO: %w", err)
		return
	}
	if !strings.HasPrefix(line, "LHLO ") {
		errCh <- fmt.Errorf("expected LHLO, got: %s", line)
		return
	}
	fmt.Fprintf(conn, "250 OK\r\n")

	// MAIL FROM
	line, err = reader.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("reading MAIL FROM: %w", err)
		return
	}
	if !strings.HasPrefix(line, "MAIL FROM:") {
		errCh <- fmt.Errorf("expected MAIL FROM, got: %s", line)
		return
	}
	fmt.Fprintf(conn, "250 OK\r\n")

	// RCPT TO
	line, err = reader.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("reading RCPT TO: %w", err)
		return
	}
	if !strings.HasPrefix(line, "RCPT TO:") {
		errCh <- fmt.Errorf("expected RCPT TO, got: %s", line)
		return
	}
	fmt.Fprintf(conn, "250 OK\r\n")

	// DATA
	line, err = reader.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("reading DATA: %w", err)
		return
	}
	if !strings.HasPrefix(line, "DATA") {
		errCh <- fmt.Errorf("expected DATA, got: %s", line)
		return
	}
	fmt.Fprintf(conn, "354 Go ahead\r\n")

	// Read message body until lone ".\r\n"
	var body strings.Builder
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			errCh <- fmt.Errorf("reading body: %w", err)
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			break
		}
		// Un-dot-stuff
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}
		body.WriteString(trimmed)
		body.WriteString("\n")
	}

	bodyCh <- []byte(body.String())

	// Per-recipient response
	fmt.Fprintf(conn, "%d 2.1.5 Delivered\r\n", responseCode)

	// Read QUIT (best-effort from client)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader.ReadString('\n')

	errCh <- nil
}

// lmtpHandshake connects to the mock server, reads greeting, sends LHLO.
func lmtpHandshake(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)

	code, _, err := readLMTPResponse(reader, conn, 5*time.Second)
	if err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if code != 220 {
		t.Fatalf("expected 220, got %d", code)
	}

	writeLMTPCommand(conn, 5*time.Second, "LHLO test.local")
	code, _, err = readLMTPResponse(reader, conn, 5*time.Second)
	if err != nil {
		t.Fatalf("LHLO: %v", err)
	}
	if code != 250 {
		t.Fatalf("expected 250, got %d", code)
	}

	return conn, reader
}

func TestPerformLMTPTransaction_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	bodyCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go mockLMTPServer(t, ln, 250, bodyCh, errCh)

	conn, reader := lmtpHandshake(t, ln.Addr().String())

	msg := []byte("From: sender@example.com\r\nTo: rcpt@example.com\r\nSubject: Test\r\n\r\nHello world\r\n")

	result := (&Deliverer{config: &defaultTestConfig}).performLMTPTransaction(
		t.Context(), testLogger(), "trace-1", conn, reader,
		"sender@example.com", "rcpt@example.com", msg, "localhost", "",
	)

	if result.Status != "delivered" {
		t.Fatalf("expected delivered, got %s (error: %s)", result.Status, result.Error)
	}
	if result.SMTPCode != 250 {
		t.Errorf("expected code 250, got %d", result.SMTPCode)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("server error: %v", err)
	}

	body := <-bodyCh
	if !strings.Contains(string(body), "Hello world") {
		t.Errorf("body mismatch: %s", string(body)[:min(len(body), 200)])
	}
}

func TestPerformLMTPTransaction_HardBounce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	bodyCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go mockLMTPServer(t, ln, 550, bodyCh, errCh)

	conn, reader := lmtpHandshake(t, ln.Addr().String())

	msg := []byte("From: a@b.com\r\nTo: c@d.com\r\n\r\nbody\r\n")
	result := (&Deliverer{config: &defaultTestConfig}).performLMTPTransaction(
		t.Context(), testLogger(), "trace-2", conn, reader,
		"a@b.com", "c@d.com", msg, "localhost", "",
	)

	if result.Status != "hard_bounce" {
		t.Fatalf("expected hard_bounce, got %s", result.Status)
	}
	if result.SMTPCode != 550 {
		t.Errorf("expected code 550, got %d", result.SMTPCode)
	}

	<-errCh
}

func TestPerformLMTPTransaction_LargeMessage(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	bodyCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go mockLMTPServer(t, ln, 250, bodyCh, errCh)

	conn, reader := lmtpHandshake(t, ln.Addr().String())

	// Build a ~10MB RFC 822 message with a base64-encoded attachment
	var msgBuf strings.Builder
	msgBuf.WriteString("From: sender@example.com\r\n")
	msgBuf.WriteString("To: rcpt@example.com\r\n")
	msgBuf.WriteString("Subject: Large attachment test\r\n")
	msgBuf.WriteString("MIME-Version: 1.0\r\n")
	msgBuf.WriteString("Content-Type: application/octet-stream\r\n")
	msgBuf.WriteString("Content-Transfer-Encoding: base64\r\n")
	msgBuf.WriteString("\r\n")

	// ~10MB of random data, base64 encoded in 76-char lines
	rawSize := 10 * 1024 * 1024
	randomData := make([]byte, rawSize)
	if _, err := io.ReadFull(rand.Reader, randomData); err != nil {
		t.Fatalf("generating random data: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(randomData)
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		msgBuf.WriteString(encoded[i:end])
		msgBuf.WriteString("\r\n")
	}

	msg := []byte(msgBuf.String())
	t.Logf("message size: %d bytes (~%.1f MB)", len(msg), float64(len(msg))/(1024*1024))

	result := (&Deliverer{config: &defaultTestConfig}).performLMTPTransaction(
		t.Context(), testLogger(), "trace-large", conn, reader,
		"sender@example.com", "rcpt@example.com", msg, "localhost", "",
	)

	if result.Status != "delivered" {
		t.Fatalf("expected delivered, got %s (error: %s)", result.Status, result.Error)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("server error: %v", err)
	}

	receivedBody := <-bodyCh
	// Verify the received body is roughly the right size
	// (allow variance from line ending normalization)
	expectedMinSize := len(msg) * 80 / 100
	if len(receivedBody) < expectedMinSize {
		t.Errorf("received body too small: %d bytes, expected at least %d", len(receivedBody), expectedMinSize)
	}
	t.Logf("received body size: %d bytes (~%.1f MB)", len(receivedBody), float64(len(receivedBody))/(1024*1024))
}

func TestPerformLMTPTransaction_DotStuffing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	bodyCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go mockLMTPServer(t, ln, 250, bodyCh, errCh)

	conn, reader := lmtpHandshake(t, ln.Addr().String())

	// Message with lines starting with dots
	msg := []byte("From: a@b.com\r\nTo: c@d.com\r\n\r\n.This line starts with a dot\r\n..Two dots\r\nNormal line\r\n")

	result := (&Deliverer{config: &defaultTestConfig}).performLMTPTransaction(
		t.Context(), testLogger(), "trace-dot", conn, reader,
		"a@b.com", "c@d.com", msg, "localhost", "",
	)

	if result.Status != "delivered" {
		t.Fatalf("expected delivered, got %s (error: %s)", result.Status, result.Error)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("server error: %v", err)
	}

	body := string(<-bodyCh)
	if !strings.Contains(body, ".This line starts with a dot") {
		t.Errorf("dot-stuffing broken: single dot line not preserved")
	}
	if !strings.Contains(body, "..Two dots") {
		t.Errorf("dot-stuffing broken: double dot line not preserved")
	}
	if !strings.Contains(body, "Normal line") {
		t.Errorf("normal line missing")
	}
}

// TestPerformLMTPTransaction_NullSender verifies that both "" and "<>" produce
// the wire form "MAIL FROM:<>" (and that angle-bracketed recipients are
// normalized to a single pair of brackets).
func TestPerformLMTPTransaction_NullSender(t *testing.T) {
	cases := []struct {
		name string
		from string
		to   string
		want string // expected MAIL FROM line
		rcpt string // expected RCPT TO line
	}{
		{name: "empty from", from: "", to: "rcpt@example.com", want: "MAIL FROM:<>", rcpt: "RCPT TO:<rcpt@example.com>"},
		{name: "<> from", from: "<>", to: "rcpt@example.com", want: "MAIL FROM:<>", rcpt: "RCPT TO:<rcpt@example.com>"},
		{name: "bracketed from", from: "<bounce@example.com>", to: "rcpt@example.com", want: "MAIL FROM:<bounce@example.com>", rcpt: "RCPT TO:<rcpt@example.com>"},
		{name: "bracketed to", from: "a@b.com", to: "<c@d.com>", want: "MAIL FROM:<a@b.com>", rcpt: "RCPT TO:<c@d.com>"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()

			mailFromCh := make(chan string, 1)
			rcptToCh := make(chan string, 1)
			errCh := make(chan error, 1)
			go mockLMTPServerCapture(t, ln, 250, mailFromCh, rcptToCh, errCh)

			conn, reader := lmtpHandshake(t, ln.Addr().String())

			msg := []byte("From: a@b.com\r\nTo: c@d.com\r\n\r\nbody\r\n")
			result := (&Deliverer{config: &defaultTestConfig}).performLMTPTransaction(
				t.Context(), testLogger(), "trace-null", conn, reader,
				tc.from, tc.to, msg, "localhost", "",
			)

			if result.Status != "delivered" {
				t.Fatalf("expected delivered, got %s (error: %s)", result.Status, result.Error)
			}
			if got := <-mailFromCh; got != tc.want {
				t.Errorf("MAIL FROM wire form: got %q, want %q", got, tc.want)
			}
			if got := <-rcptToCh; got != tc.rcpt {
				t.Errorf("RCPT TO wire form: got %q, want %q", got, tc.rcpt)
			}
			if err := <-errCh; err != nil {
				t.Fatalf("server error: %v", err)
			}
		})
	}
}
