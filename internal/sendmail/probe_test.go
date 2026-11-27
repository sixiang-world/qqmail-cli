package sendmail

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/netx"
	"github.com/situker/qqmail-cli/internal/output"
)

func TestProbeDistinguishesAuthenticatedAndUnauthenticatedModes(t *testing.T) {
	serverTLS, clientTLS := fixtureTLS(t)
	srv := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true})
	named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: srv.port}
	implicitTrue := true

	t.Run("unauthenticated probe skips AUTH and never sends mail", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := probeWithOptions(ctx, named, "", ProbeOptions{NoAuth: true}, transportOptions{
			tlsOverride: clientTLS,
			implicitTLS: &implicitTrue,
			cmdTimeout:  2 * time.Second,
		})
		if err != nil {
			t.Fatalf("unauthenticated probe failed: %v", err)
		}
		if res.AuthMode != "unauthenticated" || res.Transport != "implicit_tls" || res.Endpoint != srv.addr {
			t.Fatalf("unexpected probe result: %+v", res)
		}
		if srv.authHits.Load() != 0 || srv.received.Load() != 0 {
			t.Fatalf("unauthenticated probe must not send AUTH or mail: auth=%d received=%d", srv.authHits.Load(), srv.received.Load())
		}
		if !slices.Contains(res.Extensions, "AUTH") {
			t.Fatalf("expected AUTH extension, got %v", res.Extensions)
		}
		for _, st := range res.Stages {
			if st.Stage == StageAuth {
				t.Fatalf("unauthenticated probe must not include smtp_auth stage: %+v", res.Stages)
			}
		}
	})

	t.Run("authenticated probe runs AUTH once and never sends mail", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := probeWithOptions(ctx, named, "abcdefghijklmnop", ProbeOptions{NoAuth: false}, transportOptions{
			tlsOverride: clientTLS,
			implicitTLS: &implicitTrue,
			cmdTimeout:  2 * time.Second,
		})
		if err != nil {
			t.Fatalf("authenticated probe failed: %v", err)
		}
		if res.AuthMode != "authenticated" || srv.authHits.Load() != 1 || srv.received.Load() != 0 {
			t.Fatalf("unexpected auth probe state: mode=%s auth=%d received=%d", res.AuthMode, srv.authHits.Load(), srv.received.Load())
		}
		last := res.Stages[len(res.Stages)-1]
		if last.Stage != StageAuth || last.Status != netx.StageStatusPass {
			t.Fatalf("expected final passing smtp_auth stage, got %+v", res.Stages)
		}
	})
}

