package sendmail

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/netx"
	"github.com/situker/qqmail-cli/internal/output"
)

type syntheticSMTPConfig struct {
	serverTLS     *tls.Config
	implicitTLS   bool
	dropAttempts  int32 // drop first N connections before TLS / greeting
	authResponse  string
	dataDropAfter bool // drop socket inside DATA after accepting body
	noStartTLS    bool
	greetResponse string
}

type syntheticSMTPServer struct {
	addr     string
	port     int
	accepts  atomic.Int32
	received atomic.Int32
	authHits atomic.Int32
}

func startSyntheticSMTP(t *testing.T, cfg syntheticSMTPConfig) *syntheticSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	srv := &syntheticSMTPServer{addr: listener.Addr().String(), port: port}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			n := srv.accepts.Add(1)
			wg.Add(1)
			go func(c net.Conn, attempt int32) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				if attempt <= cfg.dropAttempts {
					return
				}
				rw := c
				if cfg.implicitTLS {
					sc := tls.Server(c, cfg.serverTLS)
					if err := sc.Handshake(); err != nil {
						return
					}
					rw = sc
				}
				reader := bufio.NewReader(rw)
				writer := bufio.NewWriter(rw)
				writeLine := func(s string) bool {
					if _, err := writer.WriteString(s + "\r\n"); err != nil {
						return false
					}
					return writer.Flush() == nil
				}
				greet := cfg.greetResponse
				if greet == "" {
					greet = "220 fixture ESMTP"
				}
				if !writeLine(greet) {
					return
				}
				if strings.HasPrefix(greet, "4") || strings.HasPrefix(greet, "5") {
					return
				}
				upgraded := cfg.implicitTLS
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					upper := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(upper, "EHLO"):
						if !upgraded && !cfg.noStartTLS {
							if !writeLine("250-fixture\r\n250 STARTTLS") {
								return
							}
						} else if !upgraded && cfg.noStartTLS {
							if !writeLine("250-fixture\r\n250 AUTH PLAIN") {
								return
							}
						} else {
							if !writeLine("250-fixture\r\n250-AUTH PLAIN LOGIN\r\n250 8BITMIME") {
								return
							}
						}
					case upper == "STARTTLS":
						if cfg.noStartTLS {
							_ = writeLine("502 not supported")
							return
						}
						if !writeLine("220 go ahead") {
							return
						}
						sc := tls.Server(rw, cfg.serverTLS)
						if err := sc.Handshake(); err != nil {
							return
						}
						rw = sc
						reader = bufio.NewReader(rw)
						writer = bufio.NewWriter(rw)
						upgraded = true
					case strings.HasPrefix(upper, "AUTH "):
						srv.authHits.Add(1)
						resp := cfg.authResponse
						if resp == "" {
							resp = "235 ok"
						}
						if !writeLine(resp) {
							return
						}
					case strings.HasPrefix(upper, "MAIL FROM:"):
						if !writeLine("250 ok") {
							return
						}
					case strings.HasPrefix(upper, "RCPT TO:"):
						if !writeLine("250 ok") {
							return
						}
					case upper == "DATA":
						if !writeLine("354 end with .") {
							return
						}
						for {
							bodyLine, err := reader.ReadString('\n')
							if err != nil {
								return
							}
							if bodyLine == ".\r\n" || bodyLine == ".\n" {
								break
							}
						}
						srv.received.Add(1)
						if cfg.dataDropAfter {
							// Drop immediately without sending 250 OK -> uncertain delivery.
							return
						}
						if !writeLine("250 queued") {
							return
						}
					case upper == "NOOP":
						if !writeLine("250 ok") {
							return
						}
					case upper == "QUIT":
						_ = writeLine("221 bye")
						return
					default:
						if !writeLine("500 unknown") {
							return
						}
					}
				}
			}(conn, n)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return srv
}

func sampleDraft() (Draft, []byte) {
	draft := Draft{
		From:    mail.Address{Address: "user@qq.com"},
		To:      []mail.Address{{Address: "allowed@example.com"}},
		Subject: "fixture",
		Body:    "hello",
	}
	raw, _ := Build(draft)
	return draft, raw
}

func TestSendRecoversAfterTransientHandshakeDropOnce(t *testing.T) {
	serverTLS, clientTLS := fixtureTLS(t)
	srv := startSyntheticSMTP(t, syntheticSMTPConfig{
		serverTLS:    serverTLS,
		implicitTLS:  true,
		dropAttempts: 1,
	})
	draft, raw := sampleDraft()
	named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: srv.port}

	recorder := &netx.ConnectionRecorder{}
	ctx, cancel := context.WithTimeout(netx.WithConnectionRecorder(context.Background(), recorder), 5*time.Second)
	defer cancel()

	implicitTrue := true
	err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
		tlsOverride: clientTLS,
		implicitTLS: &implicitTrue,
		cmdTimeout:  2 * time.Second,
		retry:       netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
	})
	if err != nil {
		t.Fatalf("expected recovery on attempt 2, got: %v", err)
	}
	if srv.accepts.Load() != 2 || srv.received.Load() != 1 {
		t.Fatalf("accepts=%d received=%d; want 2 accepts and 1 delivery", srv.accepts.Load(), srv.received.Load())
	}
	info := recorder.Latest()
	if info == nil || info.Protocol != "smtp" || info.Attempts != 2 || info.Stage != "smtp_sent" || info.Endpoint != srv.addr {
		t.Fatalf("unexpected ConnectionInfo: %+v", info)
	}
}

