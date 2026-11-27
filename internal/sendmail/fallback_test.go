package sendmail

import (
	"context"
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/netx"
	"github.com/situker/qqmail-cli/internal/output"
)

func unusedLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portText, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestFallbackPolicyRespectsCustomServerAndPort(t *testing.T) {
	cases := []struct {
		name string
		cfg  account.Named
		want bool
	}{
		{"default QQ 465 allows fallback", account.Named{SMTPHost: "smtp.qq.com", SMTPPort: 465}, true},
		{"explicit QQ 587 does not fall back", account.Named{SMTPHost: "smtp.qq.com", SMTPPort: 587}, false},
		{"custom host 465 does not fall back", account.Named{SMTPHost: "mail.example.test", SMTPPort: 465}, false},
		{"custom port does not fall back", account.Named{SMTPHost: "smtp.qq.com", SMTPPort: 2525}, false},
	}
	for _, tc := range cases {
		if got := FallbackAllowed(tc.cfg); got != tc.want {
			t.Errorf("%s: FallbackAllowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSendFallsBackFrom465To587StartTLSAndDeliversOnce(t *testing.T) {
	serverTLS, clientTLS := fixtureTLS(t)
	deadPrimaryPort := unusedLocalPort(t)
	fallback := startSyntheticSMTP(t, syntheticSMTPConfig{
		serverTLS:   serverTLS,
		implicitTLS: false,
	})
	draft, raw := sampleDraft()
	named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: deadPrimaryPort}

	recorder := &netx.ConnectionRecorder{}
	ctx, cancel := context.WithTimeout(netx.WithConnectionRecorder(context.Background(), recorder), 5*time.Second)
	defer cancel()

	err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
		tlsOverride:  clientTLS,
		fallbackPort: fallback.port,
		cmdTimeout:   2 * time.Second,
		retry:        netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
	})
	if err != nil {
		t.Fatalf("expected 587 STARTTLS fallback to succeed, got: %v", err)
	}
	if fallback.received.Load() != 1 || fallback.authHits.Load() != 1 {
		t.Fatalf("fallback received=%d authHits=%d; want 1, 1", fallback.received.Load(), fallback.authHits.Load())
	}
	info := recorder.Latest()
	if info == nil || !info.FallbackUsed || info.Transport != "starttls" || info.Endpoint != fallback.addr || info.Attempts != 1 {
		t.Fatalf("unexpected ConnectionInfo after fallback: %+v", info)
	}
}

func TestSendRefusesMissingStartTLSWithoutPlaintextAuth(t *testing.T) {
	serverTLS, clientTLS := fixtureTLS(t)
	deadPrimaryPort := unusedLocalPort(t)
	fallback := startSyntheticSMTP(t, syntheticSMTPConfig{
		serverTLS:   serverTLS,
		implicitTLS: false,
		noStartTLS:  true,
	})
	draft, raw := sampleDraft()
	named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: deadPrimaryPort}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
		tlsOverride:  clientTLS,
		fallbackPort: fallback.port,
		cmdTimeout:   2 * time.Second,
		retry:        netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
	})
	detail, code := errmap.Details(err)
	if code != output.ExitTLS || detail.Retryable || detail.Context["stage"] != StageSTARTTLS || detail.Context["error_category"] != "starttls_unsupported" {
		t.Fatalf("expected non-retryable exit 21 starttls_unsupported, got code=%d detail=%+v", code, detail)
	}
	if fallback.authHits.Load() != 0 || fallback.received.Load() != 0 || fallback.accepts.Load() != 1 {
		t.Fatalf("plaintext auth/delivery must never occur: accepts=%d authHits=%d received=%d",
			fallback.accepts.Load(), fallback.authHits.Load(), fallback.received.Load())
	}
}

func TestSendRefusesUntrustedStartTLSCertWithoutPlaintextAuth(t *testing.T) {
	serverTLS, _ := fixtureTLS(t)
	deadPrimaryPort := unusedLocalPort(t)
	fallback := startSyntheticSMTP(t, syntheticSMTPConfig{
		serverTLS:   serverTLS,
		implicitTLS: false,
	})
	draft, raw := sampleDraft()
	named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: deadPrimaryPort}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Do not pass clientTLS so self-signed STARTTLS cert fails x509 verification.
	err := sendWithOptions(ctx, named, "abcdefghijklmnop", draft, raw, transportOptions{
		fallbackPort: fallback.port,
		cmdTimeout:   2 * time.Second,
		retry:        netx.RetryConfig{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, NoJitter: true},
	})
	detail, code := errmap.Details(err)
	if code != output.ExitTLS || detail.Retryable || detail.Context["stage"] != StageTLS || detail.Context["error_category"] != netx.CategoryTLSCertificate {
		t.Fatalf("expected non-retryable exit 21 tls_certificate, got code=%d detail=%+v", code, detail)
	}
	if fallback.authHits.Load() != 0 || fallback.received.Load() != 0 || fallback.accepts.Load() != 1 {
		t.Fatalf("auth/delivery must never occur on untrusted STARTTLS cert: accepts=%d authHits=%d received=%d",
			fallback.accepts.Load(), fallback.authHits.Load(), fallback.received.Load())
	}
}

func TestVerifyAuthenticatesWithoutSendingMail(t *testing.T) {
	serverTLS, clientTLS := fixtureTLS(t)

	t.Run("implicit TLS primary", func(t *testing.T) {
		primary := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: primary.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		implicitTrue := true
		res, err := verifyWithOptions(ctx, named, "abcdefghijklmnop", transportOptions{
			tlsOverride: clientTLS,
			implicitTLS: &implicitTrue,
			cmdTimeout:  2 * time.Second,
			retry:       netx.RetryConfig{MaxAttempts: 1},
		})
		if err != nil {
			t.Fatalf("Verify failed: %v", err)
		}
		if res.Transport != "implicit_tls" || res.FallbackUsed || res.Endpoint != primary.addr || res.Attempts != 1 {
			t.Fatalf("unexpected VerifyResult: %+v", res)
		}
		if !slices.Contains(res.Extensions, "AUTH") || !slices.Contains(res.Extensions, "8BITMIME") {
			t.Fatalf("unexpected extensions: %v", res.Extensions)
		}
		if primary.authHits.Load() != 1 || primary.received.Load() != 0 {
			t.Fatalf("Verify must authenticate once and never send mail: auth=%d received=%d", primary.authHits.Load(), primary.received.Load())
		}
	})

	t.Run("465 connect failure falls back to 587 STARTTLS", func(t *testing.T) {
		deadPrimaryPort := unusedLocalPort(t)
		fallback := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: false})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: deadPrimaryPort}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := verifyWithOptions(ctx, named, "abcdefghijklmnop", transportOptions{
			tlsOverride:  clientTLS,
			fallbackPort: fallback.port,
			cmdTimeout:   2 * time.Second,
			retry:        netx.RetryConfig{MaxAttempts: 1},
		})
		if err != nil {
			t.Fatalf("Verify fallback failed: %v", err)
		}
		if res.Transport != "starttls" || !res.FallbackUsed || res.Endpoint != fallback.addr {
			t.Fatalf("unexpected fallback VerifyResult: %+v", res)
		}
		if fallback.authHits.Load() != 1 || fallback.received.Load() != 0 {
			t.Fatalf("Verify fallback must authenticate once and never send mail: auth=%d received=%d", fallback.authHits.Load(), fallback.received.Load())
		}
	})
}
