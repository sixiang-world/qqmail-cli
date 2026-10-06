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
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
)

// searchWireFixture is a recording fake IMAP server in the hand-rolled bufio
// per-line service style of dialect_test.go: it answers the dial handshake and
// captures every client command line, so tests can assert on the raw SEARCH
// wire format the library actually emits.
type searchWireFixture struct {
	Client *Client

	mu    sync.Mutex
	lines []string
}

func newFixture(t *testing.T) *searchWireFixture {
	t.Helper()
	serverTLS, clientTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	f := &searchWireFixture{}
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		tlsConn := tls.Server(conn, serverTLS)
		if tlsConn.Handshake() != nil {
			return
		}
		_, _ = fmt.Fprint(tlsConn, "* OK [CAPABILITY IMAP4rev1] fixture ready\r\n")
		reader := bufio.NewReader(tlsConn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			f.record(strings.TrimSpace(line))
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			tag := fields[0]
			switch strings.ToUpper(fields[1]) {
			case "CAPABILITY":
				_, _ = fmt.Fprintf(tlsConn, "* CAPABILITY IMAP4rev1\r\n%s OK capability\r\n", tag)
			case "LOGIN":
				_, _ = fmt.Fprintf(tlsConn, "%s OK login\r\n", tag)
			case "SELECT":
				_, _ = fmt.Fprintf(tlsConn, "* FLAGS (\\Seen)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY 11] stable\r\n* OK [UIDNEXT 8] next\r\n%s OK [READ-ONLY] selected\r\n", tag)
			case "UID":
				_, _ = fmt.Fprintf(tlsConn, "* SEARCH 1 2\r\n%s OK search\r\n", tag)
			case "LOGOUT":
				_, _ = fmt.Fprintf(tlsConn, "* BYE done\r\n%s OK logout\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(tlsConn, "%s BAD unsupported\r\n", tag)
			}
		}
	}()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	client, err := dial(ctx, account.Named{Name: "criteria", Email: "user@qq.com", IMAPHost: host, IMAPPort: port}, "abcdefghijklmnop", clientTLS, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Logout(context.Background()) })
	f.Client = client
	return f
}

func (f *searchWireFixture) record(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, line)
}

// LastSearchLine returns the most recent client command line carrying a SEARCH
// command (the wire includes the UID prefix, e.g. "A003 UID SEARCH ...").
func (f *searchWireFixture) LastSearchLine() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.lines) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToUpper(f.lines[i]), "SEARCH") {
			return f.lines[i]
		}
	}
	return ""
}

func TestSearchSendsBeforeAndToCriteria(t *testing.T) {
	f := newFixture(t)
	_, err := f.Client.Search(context.Background(), SearchFilter{
		Since:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Before: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		To:     "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// go-imap's wire date format is 2-Jan-2006, without leading zeros; beta.8
	// emits dates and header values as quoted IMAP strings.
	if got := f.LastSearchLine(); !strings.Contains(got, `BEFORE "1-Feb-2026"`) ||
		!strings.Contains(got, `TO "alice@example.com"`) ||
		!strings.Contains(got, `SINCE "1-Jan-2026"`) {
		t.Fatalf("search line missing criteria: %q", got)
	}
}
