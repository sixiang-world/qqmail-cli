package imapx

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
)

func TestDialTCPAndTLSFailureStages(t *testing.T) {
	t.Run("TCP canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cfg := account.Named{IMAPHost: "127.0.0.1", IMAPPort: 1}
		_, err := dial(ctx, cfg, "", nil, time.Second)
		assertDialStage(t, err, "tcp", errmap.Network)
	})
	t.Run("TLS untrusted certificate", func(t *testing.T) {
		cfg, _ := greetingTestServer(t, func(_ *tls.Conn) {
			t.Error("client accepted untrusted certificate")
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := dial(ctx, cfg, "", nil, time.Second)
		assertDialStage(t, err, "tls", errmap.TLS)
	})
}

func TestDialIMAPFailureStages(t *testing.T) {
	for _, test := range []struct {
		name, stage string
		response    string
		kind        errmap.Kind
	}{
		{name: "greeting EOF", stage: "imap_greeting", kind: errmap.Network},
		{name: "pre-login CAPABILITY EOF", stage: "imap_capability_before_login", kind: errmap.Network},
		{name: "login EOF", stage: "imap_login", kind: errmap.Network},
		{name: "login refusal", stage: "imap_login", response: "NO [AUTHENTICATIONFAILED] fixture refused", kind: errmap.AuthFailed},
		{name: "post-login CAPABILITY timeout", stage: "imap_capability_after_login", response: "timeout", kind: errmap.Network},
		{name: "ID EOF", stage: "imap_id", kind: errmap.Network},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, clientTLS := greetingTestServer(t, func(conn *tls.Conn) {
				if test.stage == "imap_greeting" {
					return
				}
				_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] fixture ready\r\n")
				reader := bufio.NewReader(conn)
				for _, step := range []struct{ stage, cmd, response string }{
					{"imap_capability_before_login", "CAPABILITY", "* CAPABILITY IMAP4rev1\r\n"},
					{"imap_login", "LOGIN", ""},
					{"imap_capability_after_login", "CAPABILITY", "* CAPABILITY IMAP4rev1 ID\r\n"},
					{"imap_id", "ID", "* ID NIL\r\n"},
				} {
					line, err := reader.ReadString('\n')
					if err != nil {
						t.Errorf("did not receive %s: %v", step.cmd, err)
						return
					}
					fields := strings.Fields(line)
					if len(fields) < 2 || strings.ToUpper(fields[1]) != step.cmd {
						t.Errorf("unexpected command while waiting for %s", step.cmd)
						return
					}
					if step.stage == test.stage {
						switch test.response {
						case "timeout":
							_, _ = io.Copy(io.Discard, reader)
						case "":
						default:
							_, _ = fmt.Fprintf(conn, "%s %s\r\n", fields[0], test.response)
						}
						return
					}
					if step.cmd == "LOGIN" {
						// A tagged capability response prevents the library from
						// automatically issuing a second CAPABILITY after login.
						_, _ = fmt.Fprintf(conn, "%s OK [CAPABILITY IMAP4rev1 ID] fixture\r\n", fields[0])
					} else {
						_, _ = fmt.Fprintf(conn, "%s%s OK fixture\r\n", step.response, fields[0])
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client, err := dial(ctx, cfg, "synthetic-fixture", clientTLS, 500*time.Millisecond)
			if client != nil {
				client.close()
			}
			assertDialStage(t, err, test.stage, test.kind)
		})
	}
}

func assertDialStage(t *testing.T, err error, stage string, kind errmap.Kind) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a connection failure")
	}
	failure := errmap.Classify(err)
	if failure.Kind != kind || failure.Context["stage"] != stage {
		t.Fatalf("kind and stage = %s, %v; want %s, %s (error: %v)", failure.Kind, failure.Context["stage"], kind, stage, err)
	}
}