func TestSendNeverRetriesOrFallsBackOnCertAuthRateLimitOrUncertainDelivery(t *testing.T) {
	serverTLS, clientTLS := fixtureTLS(t)
	draft, raw := sampleDraft()

	t.Run("certificate error never retries or falls back", func(t *testing.T) {
		primary := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true})
		fallback := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: false})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: primary.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Omit clientTLS so self-signed cert fails x509 verification.
		err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
			fallbackPort: fallback.port,
			cmdTimeout:   2 * time.Second,
			retry:        netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
		})
		detail, code := errmap.Details(err)
		if code != output.ExitTLS || detail.Retryable {
			t.Fatalf("expected non-retryable exit 21, got code=%d detail=%+v", code, detail)
		}
		if primary.accepts.Load() != 1 || fallback.accepts.Load() != 0 {
			t.Fatalf("primary=%d fallback=%d; want 1, 0", primary.accepts.Load(), fallback.accepts.Load())
		}
	})

	t.Run("auth failure never retries or falls back", func(t *testing.T) {
		primary := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true, authResponse: "535 Login Fail"})
		fallback := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: false})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: primary.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
			tlsOverride:  clientTLS,
			fallbackPort: fallback.port,
			cmdTimeout:   2 * time.Second,
			retry:        netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
		})
		detail, code := errmap.Details(err)
		if code != output.ExitAuth || detail.Retryable {
			t.Fatalf("expected non-retryable exit 10, got code=%d detail=%+v", code, detail)
		}
		if primary.accepts.Load() != 1 || fallback.accepts.Load() != 0 || primary.received.Load() != 0 {
			t.Fatalf("primary=%d fallback=%d received=%d", primary.accepts.Load(), fallback.accepts.Load(), primary.received.Load())
		}
	})

	t.Run("rate limit never retries or falls back", func(t *testing.T) {
		primary := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true, greetResponse: "421 too many connections"})
		fallback := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: false})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: primary.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
			tlsOverride:  clientTLS,
			fallbackPort: fallback.port,
			cmdTimeout:   2 * time.Second,
			retry:        netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
		})
		detail, code := errmap.Details(err)
		if code != output.ExitRateLimited {
			t.Fatalf("expected exit 30 rate_limited, got code=%d detail=%+v", code, detail)
		}
		if primary.accepts.Load() != 1 || fallback.accepts.Load() != 0 {
			t.Fatalf("primary=%d fallback=%d; want 1, 0", primary.accepts.Load(), fallback.accepts.Load())
		}
	})

	t.Run("uncertain delivery during DATA never retries or falls back", func(t *testing.T) {
		primary := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true, dataDropAfter: true})
		fallback := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: false})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: primary.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
			tlsOverride:  clientTLS,
			fallbackPort: fallback.port,
			cmdTimeout:   2 * time.Second,
			retry:        netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
		})
		detail, code := errmap.Details(err)
		if code != output.ExitNetwork || detail.Code != string(errmap.DeliveryUncertain) || detail.Retryable {
			t.Fatalf("expected non-retryable delivery_uncertain (exit 20), got code=%d detail=%+v", code, detail)
		}
		if primary.accepts.Load() != 1 || primary.received.Load() != 1 || fallback.accepts.Load() != 0 || fallback.received.Load() != 0 {
			t.Fatalf("primary accepts=%d received=%d, fallback accepts=%d received=%d",
				primary.accepts.Load(), primary.received.Load(), fallback.accepts.Load(), fallback.received.Load())
		}
	})
}

func TestSendExhaustsBoundedRetries(t *testing.T) {
	serverTLS, clientTLS := fixtureTLS(t)
	srv := startSyntheticSMTP(t, syntheticSMTPConfig{
		serverTLS:    serverTLS,
		implicitTLS:  true,
		dropAttempts: 10,
	})
	draft, raw := sampleDraft()
	named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: srv.port}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	implicitTrue := true
	err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
		tlsOverride: clientTLS,
		implicitTLS: &implicitTrue,
		cmdTimeout:  2 * time.Second,
		retry:       netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
	})
	detail, code := errmap.Details(err)
	if code != output.ExitNetwork || !detail.Retryable {
		t.Fatalf("expected retryable exit 20, got code=%d detail=%+v", code, detail)
	}
	if srv.accepts.Load() != 3 || detail.Context["attempts"] != 3 || detail.Context["stage"] != StageTLS || detail.Context["endpoint"] != srv.addr {
		t.Fatalf("unexpected exhaustion metadata: accepts=%d context=%+v", srv.accepts.Load(), detail.Context)
	}
}
