package delivery

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"strela/internal/config"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNSServer is a minimal DNS server on 127.0.0.1 that answers TXT queries
// over UDP and TCP on the same port. Names it does not know get NXDOMAIN.
type fakeDNSServer struct {
	addr        string
	txt         map[string][]string // FQDN (lowercase, trailing dot) -> strings of one TXT record
	truncateUDP bool                // answer UDP queries with the TC bit set and no records
	udpQueries  atomic.Int32
	tcpQueries  atomic.Int32
}

func startFakeDNSServer(t *testing.T, txt map[string][]string, truncateUDP bool) *fakeDNSServer {
	t.Helper()

	// UDP and TCP must share a port, so pick a free UDP port and retry if the
	// same TCP port happens to be taken.
	var pc net.PacketConn
	var ln net.Listener
	for attempt := 0; attempt < 10; attempt++ {
		var err error
		pc, err = net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Failed to listen on UDP: %v", err)
		}
		ln, err = net.Listen("tcp", pc.LocalAddr().String())
		if err == nil {
			break
		}
		pc.Close()
		pc, ln = nil, nil
	}
	if ln == nil {
		t.Fatal("Failed to find a port free on both UDP and TCP")
	}

	s := &fakeDNSServer{
		addr:        pc.LocalAddr().String(),
		txt:         txt,
		truncateUDP: truncateUDP,
	}
	t.Cleanup(func() {
		pc.Close()
		ln.Close()
	})

	go s.serveUDP(pc)
	go s.serveTCP(ln)
	return s
}

func (s *fakeDNSServer) serveUDP(pc net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		s.udpQueries.Add(1)
		if resp := s.respond(buf[:n], true); resp != nil {
			pc.WriteTo(resp, addr)
		}
	}
}

func (s *fakeDNSServer) serveTCP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s.handleTCP(conn)
	}
}

// handleTCP serves length-prefixed DNS messages (RFC 1035 §4.2.2) on one connection.
func (s *fakeDNSServer) handleTCP(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	for {
		var length uint16
		if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
			return
		}
		query := make([]byte, length)
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		s.tcpQueries.Add(1)
		resp := s.respond(query, false)
		if resp == nil {
			return
		}
		if err := binary.Write(conn, binary.BigEndian, uint16(len(resp))); err != nil {
			return
		}
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

func (s *fakeDNSServer) respond(query []byte, overUDP bool) []byte {
	var parser dnsmessage.Parser
	hdr, err := parser.Start(query)
	if err != nil {
		return nil
	}
	q, err := parser.Question()
	if err != nil {
		return nil
	}

	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 hdr.ID,
			Response:           true,
			RecursionDesired:   hdr.RecursionDesired,
			RecursionAvailable: true,
		},
		Questions: []dnsmessage.Question{q},
	}

	record, found := s.txt[strings.ToLower(q.Name.String())]
	switch {
	case !found:
		resp.Header.RCode = dnsmessage.RCodeNameError
	case q.Type != dnsmessage.TypeTXT:
		// Name exists but has no records of this type (NODATA)
	case overUDP && s.truncateUDP:
		resp.Header.Truncated = true
	default:
		resp.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name:  q.Name,
				Type:  dnsmessage.TypeTXT,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			},
			Body: &dnsmessage.TXTResource{TXT: record},
		}}
	}

	packed, err := resp.Pack()
	if err != nil {
		return nil
	}
	return packed
}

func newTXTTestResolver(resolvers ...string) *DNSResolver {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewDNSResolver(&config.DNSConfig{
		Resolvers:      resolvers,
		TimeoutSeconds: 5,
	}, logger)
}

// The name only exists on the fake server (.invalid never resolves publicly), so
// a successful lookup proves the configured resolver was used, not the system one.
func TestDNSResolver_LookupTXT_UsesCustomResolver(t *testing.T) {
	const name = "key1._domainkey.strela-test.invalid"
	srv := startFakeDNSServer(t, map[string][]string{
		name + ".": {"v=DKIM1; k=rsa; ", "p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A"},
	}, false)

	resolver := newTXTTestResolver(srv.addr)

	records, err := resolver.LookupTXT(context.Background(), name)
	if err != nil {
		t.Fatalf("TXT lookup with custom resolver failed: %v", err)
	}

	// Strings of one TXT record are joined without separator
	expected := []string{"v=DKIM1; k=rsa; p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A"}
	if !reflect.DeepEqual(records, expected) {
		t.Errorf("Expected %q, got %q", expected, records)
	}

	if srv.udpQueries.Load() == 0 {
		t.Error("Expected the custom resolver to be queried over UDP")
	}
}

func TestDNSResolver_LookupTXT_NotFound(t *testing.T) {
	srv := startFakeDNSServer(t, map[string][]string{}, false)

	resolver := newTXTTestResolver(srv.addr)

	_, err := resolver.LookupTXT(context.Background(), "missing._domainkey.strela-test.invalid")
	if err == nil {
		t.Fatal("Expected error for missing TXT record, got nil")
	}
	if !isDNSNotFound(err) {
		t.Errorf("Expected a DNS not-found error, got: %v", err)
	}
	// The error must name the configured resolver, not one from resolv.conf
	if !strings.Contains(err.Error(), "on "+srv.addr) {
		t.Errorf("Expected error to name resolver %s, got: %v", srv.addr, err)
	}
}

// A truncated UDP response must be retried over TCP to get the full record
// (large DKIM keys may not fit in a UDP response).
func TestDNSResolver_LookupTXT_TruncatedUDPFallsBackToTCP(t *testing.T) {
	const name = "big._domainkey.strela-test.invalid"
	chunk := strings.Repeat("A", 250)
	srv := startFakeDNSServer(t, map[string][]string{
		name + ".": {"v=DKIM1; k=rsa; p=", chunk, chunk, chunk},
	}, true)

	resolver := newTXTTestResolver(srv.addr)

	records, err := resolver.LookupTXT(context.Background(), name)
	if err != nil {
		t.Fatalf("TXT lookup failed: %v", err)
	}

	expected := []string{"v=DKIM1; k=rsa; p=" + strings.Repeat("A", 750)}
	if !reflect.DeepEqual(records, expected) {
		t.Errorf("Expected full record of %d bytes, got %q", len(expected[0]), records)
	}

	if srv.udpQueries.Load() == 0 {
		t.Error("Expected the first query to go over UDP")
	}
	if srv.tcpQueries.Load() == 0 {
		t.Error("Expected the truncated response to be retried over TCP")
	}
}

// A resolver that cannot be reached must fail over to the next configured one.
func TestDNSResolver_LookupTXT_FailsOverToNextResolver(t *testing.T) {
	const name = "key1._domainkey.strela-test.invalid"
	srv := startFakeDNSServer(t, map[string][]string{
		name + ".": {"v=DKIM1; k=rsa; p=abc"},
	}, true) // TCP only answers, so every lookup needs a TCP dial

	// Reserve a TCP port and close it again so connecting to it is refused
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to reserve port: %v", err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	resolver := newTXTTestResolver(deadAddr, srv.addr)

	// Run twice so the round-robin starts on the dead resolver at least once
	for i := 0; i < 2; i++ {
		records, err := resolver.LookupTXT(context.Background(), name)
		if err != nil {
			t.Fatalf("TXT lookup %d failed: %v", i, err)
		}
		if len(records) != 1 || records[0] != "v=DKIM1; k=rsa; p=abc" {
			t.Errorf("Lookup %d: unexpected records %q", i, records)
		}
	}
}