func TestProbePinpointsEverySMTPFailureStage(t *testing.T) {
	serverTLS, clientTLS := fixtureTLS(t)
	implicitTrue := true
	implicitFalse := false

	t.Run("TCP refused on dead port", func(t *testing.T) {
		deadPort := unusedLocalPort(t)
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: deadPort}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := probeWithOptions(ctx, named, "", ProbeOptions{NoAuth: true}, transportOptions{
			tlsOverride: clientTLS,
			implicitTLS: &implicitTrue,
			cmdTimeout:  2 * time.Second,
		})
		detail, code := errmap.Details(err)
		if code != output.ExitNetwork || detail.Context["stage"] != StageTCP || detail.Context["protocol"] != "smtp" {
			t.Fatalf("expected smtp_tcp network failure, got code=%d detail=%+v", code, detail)
		}
		if len(res.Stages) == 0 || res.Stages[len(res.Stages)-1].Stage != StageTCP || res.Stages[len(res.Stages)-1].Status != netx.StageStatusFail {
			t.Fatalf("unexpected stages: %+v", res.Stages)
		}
	})

	t.Run("TLS untrusted certificate", func(t *testing.T) {
		srv := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: srv.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := probeWithOptions(ctx, named, "", ProbeOptions{NoAuth: true}, transportOptions{
			implicitTLS: &implicitTrue,
			cmdTimeout:  2 * time.Second,
		})
		detail, code := errmap.Details(err)
		if code != output.ExitTLS || detail.Context["stage"] != StageTLS || detail.Context["error_category"] != netx.CategoryTLSCertificate {
			t.Fatalf("expected smtp_tls cert failure, got code=%d detail=%+v", code, detail)
		}
		if len(res.Stages) < 3 || res.Stages[len(res.Stages)-1].Stage != StageTLS {
			t.Fatalf("unexpected stages: %+v", res.Stages)
		}
	})

	t.Run("greeting rejection (rate limit)", func(t *testing.T) {
		srv := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true, greetResponse: "421 too many"})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: srv.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := probeWithOptions(ctx, named, "", ProbeOptions{NoAuth: true}, transportOptions{
			tlsOverride: clientTLS,
			implicitTLS: &implicitTrue,
			cmdTimeout:  2 * time.Second,
		})
		detail, code := errmap.Details(err)
		if code != output.ExitRateLimited || detail.Context["stage"] != StageGreeting {
			t.Fatalf("expected smtp_greeting rate_limited, got code=%d detail=%+v", code, detail)
		}
	})

	t.Run("STARTTLS unsupported on 587", func(t *testing.T) {
		srv := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: false, noStartTLS: true})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: srv.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := probeWithOptions(ctx, named, "abcdefghijklmnop", ProbeOptions{NoAuth: false}, transportOptions{
			tlsOverride: clientTLS,
			implicitTLS: &implicitFalse,
			cmdTimeout:  2 * time.Second,
		})
		detail, code := errmap.Details(err)
		if code != output.ExitTLS || detail.Context["stage"] != StageSTARTTLS || detail.Context["error_category"] != "starttls_unsupported" {
			t.Fatalf("expected smtp_starttls TLS failure, got code=%d detail=%+v", code, detail)
		}
		if srv.authHits.Load() != 0 {
			t.Fatal("probe sent AUTH without STARTTLS")
		}
	})

	t.Run("auth failure on 465", func(t *testing.T) {
		srv := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: true, authResponse: "535 Login Fail"})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: srv.port}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := probeWithOptions(ctx, named, "abcdefghijklmnop", ProbeOptions{NoAuth: false}, transportOptions{
			tlsOverride: clientTLS,
			implicitTLS: &implicitTrue,
			cmdTimeout:  2 * time.Second,
		})
		detail, code := errmap.Details(err)
		if code != output.ExitAuth || detail.Context["stage"] != StageAuth {
			t.Fatalf("expected smtp_auth exit 10, got code=%d detail=%+v", code, detail)
		}
	})

	t.Run("465 connect failure probes 587 STARTTLS once and records primary stages", func(t *testing.T) {
		deadPrimary := unusedLocalPort(t)
		fallback := startSyntheticSMTP(t, syntheticSMTPConfig{serverTLS: serverTLS, implicitTLS: false})
		named := account.Named{Name: "personal", Email: "user@qq.com", SMTPHost: "127.0.0.1", SMTPPort: deadPrimary}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := probeWithOptions(ctx, named, "abcdefghijklmnop", ProbeOptions{NoAuth: false}, transportOptions{
			tlsOverride:  clientTLS,
			fallbackPort: fallback.port,
			cmdTimeout:   2 * time.Second,
		})
		if err != nil {
			t.Fatalf("fallback probe failed: %v", err)
		}
		if !res.FallbackUsed || res.Transport != "starttls" || len(res.PrimaryStages) == 0 || res.PrimaryError == "" {
			t.Fatalf("expected fallback metadata with primary stages, got %+v", res)
		}
		if fallback.accepts.Load() != 1 || fallback.received.Load() != 0 {
			t.Fatalf("expected single probe pass with zero sends: accepts=%d received=%d", fallback.accepts.Load(), fallback.received.Load())
		}
	})
}
