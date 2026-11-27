package netx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/output"
)

type staticResolver struct {
	ips []string
	err error
}

func (r staticResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	if r.err != nil {
		return nil, r.err
	}
	out := make([]net.IPAddr, 0, len(r.ips))
	for _, s := range r.ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func makeHostTLS(t *testing.T, dnsName string) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{dnsName},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
			MinVersion:   tls.VersionTLS12,
		}, &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}
}

func TestClassifyStagesAndRetryability(t *testing.T) {
	t.Run("DNS timeout is retryable", func(t *testing.T) {
		err := ClassifyDNSError(&net.DNSError{Err: "i/o timeout", Name: "imap.example", IsTimeout: true}, "imap.example:993")
		detail, code := errmap.Details(err)
		if code != output.ExitNetwork || !detail.Retryable || !IsRetryable(err) {
			t.Fatalf("unexpected DNS timeout classification: code=%d detail=%+v", code, detail)
		}
		if detail.Context["stage"] != StageDNS || detail.Context["error_category"] != CategoryDNSTimeout {
			t.Fatalf("unexpected context: %+v", detail.Context)
		}
	})

	t.Run("DNS not found is non-retryable", func(t *testing.T) {
		err := ClassifyDNSError(&net.DNSError{Err: "no such host", Name: "missing.invalid", IsNotFound: true}, "missing.invalid:993")
		detail, code := errmap.Details(err)
		if code != output.ExitNetwork || detail.Retryable || IsRetryable(err) {
			t.Fatalf("unexpected NXDOMAIN classification: code=%d detail=%+v", code, detail)
		}
		if detail.Context["error_category"] != CategoryDNSNotFound {
			t.Fatalf("unexpected context: %+v", detail.Context)
		}
	})

	t.Run("TLS certificate error is exit 21 non-retryable", func(t *testing.T) {
		err := ClassifyTLSError(x509.UnknownAuthorityError{}, "imap.example:993", "127.0.0.1:993")
		detail, code := errmap.Details(err)
		if code != output.ExitTLS || detail.Retryable || IsRetryable(err) {
			t.Fatalf("unexpected TLS cert classification: code=%d detail=%+v", code, detail)
		}
		if detail.Context["stage"] != StageTLS || detail.Context["error_category"] != CategoryTLSCertificate {
			t.Fatalf("unexpected context: %+v", detail.Context)
		}
	})

	t.Run("TLS protocol alert is exit 21 non-retryable", func(t *testing.T) {
		err := ClassifyTLSError(tls.AlertError(40), "imap.example:993", "127.0.0.1:993")
		detail, code := errmap.Details(err)
		if code != output.ExitTLS || detail.Retryable || IsRetryable(err) {
			t.Fatalf("unexpected TLS alert classification: code=%d detail=%+v", code, detail)
		}
		if detail.Context["error_category"] != CategoryTLSProtocol {
			t.Fatalf("unexpected context: %+v", detail.Context)
		}
	})

	t.Run("TLS handshake EOF and timeout are transient network errors", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			cat  string
		}{
			{"eof", io.EOF, CategoryTLSNetwork},
			{"timeout", context.DeadlineExceeded, CategoryTLSTimeout},
		} {
			got := ClassifyTLSError(tc.err, "imap.example:993", "127.0.0.1:993")
			detail, code := errmap.Details(got)
			if code != output.ExitNetwork || !detail.Retryable || !IsRetryable(got) {
				t.Fatalf("%s: unexpected classification: code=%d detail=%+v", tc.name, code, detail)
			}
			if detail.Context["stage"] != StageTLS || detail.Context["error_category"] != tc.cat {
				t.Fatalf("%s: unexpected context: %+v", tc.name, detail.Context)
			}
		}
	})

	t.Run("non-retryable kinds are never retried", func(t *testing.T) {
		for _, err := range []error{
			&errmap.Error{Kind: errmap.AuthFailed, Message: "bad auth"},
			&errmap.Error{Kind: errmap.RateLimited, Message: "too many"},
			&errmap.Error{Kind: errmap.DeliveryUncertain, Message: "uncertain"},
			&errmap.Error{Kind: errmap.TLS, Message: "cert"},
			&errmap.Error{Kind: errmap.ServiceNotEnabled, Message: "disabled"},
			context.Canceled,
		} {
			if IsRetryable(err) {
				t.Fatalf("expected non-retryable for %v", err)
			}
		}
	})
}

