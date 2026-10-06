package imapx

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/mailmodel"
)

// writableFixture is a recording fake IMAP server in the hand-rolled bufio
// per-line service style of dialect_test.go: it answers the dial handshake and
// a writable SELECT, and captures every client command line, so tests can
// assert on the raw STORE wire format the library actually emits. The repo had
// no SetSeen test precedent, so this fixture was built new for the flag
// primitives rather than adapted from a read-path fixture.
type writableFixture struct {
	Mutator Mutator

	mu      sync.Mutex
	lines   []string
	appends []appendRecording
}

func newWritableFixture(t *testing.T) *writableFixture {
	t.Helper()
	serverTLS, clientTLS := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	f := &writableFixture{}
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
				_, _ = fmt.Fprintf(tlsConn, "* FLAGS (\\Seen \\Flagged)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY 1] stable\r\n* OK [UIDNEXT 8] next\r\n%s OK [READ-WRITE] selected\r\n", tag)
			case "UID":
				_, _ = fmt.Fprintf(tlsConn, "%s OK stored\r\n", tag)
			case "CREATE", "RENAME":
				_, _ = fmt.Fprintf(tlsConn, "%s OK folder done\r\n", tag)
			case "APPEND":
				// Synchronizing literal: the fixture has no LITERAL+ capability,
				// so the client waits for the continuation before streaming the
				// message bytes and the terminating CRLF.
				_, _ = fmt.Fprint(tlsConn, "+ ready\r\n")
				info := parseAppendLine(line)
				if info.size > 0 {
					literal := make([]byte, info.size)
					if _, err := io.ReadFull(reader, literal); err == nil {
						info.body = literal
					}
				}
				f.recordAppend(info)
				_, _ = fmt.Fprintf(tlsConn, "%s OK append done\r\n", tag)
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
	client, err := dial(ctx, account.Named{Name: "flags", Email: "user@qq.com", IMAPHost: host, IMAPPort: port}, "abcdefghijklmnop", clientTLS, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Logout(context.Background()) })
	f.Mutator = client
	return f
}

func (f *writableFixture) record(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, line)
}

// LastStoreLine returns the most recent client command line carrying a STORE
// command (e.g. "A005 UID STORE 7 +FLAGS.SILENT (\Flagged)").
func (f *writableFixture) LastStoreLine() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.lines) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToUpper(f.lines[i]), "STORE") {
			return f.lines[i]
		}
	}
	return ""
}

func TestSetFlagsSendsStoreAddAndRemove(t *testing.T) {
	f := newWritableFixture(t) // 新建：连接记录型 fake，返回可调 Mutator 的构造
	id := mailmodel.MsgID{Folder: "INBOX", UIDValidity: 1, UID: 7}

	if err := f.Mutator.SetFlags(context.Background(), id, []string{"\\Flagged"}, nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := f.LastStoreLine(); !strings.Contains(got, `+FLAGS.SILENT`) || !strings.Contains(got, `\Flagged`) {
		t.Fatalf("add line: %q", got)
	}
	if err := f.Mutator.SetFlags(context.Background(), id, nil, []string{"\\Seen"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := f.LastStoreLine(); !strings.Contains(got, `-FLAGS.SILENT`) || !strings.Contains(got, `\Seen`) {
		t.Fatalf("remove line: %q", got)
	}
	if err := f.Mutator.SetFlags(context.Background(), id, []string{"\\Flagged"}, []string{"\\Seen"}); err == nil {
		t.Fatal("want error when both add and remove are set")
	}
}
