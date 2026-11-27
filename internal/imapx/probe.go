package imapx

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"
	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/netx"
)

// ProbeOptions controls a single-pass IMAP diagnostic probe.
type ProbeOptions struct {
	NoAuth  bool
	Version string
}

// ProbeResult captures stage-by-stage IMAP diagnostics without leaking secrets.
type ProbeResult struct {
	AuthMode                string             `json:"auth_mode"`
	Server                  string             `json:"server"`
	Endpoint                string             `json:"endpoint,omitempty"`
	Transport               string             `json:"transport"`
	DurationMS              int64              `json:"duration_ms"`
	Stages                  []netx.StageReport `json:"stages"`
	CapabilitiesBeforeLogin []string           `json:"capabilities_before_login"`
	CapabilitiesAfterLogin  []string           `json:"capabilities_after_login"`
}

type probeConfig struct {
	ProbeOptions
	tlsOverride    *tls.Config
	resolver       netx.Resolver
	dialContext    func(ctx context.Context, network, addr string) (net.Conn, error)
	dialTimeout    time.Duration
	tlsTimeout     time.Duration
	commandTimeout time.Duration
}

// Probe performs a single-pass, stage-by-stage IMAP diagnostic probe. When
// opts.NoAuth is true it stops after pre-login CAPABILITY without sending LOGIN.
func Probe(ctx context.Context, cfg account.Named, authCode string, opts ProbeOptions) (ProbeResult, error) {
	return probeWithConfig(ctx, cfg, authCode, probeConfig{ProbeOptions: opts})
}

