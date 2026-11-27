package imapx

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
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

func serveHealthyIMAP(conn *tls.Conn) {
	_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] fixture ready\r\n")
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[1]) {
		case "CAPABILITY":
			_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 ID\r\n%s OK capability\r\n", fields[0])
		case "LOGIN":
			_, _ = fmt.Fprintf(conn, "%s OK [CAPABILITY IMAP4rev1 ID] login\r\n", fields[0])
		case "ID":
			_, _ = fmt.Fprintf(conn, "* ID NIL\r\n%s OK id\r\n", fields[0])
		case "LOGOUT":
			_, _ = fmt.Fprintf(conn, "* BYE fixture\r\n%s OK logout\r\n", fields[0])
			return
		default:
			_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", fields[0])
		}
	}
}

func TestDialRecoversAfterTransientTLSHandshakeFailure(t *testing.T) {
	serverTLS, clientTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepts atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			n := accepts.Add(1)
			wg.Add(1)
			go func(c net.Conn, attempt int32) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				if attempt == 1 {
					// Simulate transient drop during TLS handshake.
					return
				}
				tlsConn := tls.Server(c, serverTLS)
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				serveHealthyIMAP(tlsConn)
			}(conn, n)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	cfg := account.Named{Name: "retry", Email: "user@qq.com", IMAPHost: host, IMAPPort: port}

	recorder := &netx.ConnectionRecorder{}
	ctx, cancel := context.WithTimeout(netx.WithConnectionRecorder(context.Background(), recorder), 5*time.Second)
	defer cancel()

	client, err := dialWithOptions(ctx, cfg, "abcdefghijklmnop", dialOptions{
		tlsOverride:    clientTLS,
		commandTimeout: 2 * time.Second,
		retry: netx.RetryConfig{
			MaxAttempts: 3,
			BaseBackoff: 5 * time.Millisecond,
			NoJitter:    true,
		},
	})
	if err != nil {
		t.Fatalf("expected recovery on attempt 2, got: %v", err)
	}
	defer func() { _ = client.Logout(context.Background()) }()

	info, ok := ConnectionInfoOf(client)
	if !ok {
		t.Fatal("expected ConnectionInfoOf to return recorded metadata")
	}
	if info.Attempts != 2 || info.Stage != "imap_ready" || info.Endpoint != listener.Addr().String() || info.Transport != "implicit_tls" {
		t.Fatalf("unexpected ConnectionInfo: %+v", info)
	}
	recorded := recorder.Latest()
	if recorded == nil || recorded.Attempts != 2 || recorded.Endpoint != listener.Addr().String() {
		t.Fatalf("context recorder missed connection info: %+v", recorded)
	}
}

func TestDialExhaustsRetriesAndReportsMetadata(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepts atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	cfg := account.Named{Name: "exhaust", Email: "user@qq.com", IMAPHost: host, IMAPPort: port}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = dialWithOptions(ctx, cfg, "abcdefghijklmnop", dialOptions{
		commandTimeout: 2 * time.Second,
		retry: netx.RetryConfig{
			MaxAttempts: 3,
			BaseBackoff: 5 * time.Millisecond,
			NoJitter:    true,
		},
	})
	if err == nil {
		t.Fatal("expected dial failure")
	}
	detail, code := errmap.Details(err)
	if code != output.ExitNetwork || !detail.Retryable {
		t.Fatalf("expected retryable exit 20, got code=%d detail=%+v", code, detail)
	}
	if accepts.Load() != 3 || detail.Context["attempts"] != 3 || detail.Context["stage"] != stageTLS || detail.Context["endpoint"] != listener.Addr().String() {
		t.Fatalf("unexpected exhaustion metadata: accepts=%d context=%+v", accepts.Load(), detail.Context)
	}
}

func TestDialNeverRetriesCertErrorAuthOrRateLimit(t *testing.T) {
	t.Run("untrusted certificate", func(t *testing.T) {
		var accepts atomic.Int32
		cfg, _ := greetingTestServer(t, func(_ *tls.Conn) {
			accepts.Add(1)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := dialWithOptions(ctx, cfg, "abcdefghijklmnop", dialOptions{
			commandTimeout: time.Second,
			retry:          netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond},
		})
		detail, code := errmap.Details(err)
		if code != output.ExitTLS || detail.Retryable || detail.Context["attempts"] != 1 {
			t.Fatalf("expected 1-attempt exit 21 TLS failure, got code=%d detail=%+v", code, detail)
		}
		if accepts.Load() != 0 {
			t.Fatalf("TLS handshake unexpectedly completed %d times", accepts.Load())
		}
	})

	for _, tc := range []struct {
		name     string
		response string
		wantExit int
		wantCode string
	}{
		{"auth failure", "NO [AUTHENTICATIONFAILED] bad code", output.ExitAuth, string(errmap.AuthFailed)},
		{"rate limited", "NO too many logins, try later", output.ExitRateLimited, string(errmap.RateLimited)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var accepts atomic.Int32
			cfg, clientTLS := greetingTestServer(t, func(conn *tls.Conn) {
				accepts.Add(1)
				_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] fixture ready\r\n")
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					if len(fields) < 2 {
						continue
					}
					switch strings.ToUpper(fields[1]) {
					case "CAPABILITY":
						_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK\r\n", fields[0])
					case "LOGIN":
						_, _ = fmt.Fprintf(conn, "%s %s\r\n", fields[0], tc.response)
						return
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := dialWithOptions(ctx, cfg, "abcdefghijklmnop", dialOptions{
				tlsOverride:    clientTLS,
				commandTimeout: time.Second,
				retry:          netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond},
			})
			detail, code := errmap.Details(err)
			if code != tc.wantExit || detail.Code != tc.wantCode || accepts.Load() != 1 || detail.Context["attempts"] != 1 {
				t.Fatalf("got code=%d detail=%+v accepts=%d", code, detail, accepts.Load())
			}
		})
	}
}