func TestDialTLSMultiIPFailoverOnTransientError(t *testing.T) {
	serverTLS, clientTLS := makeHostTLS(t, "mail.example.test")

	// Listener 1 drops connections immediately before TLS handshake (transient IO).
	badL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = badL.Close() })
	go func() {
		for {
			c, err := badL.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	// Listener 2 completes TLS with a valid cert for mail.example.test.
	goodL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = goodL.Close() })
	go func() {
		for {
			c, err := goodL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				sc := tls.Server(c, serverTLS)
				_ = sc.Handshake()
			}(c)
		}
	}()

	// Map two synthetic resolved IPs to the bad and good local listeners.
	const ip1, ip2 = "198.51.100.1", "198.51.100.2"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := DialTLS(ctx, DialConfig{
		Host:      "mail.example.test",
		Port:      993,
		TLSConfig: clientTLS,
		Resolver:  staticResolver{ips: []string{ip1, ip2}},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			switch host {
			case ip1:
				return (&net.Dialer{}).DialContext(ctx, network, badL.Addr().String())
			case ip2:
				return (&net.Dialer{}).DialContext(ctx, network, goodL.Addr().String())
			default:
				return nil, errors.New("unexpected addr " + addr)
			}
		},
	})
	if err != nil {
		t.Fatalf("expected multi-IP failover to succeed, got: %v", err)
	}
	_ = res.Conn.Close()
	if res.Endpoint != ip2+":993" {
		t.Fatalf("endpoint = %q, want %q", res.Endpoint, ip2+":993")
	}
	if len(res.TriedEndpoints) != 2 {
		t.Fatalf("tried endpoints = %v, want 2 entries", res.TriedEndpoints)
	}
}

func TestDialTLSNeverFailsOverOnCertError(t *testing.T) {
	badServerTLS, _ := makeHostTLS(t, "wrong.example.test")
	goodServerTLS, clientTLS := makeHostTLS(t, "mail.example.test")

	badL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = badL.Close() })
	go func() {
		for {
			c, err := badL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = tls.Server(c, badServerTLS).Handshake()
			}(c)
		}
	}()

	goodL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = goodL.Close() })
	var goodHits atomic.Int32
	go func() {
		for {
			c, err := goodL.Accept()
			if err != nil {
				return
			}
			goodHits.Add(1)
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = tls.Server(c, goodServerTLS).Handshake()
			}(c)
		}
	}()

	const ip1, ip2 = "198.51.100.1", "198.51.100.2"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = DialTLS(ctx, DialConfig{
		Host:      "mail.example.test",
		Port:      993,
		TLSConfig: clientTLS,
		Resolver:  staticResolver{ips: []string{ip1, ip2}},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			if host == ip1 {
				return (&net.Dialer{}).DialContext(ctx, network, badL.Addr().String())
			}
			return (&net.Dialer{}).DialContext(ctx, network, goodL.Addr().String())
		},
	})
	if err == nil {
		t.Fatal("expected certificate error")
	}
	detail, code := errmap.Details(err)
	if code != output.ExitTLS || detail.Retryable {
		t.Fatalf("expected non-retryable exit 21 TLS error, got code=%d detail=%+v", code, detail)
	}
	if goodHits.Load() != 0 {
		t.Fatalf("second IP was dialed %d times after a certificate verification failure", goodHits.Load())
	}
}

