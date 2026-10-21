package delivery

import (
	"bufio"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"strings"
	"testing"
	"time"

	"strela/internal/config"
)

// negativeSerialCertPEM is a self-signed P-256 certificate with serial -12345,
// generated with:
//
//	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
//	  -subj /CN=negative-serial.test -set_serial -12345 -days 36500
//
// x509.CreateCertificate refuses negative serials, so it cannot be built in Go.
const negativeSerialCertPEM = `-----BEGIN CERTIFICATE-----
MIIBgzCCASmgAwIBAgICz8cwCgYIKoZIzj0EAwIwHzEdMBsGA1UEAwwUbmVnYXRp
dmUtc2VyaWFsLnRlc3QwIBcNMjYxMDIxMjE1MzMxWhgPMjEyNjA5MjcyMTUzMzFa
MB8xHTAbBgNVBAMMFG5lZ2F0aXZlLXNlcmlhbC50ZXN0MFkwEwYHKoZIzj0CAQYI
KoZIzj0DAQcDQgAEcX6d/odSgk2WzM+TqrGxNDaBr3gKoVczPvRd4fwiBrcKVJ/k
TtvES/EWpkjEHo2Vb7P8MnFnQdFueeaM6uINT6NTMFEwHQYDVR0OBBYEFLlhOKub
CpLxzBz9gnZCen2Z2LJpMB8GA1UdIwQYMBaAFLlhOKubCpLxzBz9gnZCen2Z2LJp
MA8GA1UdEwEB/wQFMAMBAf8wCgYIKoZIzj0EAwIDSAAwRQIgRTvatibmvfOGac7n
nF2ldc1PxejERdOMkuGTJxYO6iUCIQCOfIMjzH3S+Oh16xQodh9rg2ipv/meBd9N
U7/KSIRz+w==
-----END CERTIFICATE-----`

const negativeSerialKeyPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgvBNCzl0sl6JFkxsG
5PKD7USxYXS3jS3sMa2iicebjUGhRANCAARxfp3+h1KCTZbMz5OqsbE0NoGveAqh
VzM+9F3h/CIGtwpUn+RO28RL8RamSMQejZVvs/wycWdB0W555ozq4g1P
-----END PRIVATE KEY-----`

// negativeSerialCertificate builds the server certificate without parsing the
// leaf (tls.X509KeyPair would), so only the client side depends on whether
// negative serials are accepted.
func negativeSerialCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	certBlock, _ := pem.Decode([]byte(negativeSerialCertPEM))
	keyBlock, _ := pem.Decode([]byte(negativeSerialKeyPEM))
	if certBlock == nil || keyBlock == nil {
		t.Fatal("bad PEM fixture")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		t.Fatalf("fixture key is %T, not a crypto.Signer", key)
	}
	return tls.Certificate{Certificate: [][]byte{certBlock.Bytes}, PrivateKey: signer}
}

// startSTARTTLSServer starts a fake MX that advertises STARTTLS, presents cert
// and accepts any message.
func startSTARTTLSServer(t *testing.T, cert tls.Certificate) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSTARTTLS(conn, tlsConfig)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func serveSTARTTLS(conn net.Conn, tlsConfig *tls.Config) {
	defer func() { conn.Close() }()
	reader := bufio.NewReader(conn)
	usedTLS := false
	conn.Write([]byte("220 fake ESMTP\r\n"))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if usedTLS {
				conn.Write([]byte("250-fake\r\n250 8BITMIME\r\n"))
			} else {
				conn.Write([]byte("250-fake\r\n250-STARTTLS\r\n250 8BITMIME\r\n"))
			}
		case cmd == "STARTTLS" && !usedTLS:
			conn.Write([]byte("220 ready to start TLS\r\n"))
			conn = tls.Server(conn, tlsConfig)
			reader = bufio.NewReader(conn)
			usedTLS = true
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
			conn.Write([]byte("250 2.0.0 OK queued\r\n"))
		case cmd == "QUIT":
			conn.Write([]byte("221 bye\r\n"))
			return
		default:
			conn.Write([]byte("250 OK\r\n"))
		}
	}
}

// TestSTARTTLSNegativeSerialCertificate verifies delivery to an MX whose
// certificate has a negative serial number. Go 1.23+ refuses to parse such a
// certificate (even with InsecureSkipVerify), failing the STARTTLS handshake on
// every MX and source IP with "x509: negative serial number". go.mod sets
// godebug x509negativeserial=1 to accept them; this test fails without it.
func TestSTARTTLSNegativeSerialCertificate(t *testing.T) {
	port := startSTARTTLSServer(t, negativeSerialCertificate(t))
	d := newDialTestDeliverer(t, port)
	t.Cleanup(d.Stop)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	r := d.attemptDelivery(ctx, testLogger(), "trace", "a@example.com", "b@example.net",
		[]byte("Subject: hi\r\n\r\nhello\r\n"),
		"127.0.0.1", []string{"127.0.0.1"}, "", false, config.ProtocolSMTP, nil, d.config)

	if r.Status != "delivered" {
		t.Fatalf("status = %q, want \"delivered\" (error: %s)", r.Status, r.Error)
	}
}
