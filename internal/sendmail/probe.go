package sendmail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/netx"
)

// ProbeOptions controls a single-pass SMTP diagnostic probe.
type ProbeOptions struct {
	NoAuth bool
}

// ProbeResult captures stage-by-stage SMTP diagnostics without sending mail or
// exposing credentials.
type ProbeResult struct {
	AuthMode      string             `json:"auth_mode"`
	Server        string             `json:"server"`
	Endpoint      string             `json:"endpoint,omitempty"`
	Transport     string             `json:"transport"`
	FallbackUsed  bool               `json:"fallback_used"`
	DurationMS    int64              `json:"duration_ms"`
	Stages        []netx.StageReport `json:"stages"`
	Extensions    []string           `json:"extensions"`
	PrimaryStages []netx.StageReport `json:"primary_stages,omitempty"`
	PrimaryError  string             `json:"primary_error,omitempty"`
}

// CompletedProbeStages returns the SMTP stages known to have passed before a
// failure for the given transport ("implicit_tls" or "starttls").
func CompletedProbeStages(transport, failedStage string) []string {
	var order []string
	if transport == "starttls" {
		order = []string{StageDNS, StageTCP, StageGreeting, StageEHLO, StageSTARTTLS, StageTLS, StageAuth}
	} else {
		order = []string{StageDNS, StageTCP, StageTLS, StageGreeting, StageEHLO, StageAuth}
	}
	if idx := slices.Index(order, failedStage); idx >= 0 {
		return order[:idx]
	}
	return nil
}

// Probe performs a single-pass, stage-by-stage SMTP diagnostic check. It never
// sends mail and never performs repeated retry storms. When opts.NoAuth is true
// it probes up to EHLO (and STARTTLS + post-STARTTLS EHLO on 587) without
// reading credentials or sending AUTH.
func Probe(ctx context.Context, named account.Named, authCode string, opts ProbeOptions) (ProbeResult, error) {
	return probeWithOptions(ctx, named, authCode, opts, transportOptions{tlsOverride: tlsOverride})
}

func probeWithOptions(ctx context.Context, named account.Named, authCode string, probeOpts ProbeOptions, opts transportOptions) (ProbeResult, error) {
	tlsConfig := netx.StrictTLSConfig(named.SMTPHost, opts.tlsOverride)
	fallbackPort := opts.fallbackPort
	if fallbackPort == 0 && FallbackAllowed(named) {
		fallbackPort = 587
	}
	implicit := named.SMTPPort == 465
	if opts.implicitTLS != nil {
		implicit = *opts.implicitTLS
	}
	if fallbackPort > 0 {
		implicit = true
	}

	primaryRes, primaryErr := probeEndpoint(ctx, named.SMTPHost, named.SMTPPort, named.Email, authCode, tlsConfig, implicit, probeOpts, opts)
	if primaryErr == nil {
		return primaryRes, nil
	}
	if fallbackPort <= 0 || !netx.IsRetryable(primaryErr) || ctx.Err() != nil {
		return primaryRes, primaryErr
	}

	// Only probe 587 STARTTLS once when primary 465 failed with a transient
	// pre-auth error under a fallback-allowed configuration.
	fallbackRes, fallbackErr := probeEndpoint(ctx, named.SMTPHost, fallbackPort, named.Email, authCode, tlsConfig, false, probeOpts, opts)
	fallbackRes.PrimaryStages = primaryRes.Stages
	fallbackRes.PrimaryError = errmap.Classify(primaryErr).Message
	if fallbackErr == nil {
		fallbackRes.FallbackUsed = true
		return fallbackRes, nil
	}
	classified := errmap.Classify(fallbackErr).WithContext(map[string]any{
		"fallback_attempted": true,
		"primary_server":     primaryRes.Server,
	})
	if stage, ok := errmap.Classify(primaryErr).Context["stage"]; ok {
		classified = classified.WithContext(map[string]any{"primary_stage": stage})
	}
	return fallbackRes, classified
}