func probeWithConfig(ctx context.Context, cfg account.Named, authCode string, opts probeConfig) (ProbeResult, error) {
	started := time.Now()
	authMode := "authenticated"
	if opts.NoAuth {
		authMode = "unauthenticated"
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	cmdTimeout := opts.commandTimeout
	if cmdTimeout <= 0 {
		cmdTimeout = DefaultCommandTimeout
	}
	server := net.JoinHostPort(cfg.IMAPHost, fmt.Sprint(cfg.IMAPPort))
	result := ProbeResult{
		AuthMode:                authMode,
		Server:                  server,
		Transport:               "implicit_tls",
		Stages:                  []netx.StageReport{},
		CapabilitiesBeforeLogin: []string{},
		CapabilitiesAfterLogin:  []string{},
	}

	failAt := func(stage string, dur time.Duration, endpoint string, err error) (ProbeResult, error) {
		classified := errmap.Classify(err)
		cat, _ := classified.Context["error_category"].(string)
		result.Stages = append(result.Stages, netx.StageReport{
			Stage:         stage,
			Status:        netx.StageStatusFail,
			DurationMS:    dur.Milliseconds(),
			Endpoint:      endpoint,
			Transport:     "implicit_tls",
			ErrorCategory: cat,
			Detail:        classified.Message,
		})
		result.DurationMS = time.Since(started).Milliseconds()
		return result, classified.WithContext(map[string]any{
			"stage":     stage,
			"server":    server,
			"endpoint":  endpoint,
			"transport": "implicit_tls",
			"protocol":  "imap",
			"auth_mode": authMode,
		})
	}

	dialCfg := netx.DialConfig{
		Host:        cfg.IMAPHost,
		Port:        cfg.IMAPPort,
		TLSConfig:   opts.tlsOverride,
		DialTimeout: opts.dialTimeout,
		TLSTimeout:  opts.tlsTimeout,
		Resolver:    opts.resolver,
		DialContext: opts.dialContext,
	}

	tlsStart := time.Now()
	tlsRes, err := netx.DialTLS(ctx, dialCfg)
	if err != nil {
		classified := errmap.Classify(err)
		failedStage, _ := classified.Context["stage"].(string)
		endpoint, _ := classified.Context["endpoint"].(string)
		if failedStage == "" {
			failedStage = stageTCP
		}
		if failedStage == stageTCP || failedStage == stageTLS {
			result.Stages = append(result.Stages, netx.StageReport{
				Stage: stageDNS, Status: netx.StageStatusPass, DurationMS: 0, Transport: "implicit_tls",
			})
		}
		if failedStage == stageTLS {
			result.Stages = append(result.Stages, netx.StageReport{
				Stage: stageTCP, Status: netx.StageStatusPass, DurationMS: 0, Endpoint: endpoint, Transport: "implicit_tls",
			})
		}
		return failAt(failedStage, time.Since(tlsStart), endpoint, classified)
	}
	result.Endpoint = tlsRes.Endpoint
	result.Stages = append(result.Stages,
		netx.StageReport{Stage: stageDNS, Status: netx.StageStatusPass, DurationMS: tlsRes.DNSDuration.Milliseconds(), Transport: "implicit_tls", Detail: fmt.Sprintf("resolved %d address(es)", len(tlsRes.ResolvedIPs))},
		netx.StageReport{Stage: stageTCP, Status: netx.StageStatusPass, DurationMS: tlsRes.TCPDuration.Milliseconds(), Endpoint: tlsRes.Endpoint, Transport: "implicit_tls"},
		netx.StageReport{Stage: stageTLS, Status: netx.StageStatusPass, DurationMS: tlsRes.TLSDuration.Milliseconds(), Endpoint: tlsRes.Endpoint, Transport: "implicit_tls"},
	)

	client := &Client{conn: tlsRes.Conn, commandTimeout: cmdTimeout}
	transport := &imapTransport{Conn: tlsRes.Conn}
	client.raw = imapclient.New(transport, &imapclient.Options{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}})
	defer client.close()

	greetStart := time.Now()
	if err := client.setDeadline(ctx); err != nil {
		return failAt(stageGreeting, time.Since(greetStart), tlsRes.Endpoint, err)
	}
	stopGreet := client.watchdog(ctx)
	if err := client.raw.WaitGreeting(); err != nil {
		stopGreet()
		return failAt(stageGreeting, time.Since(greetStart), tlsRes.Endpoint, &errmap.Error{
			Kind: errmap.Network, Message: "等待 IMAP 服务器欢迎消息失败",
			Context: map[string]any{"error_category": "imap_greeting"}, Cause: err,
		})
	}
	stopGreet()
	result.Stages = append(result.Stages, netx.StageReport{Stage: stageGreeting, Status: netx.StageStatusPass, DurationMS: time.Since(greetStart).Milliseconds(), Endpoint: tlsRes.Endpoint, Transport: "implicit_tls"})

	capBeforeStart := time.Now()
	if err := client.setDeadline(ctx); err != nil {
		return failAt(stageCapabilityBeforeLogin, time.Since(capBeforeStart), tlsRes.Endpoint, err)
	}
	stopBefore := client.watchdog(ctx)
	before, err := client.raw.Capability().Wait()
	stopBefore()
	if err != nil {
		return failAt(stageCapabilityBeforeLogin, time.Since(capBeforeStart), tlsRes.Endpoint, transport.commandFailure(stageCapabilityBeforeLogin, err))
	}
	result.CapabilitiesBeforeLogin = capStrings(before)
	result.Stages = append(result.Stages, netx.StageReport{Stage: stageCapabilityBeforeLogin, Status: netx.StageStatusPass, DurationMS: time.Since(capBeforeStart).Milliseconds(), Endpoint: tlsRes.Endpoint, Transport: "implicit_tls"})

	if opts.NoAuth {
		_ = client.Logout(context.Background())
		result.DurationMS = time.Since(started).Milliseconds()
		return result, nil
	}

	loginStart := time.Now()
	if err := client.setDeadline(ctx); err != nil {
		return failAt(stageLogin, time.Since(loginStart), tlsRes.Endpoint, err)
	}
	stopLogin := client.watchdog(ctx)
	if err := client.raw.Login(cfg.Email, authCode).Wait(); err != nil {
		stopLogin()
		return failAt(stageLogin, time.Since(loginStart), tlsRes.Endpoint, classifyLoginError(err, cfg, transport.interrupted(err)))
	}
	stopLogin()
	result.Stages = append(result.Stages, netx.StageReport{Stage: stageLogin, Status: netx.StageStatusPass, DurationMS: time.Since(loginStart).Milliseconds(), Endpoint: tlsRes.Endpoint, Transport: "implicit_tls"})

	capAfterStart := time.Now()
	if err := client.setDeadline(ctx); err != nil {
		return failAt(stageCapabilityAfterLogin, time.Since(capAfterStart), tlsRes.Endpoint, err)
	}
	stopAfter := client.watchdog(ctx)
	after, err := client.raw.Capability().Wait()
	stopAfter()
	if err != nil {
		return failAt(stageCapabilityAfterLogin, time.Since(capAfterStart), tlsRes.Endpoint, transport.commandFailure(stageCapabilityAfterLogin, err))
	}
	result.CapabilitiesAfterLogin = capStrings(after)
	result.Stages = append(result.Stages, netx.StageReport{Stage: stageCapabilityAfterLogin, Status: netx.StageStatusPass, DurationMS: time.Since(capAfterStart).Milliseconds(), Endpoint: tlsRes.Endpoint, Transport: "implicit_tls"})

	if after.Has(imap.CapID) {
		idStart := time.Now()
		if err := client.setDeadline(ctx); err != nil {
			return failAt(stageID, time.Since(idStart), tlsRes.Endpoint, err)
		}
		stopID := client.watchdog(ctx)
		_, err := client.raw.ID(&imap.IDData{Name: "qqmail-cli", Version: version}).Wait()
		stopID()
		if err != nil {
			return failAt(stageID, time.Since(idStart), tlsRes.Endpoint, transport.commandFailure(stageID, fmt.Errorf("IMAP ID failed: %w", err)))
		}
		result.Stages = append(result.Stages, netx.StageReport{Stage: stageID, Status: netx.StageStatusPass, DurationMS: time.Since(idStart).Milliseconds(), Endpoint: tlsRes.Endpoint, Transport: "implicit_tls"})
	}

	_ = client.Logout(context.Background())
	result.DurationMS = time.Since(started).Milliseconds()
	return result, nil
}