func TestRetryBoundedBackoffAndDeadline(t *testing.T) {
	t.Run("recovers on second attempt", func(t *testing.T) {
		var sleeps []time.Duration
		val, attempts, err := Retry(context.Background(), RetryConfig{
			MaxAttempts: 3,
			BaseBackoff: 100 * time.Millisecond,
			NoJitter:    true,
			Sleep: func(_ context.Context, d time.Duration) error {
				sleeps = append(sleeps, d)
				return nil
			},
		}, func(_ context.Context, attempt int) (string, error) {
			if attempt == 1 {
				return "", ClassifyTLSError(io.EOF, "imap.example:993", "127.0.0.1:993")
			}
			return "ok", nil
		})
		if err != nil || val != "ok" || attempts != 2 {
			t.Fatalf("got val=%q attempts=%d err=%v; want ok, 2, nil", val, attempts, err)
		}
		if len(sleeps) != 1 || sleeps[0] != 100*time.Millisecond {
			t.Fatalf("unexpected backoff sleeps: %v", sleeps)
		}
	})

	t.Run("exhausts at 3 attempts with exponential backoff", func(t *testing.T) {
		var sleeps []time.Duration
		_, attempts, err := Retry(context.Background(), RetryConfig{
			MaxAttempts: 3,
			BaseBackoff: 100 * time.Millisecond,
			MaxBackoff:  500 * time.Millisecond,
			NoJitter:    true,
			Sleep: func(_ context.Context, d time.Duration) error {
				sleeps = append(sleeps, d)
				return nil
			},
		}, func(_ context.Context, _ int) (string, error) {
			return "", ClassifyTCPError(errors.New("connection refused"), "imap.example:993", "127.0.0.1:993")
		})
		if err == nil || attempts != 3 {
			t.Fatalf("got attempts=%d err=%v; want 3 and error", attempts, err)
		}
		if len(sleeps) != 2 || sleeps[0] != 100*time.Millisecond || sleeps[1] != 200*time.Millisecond {
			t.Fatalf("unexpected backoff sleeps: %v", sleeps)
		}
		detail, code := errmap.Details(err)
		if code != output.ExitNetwork || detail.Context["attempts"] != 3 {
			t.Fatalf("unexpected error detail: code=%d detail=%+v", code, detail)
		}
	})

	t.Run("never retries non-retryable error", func(t *testing.T) {
		var sleeps int
		_, attempts, err := Retry(context.Background(), RetryConfig{
			MaxAttempts: 3,
			BaseBackoff: 10 * time.Millisecond,
			Sleep: func(context.Context, time.Duration) error {
				sleeps++
				return nil
			},
		}, func(_ context.Context, _ int) (string, error) {
			return "", &errmap.Error{Kind: errmap.RateLimited, Message: "rate limited"}
		})
		if err == nil || attempts != 1 || sleeps != 0 {
			t.Fatalf("got attempts=%d sleeps=%d err=%v; want 1, 0, error", attempts, sleeps, err)
		}
	})

	t.Run("respects tight context deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		var sleeps int
		_, attempts, err := Retry(ctx, RetryConfig{
			MaxAttempts: 3,
			BaseBackoff: time.Second,
			NoJitter:    true,
			Sleep: func(context.Context, time.Duration) error {
				sleeps++
				return nil
			},
		}, func(_ context.Context, _ int) (string, error) {
			return "", ClassifyTCPError(errors.New("connection refused"), "imap.example:993", "127.0.0.1:993")
		})
		if err == nil || attempts != 1 || sleeps != 0 {
			t.Fatalf("expected deadline guard to stop retry: attempts=%d sleeps=%d err=%v", attempts, sleeps, err)
		}
	})
}

func TestResolveHostIPLiteralSkipsDNS(t *testing.T) {
	ips, dur, err := ResolveHost(context.Background(), staticResolver{err: errors.New("dns must not be called")}, "127.0.0.1", 993, time.Second)
	if err != nil || len(ips) != 1 || ips[0] != "127.0.0.1" || dur != 0 {
		t.Fatalf("got ips=%v dur=%v err=%v", ips, dur, err)
	}
	// Ensure numeric port formatting works for IPv6 literals too.
	if got := net.JoinHostPort("::1", strconv.Itoa(993)); got != "[::1]:993" {
		t.Fatalf("unexpected ipv6 hostport: %s", got)
	}
}
