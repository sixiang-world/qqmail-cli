package imapx

import (
	"context"
	"crypto/tls"
	"sync/atomic"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/netx"
	"github.com/situker/qqmail-cli/internal/output"
)

func TestProbeNoAuthStopsBeforeLogin(t *testing.T) {
	var loginSeen atomic.Bool
	cfg, clientTLS := greetingTestServer(t, func(conn *tls.Conn) {
		serveHealthyIMAP(conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := probeWithConfig(ctx, cfg, "", probeConfig{
		ProbeOptions:   ProbeOptions{NoAuth: true, Version: "test"},
		tlsOverride:    clientTLS,
		commandTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("unauthenticated IMAP probe failed: %v", err)
	}
	if res.AuthMode != "unauthenticated" || len(res.CapabilitiesBeforeLogin) == 0 || len(res.CapabilitiesAfterLogin) != 0 {
		t.Fatalf("unexpected unauthenticated probe result: %+v", res)
	}
	for _, st := range res.Stages {
		if st.Stage == stageLogin {
			loginSeen.Store(true)
		}
	}
	if loginSeen.Load() {
		t.Fatalf("unauthenticated IMAP probe must not execute imap_login: %+v", res.Stages)
	}
}

func TestProbeAuthenticatedAndFailureStages(t *testing.T) {
	t.Run("authenticated success records all stages", func(t *testing.T) {
		cfg, clientTLS := greetingTestServer(t, func(conn *tls.Conn) {
			serveHealthyIMAP(conn)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res, err := probeWithConfig(ctx, cfg, "abcdefghijklmnop", probeConfig{
			ProbeOptions:   ProbeOptions{NoAuth: false, Version: "test"},
			tlsOverride:    clientTLS,
			commandTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("authenticated IMAP probe failed: %v", err)
		}
		if res.AuthMode != "authenticated" || len(res.CapabilitiesAfterLogin) == 0 {
			t.Fatalf("unexpected result: %+v", res)
		}
		last := res.Stages[len(res.Stages)-1]
		if last.Stage != stageID || last.Status != netx.StageStatusPass {
			t.Fatalf("expected final passing imap_id stage, got %+v", res.Stages)
		}
	})

	t.Run("TLS untrusted cert pinpoints tls stage", func(t *testing.T) {
		cfg, _ := greetingTestServer(t, func(_ *tls.Conn) {})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res, err := probeWithConfig(ctx, cfg, "", probeConfig{
			ProbeOptions:   ProbeOptions{NoAuth: true},
			commandTimeout: time.Second,
		})
		detail, code := errmap.Details(err)
		if code != output.ExitTLS || detail.Context["stage"] != stageTLS {
			t.Fatalf("expected exit 21 tls stage, got code=%d detail=%+v", code, detail)
		}
		if len(res.Stages) < 3 || res.Stages[len(res.Stages)-1].Stage != stageTLS || res.Stages[len(res.Stages)-1].Status != netx.StageStatusFail {
			t.Fatalf("unexpected stages: %+v", res.Stages)
		}
	})
}
