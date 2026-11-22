package imapx

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
)

func greetingTestServer(t *testing.T, serve func(*tls.Conn)) (account.Named, *tls.Config) {
	t.Helper()
	serverTLS, clientTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		tlsConn := tls.Server(conn, serverTLS)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		serve(tlsConn)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("greeting fixture did not stop")
		}
	})
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	return account.Named{Name: "greeting", Email: "user@qq.com", IMAPHost: host, IMAPPort: port}, clientTLS
}

func TestDialWaitsForGreetingBeforeSendingCommand(t *testing.T) {
	premature := make(chan bool, 1)
	cfg, clientTLS := greetingTestServer(t, func(conn *tls.Conn) {
		// Delay the greeting and observe the wire before allowing any command.
		// A client that pipelines CAPABILITY before the greeting fails even
		// though the fixture then completes its ordinary login sequence.
		_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		reader := bufio.NewReader(conn)
		_, err := reader.Peek(1)
		premature <- err == nil
		if err != nil {
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				t.Errorf("waiting for premature command: %v", err)
				return
			}
		}
		_ = conn.SetReadDeadline(time.Time{})
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] fixture ready\r\n")
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
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capability\r\n", fields[0])
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK login\r\n", fields[0])
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected command\r\n", fields[0])
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dial(ctx, cfg, "abcdefghijklmnop", clientTLS, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if <-premature {
		t.Fatal("client sent an IMAP command before the server greeting")
	}
}

func TestDialGreetingRespectsContextDeadline(t *testing.T) {
	cfg, clientTLS := greetingTestServer(t, func(conn *tls.Conn) {
		// Finish TLS but never send the IMAP greeting. Read until the client
		// closes its connection so the fixture also checks deadline cleanup.
		reader := bufio.NewReader(conn)
		for {
			if _, err := reader.ReadString('\n'); err != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	client, err := dial(ctx, cfg, "abcdefghijklmnop", clientTLS, 5*time.Second)
	if client != nil {
		client.close()
	}
	if err == nil {
		t.Fatal("expected greeting deadline error")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("greeting ignored context deadline: %v", elapsed)
	}
	if got := errmap.Classify(err).Kind; got != errmap.Network {
		t.Fatalf("greeting deadline classified as %s, want network", got)
	}
}