func probeEndpoint(
	ctx context.Context,
	host string,
	port int,
	email, authCode string,
	tlsConfig *tls.Config,
	implicitTLS bool,
	probeOpts ProbeOptions,
	opts transportOptions,
) (ProbeResult, error) {
	started := time.Now()
	authMode := "authenticated"
	if probeOpts.NoAuth {
		authMode = "unauthenticated"
	}
	transport := "starttls"
	if implicitTLS {
		transport = "implicit_tls"
	}
	server := net.JoinHostPort(host, strconv.Itoa(port))
	res := ProbeResult{
		AuthMode:   authMode,
		Server:     server,
		Transport:  transport,
		Stages:     []netx.StageReport{},
		Extensions: []string{},
	}

	failAt := func(stage string, dur time.Duration, endpoint string, err error) (ProbeResult, error) {
		classified := errmap.Classify(err)
		cat, _ := classified.Context["error_category"].(string)
		res.Stages = append(res.Stages, netx.StageReport{
			Stage:         stage,
			Status:        netx.StageStatusFail,
			DurationMS:    dur.Milliseconds(),
			Endpoint:      endpoint,
			Transport:     transport,
			ErrorCategory: cat,
			Detail:        classified.Message,
		})
		res.DurationMS = time.Since(started).Milliseconds()
		return res, classified.WithContext(map[string]any{
			"stage":     stage,
			"server":    server,
			"endpoint":  endpoint,
			"transport": transport,
			"protocol":  "smtp",
			"auth_mode": authMode,
		})
	}

	cmdTimeout := opts.cmdTimeout
	if cmdTimeout <= 0 {
		cmdTimeout = 20 * time.Second
	}
	tlsCfg := netx.StrictTLSConfig(host, tlsConfig)
	dialCfg := netx.DialConfig{
		Host:        host,
		Port:        port,
		TLSConfig:   tlsCfg,
		DialTimeout: opts.dialTimeout,
		TLSTimeout:  opts.tlsTimeout,
		Resolver:    opts.resolver,
		DialContext: opts.dialContext,
	}

	var (
		client   *smtp.Client
		rawConn  net.Conn
		tlsState func() (tls.ConnectionState, bool)
		endpoint string
	)

	if implicitTLS {
		dialStart := time.Now()
		tlsRes, err := netx.DialTLS(ctx, dialCfg)
		if err != nil {
			mapped := mapNetStageToSMTP(err, transport)
			failedStage, _ := mapped.Context["stage"].(string)
			ep, _ := mapped.Context["endpoint"].(string)
			if failedStage == "" {
				failedStage = StageTCP
			}
			if failedStage == StageTCP || failedStage == StageTLS {
				res.Stages = append(res.Stages, netx.StageReport{Stage: StageDNS, Status: netx.StageStatusPass, DurationMS: 0, Transport: transport})
			}
			if failedStage == StageTLS {
				res.Stages = append(res.Stages, netx.StageReport{Stage: StageTCP, Status: netx.StageStatusPass, DurationMS: 0, Endpoint: ep, Transport: transport})
			}
			return failAt(failedStage, time.Since(dialStart), ep, mapped)
		}
		endpoint = tlsRes.Endpoint
		res.Endpoint = endpoint
		rawConn = tlsRes.Conn
		tlsState = func() (tls.ConnectionState, bool) { return tlsRes.Conn.ConnectionState(), true }
		res.Stages = append(res.Stages,
			netx.StageReport{Stage: StageDNS, Status: netx.StageStatusPass, DurationMS: tlsRes.DNSDuration.Milliseconds(), Transport: transport, Detail: fmt.Sprintf("resolved %d address(es)", len(tlsRes.ResolvedIPs))},
			netx.StageReport{Stage: StageTCP, Status: netx.StageStatusPass, DurationMS: tlsRes.TCPDuration.Milliseconds(), Endpoint: endpoint, Transport: transport},
			netx.StageReport{Stage: StageTLS, Status: netx.StageStatusPass, DurationMS: tlsRes.TLSDuration.Milliseconds(), Endpoint: endpoint, Transport: transport},
		)

		tracker := &smtpStageConn{Conn: tlsRes.Conn}
		client = smtp.NewClient(tracker)
		defer func() { _ = client.Close() }()
		client.CommandTimeout = boundDuration(ctx, cmdTimeout)
		helloStart := time.Now()
		stopWatch := context.AfterFunc(ctx, func() { _ = tlsRes.Conn.Close() })
		err = client.Hello("localhost")
		stopped := stopWatch()
		if err != nil || !stopped {
			if err == nil {
				err = ctx.Err()
			}
			stage := tracker.stage()
			if stage == StageEHLO {
				greetDur := tracker.greetingDuration(helloStart)
				res.Stages = append(res.Stages, netx.StageReport{Stage: StageGreeting, Status: netx.StageStatusPass, DurationMS: greetDur.Milliseconds(), Endpoint: endpoint, Transport: transport})
			}
			return failAt(stage, time.Since(helloStart), endpoint, classifySMTPStage(stage, err, server, endpoint, true))
		}
		greetDur := tracker.greetingDuration(helloStart)
		ehloDur := time.Since(helloStart) - greetDur
		if ehloDur < 0 {
			ehloDur = 0
		}
		res.Stages = append(res.Stages,
			netx.StageReport{Stage: StageGreeting, Status: netx.StageStatusPass, DurationMS: greetDur.Milliseconds(), Endpoint: endpoint, Transport: transport},
			netx.StageReport{Stage: StageEHLO, Status: netx.StageStatusPass, DurationMS: ehloDur.Milliseconds(), Endpoint: endpoint, Transport: transport},
		)
	} else {
		tcpStart := time.Now()
		tcpRes, err := netx.DialTCP(ctx, dialCfg)
		if err != nil {
			mapped := mapNetStageToSMTP(err, transport)
			failedStage, _ := mapped.Context["stage"].(string)
			ep, _ := mapped.Context["endpoint"].(string)
			if failedStage == "" {
				failedStage = StageTCP
			}
			if failedStage == StageTCP {
				res.Stages = append(res.Stages, netx.StageReport{Stage: StageDNS, Status: netx.StageStatusPass, DurationMS: 0, Transport: transport})
			}
			return failAt(failedStage, time.Since(tcpStart), ep, mapped)
		}
		endpoint = tcpRes.Endpoint
		res.Endpoint = endpoint
		rawConn = tcpRes.Conn
		res.Stages = append(res.Stages,
			netx.StageReport{Stage: StageDNS, Status: netx.StageStatusPass, DurationMS: tcpRes.DNSDuration.Milliseconds(), Transport: transport, Detail: fmt.Sprintf("resolved %d address(es)", len(tcpRes.ResolvedIPs))},
			netx.StageReport{Stage: StageTCP, Status: netx.StageStatusPass, DurationMS: tcpRes.TCPDuration.Milliseconds(), Endpoint: endpoint, Transport: transport},
		)

		hsCtx, cancelHS := context.WithTimeout(ctx, boundDuration(ctx, cmdTimeout))
		defer cancelHS()
		stopWatch := context.AfterFunc(hsCtx, func() { _ = tcpRes.Conn.Close() })
		tracker := &smtpStageConn{Conn: tcpRes.Conn}
		stStart := time.Now()
		client, err = smtp.NewClientStartTLS(tracker, tlsCfg)
		if err != nil {
			_ = stopWatch()
			_ = tcpRes.Conn.Close()
			if strings.Contains(strings.ToLower(err.Error()), "doesn't support starttls") {
				greetDur := tracker.greetingDuration(stStart)
				res.Stages = append(res.Stages,
					netx.StageReport{Stage: StageGreeting, Status: netx.StageStatusPass, DurationMS: greetDur.Milliseconds(), Endpoint: endpoint, Transport: transport},
					netx.StageReport{Stage: StageEHLO, Status: netx.StageStatusPass, DurationMS: 0, Endpoint: endpoint, Transport: transport},
				)
				return failAt(StageSTARTTLS, time.Since(stStart), endpoint, (&errmap.Error{
					Kind:       errmap.TLS,
					Message:    "SMTP 服务器不支持 STARTTLS，拒绝明文认证",
					Suggestion: "确认服务器端口支持 STARTTLS 或改用 465 隐式 TLS；CLI 禁止明文认证",
					Context:    map[string]any{"error_category": "starttls_unsupported"},
					Cause:      err,
				}).WithRetryable(false))
			}
			stage := tracker.stage()
			if stage == StageEHLO || stage == StageSTARTTLS {
				res.Stages = append(res.Stages, netx.StageReport{Stage: StageGreeting, Status: netx.StageStatusPass, DurationMS: tracker.greetingDuration(stStart).Milliseconds(), Endpoint: endpoint, Transport: transport})
			}
			if stage == StageSTARTTLS {
				res.Stages = append(res.Stages, netx.StageReport{Stage: StageEHLO, Status: netx.StageStatusPass, DurationMS: 0, Endpoint: endpoint, Transport: transport})
			}
			return failAt(stage, time.Since(stStart), endpoint, classifySMTPStage(stage, err, server, endpoint, true))
		}
		defer func() { _ = client.Close() }()
		greetDur := tracker.greetingDuration(stStart)
		res.Stages = append(res.Stages,
			netx.StageReport{Stage: StageGreeting, Status: netx.StageStatusPass, DurationMS: greetDur.Milliseconds(), Endpoint: endpoint, Transport: transport},
			netx.StageReport{Stage: StageEHLO, Status: netx.StageStatusPass, DurationMS: 0, Endpoint: endpoint, Transport: transport},
			netx.StageReport{Stage: StageSTARTTLS, Status: netx.StageStatusPass, DurationMS: time.Since(stStart).Milliseconds(), Endpoint: endpoint, Transport: transport},
		)

		tlsStart := time.Now()
		client.CommandTimeout = boundDuration(hsCtx, cmdTimeout)
		if err := client.Hello("localhost"); err != nil {
			_ = stopWatch()
			state, ok := client.TLSConnectionState()
			if !ok || !state.HandshakeComplete {
				return failAt(StageTLS, time.Since(tlsStart), endpoint, mapNetStageToSMTP(netx.ClassifyTLSError(err, server, endpoint), transport))
			}
			res.Stages = append(res.Stages, netx.StageReport{Stage: StageTLS, Status: netx.StageStatusPass, DurationMS: time.Since(tlsStart).Milliseconds(), Endpoint: endpoint, Transport: transport})
			return failAt(StageEHLO, time.Since(tlsStart), endpoint, classifySMTPStage(StageEHLO, err, server, endpoint, true))
		}
		if !stopWatch() {
			return failAt(StageTLS, time.Since(tlsStart), endpoint, mapNetStageToSMTP(netx.ClassifyTLSError(hsCtx.Err(), server, endpoint), transport))
		}
		tlsState = client.TLSConnectionState
		if state, ok := tlsState(); !ok || !state.HandshakeComplete {
			return failAt(StageTLS, time.Since(tlsStart), endpoint, (&errmap.Error{
				Kind:       errmap.TLS,
				Message:    "SMTP STARTTLS 升级未完成，拒绝明文认证",
				Suggestion: "检查服务器 STARTTLS 与证书配置",
				Context:    map[string]any{"error_category": netx.CategoryTLSProtocol},
			}).WithRetryable(false))
		}
		res.Stages = append(res.Stages, netx.StageReport{Stage: StageTLS, Status: netx.StageStatusPass, DurationMS: time.Since(tlsStart).Milliseconds(), Endpoint: endpoint, Transport: transport})
	}

	res.Extensions = collectExtensions(client)
	if probeOpts.NoAuth {
		_ = client.Quit()
		res.DurationMS = time.Since(started).Milliseconds()
		return res, nil
	}

	if state, ok := tlsState(); !ok || !state.HandshakeComplete {
		return failAt(StageTLS, 0, endpoint, (&errmap.Error{
			Kind: errmap.TLS, Message: "SMTP 连接未建立有效 TLS 加密，拒绝认证",
			Context: map[string]any{"error_category": netx.CategoryTLSProtocol},
		}).WithRetryable(false))
	}

	authStart := time.Now()
	client.CommandTimeout = boundDuration(ctx, cmdTimeout)
	stopAuth := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	authErr := client.Auth(sasl.NewPlainClient("", email, authCode))
	stopAuth()
	if authErr != nil {
		return failAt(StageAuth, time.Since(authStart), endpoint, classifySMTPStage(StageAuth, authErr, server, endpoint, false))
	}
	res.Stages = append(res.Stages, netx.StageReport{Stage: StageAuth, Status: netx.StageStatusPass, DurationMS: time.Since(authStart).Milliseconds(), Endpoint: endpoint, Transport: transport})
	_ = client.Noop()
	_ = client.Quit()
	res.DurationMS = time.Since(started).Milliseconds()
	return res, nil
}
