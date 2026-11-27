package sendmail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/netx"
)

const (
	MaxMessagesPerInvocation = 1
	DefaultMessageInterval   = 2 * time.Second

	StageDNS      = "smtp_dns"
	StageTCP      = "smtp_tcp"
	StageTLS      = "smtp_tls"
	StageGreeting = "smtp_greeting"
	StageEHLO     = "smtp_ehlo"
	StageSTARTTLS = "smtp_starttls"
	StageAuth     = "smtp_auth"
	StageData     = "smtp_data"
)

type transportOptions struct {
	tlsOverride  *tls.Config
	resolver     netx.Resolver
	dialContext  func(ctx context.Context, network, addr string) (net.Conn, error)
	dialTimeout  time.Duration
	tlsTimeout   time.Duration
	cmdTimeout   time.Duration
	retry        netx.RetryConfig
	fallbackPort int
	implicitTLS  *bool // nil uses port==465; tests set explicitly for ephemeral ports
}

type dialedSMTP struct {
	client       *smtp.Client
	conn         net.Conn
	tlsState     func() (tls.ConnectionState, bool)
	server       string
	endpoint     string
	transport    string
	fallbackUsed bool
	resolvedIPs  []string
}

type VerifyResult struct {
	Server       string   `json:"server"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Transport    string   `json:"transport"`
	FallbackUsed bool     `json:"fallback_used"`
	Attempts     int      `json:"attempts"`
	Extensions   []string `json:"extensions"`
	DurationMS   int64    `json:"duration_ms"`
}

// FallbackAllowed reports whether implicit-TLS 465 -> STARTTLS 587 fallback is
// permitted for this account configuration. Custom hosts and explicit non-465
// ports always follow the user's configuration without fallback.
func FallbackAllowed(named account.Named) bool {
	return named.SMTPHost == "smtp.qq.com" && named.SMTPPort == 465
}

func Send(ctx context.Context, named account.Named, authCode string, draft Draft, raw []byte) error {
	return sendWithOptions(ctx, named, authCode, draft, raw, transportOptions{tlsOverride: tlsOverride})
}

// Verify connects to SMTP using the same strict TLS and safe fallback rules as
// Send, authenticates with the authorization code, and issues NOOP/QUIT without
// sending any message.
func Verify(ctx context.Context, named account.Named, authCode string) (VerifyResult, error) {
	return verifyWithOptions(ctx, named, authCode, transportOptions{tlsOverride: tlsOverride})
}

func verifyWithOptions(ctx context.Context, named account.Named, authCode string, opts transportOptions) (VerifyResult, error) {
	start := time.Now()
	dialed, attempts, err := netx.Retry(ctx, opts.retry, func(attemptCtx context.Context, _ int) (*dialedSMTP, error) {
		return dialAccount(attemptCtx, named, opts)
	})
	if err != nil {
		return VerifyResult{}, err
	}
	defer func() { _ = dialed.client.Close() }()

	metaCtx := map[string]any{
		"server":        dialed.server,
		"endpoint":      dialed.endpoint,
		"transport":     dialed.transport,
		"attempts":      attempts,
		"fallback_used": dialed.fallbackUsed,
	}

	if dialed.tlsState == nil {
		return VerifyResult{}, (&errmap.Error{
			Kind:       errmap.TLS,
			Message:    "SMTP 连接未建立有效 TLS 加密，拒绝认证",
			Suggestion: "检查服务器端口和 TLS 配置；CLI 禁止明文认证",
			Context:    map[string]any{"stage": StageTLS, "error_category": netx.CategoryTLSProtocol},
		}).WithRetryable(false).WithContext(metaCtx)
	}
	if state, ok := dialed.tlsState(); !ok || !state.HandshakeComplete {
		return VerifyResult{}, (&errmap.Error{
			Kind:       errmap.TLS,
			Message:    "SMTP 连接未建立有效 TLS 加密，拒绝认证",
			Suggestion: "检查服务器端口和 TLS 配置；CLI 禁止明文认证",
			Context:    map[string]any{"stage": StageTLS, "error_category": netx.CategoryTLSProtocol},
		}).WithRetryable(false).WithContext(metaCtx)
	}

	cmdTimeout := opts.cmdTimeout
	if cmdTimeout <= 0 {
		cmdTimeout = 20 * time.Second
	}
	dialed.client.CommandTimeout = boundDuration(ctx, cmdTimeout)

	stopWatch := context.AfterFunc(ctx, func() { _ = dialed.conn.Close() })
	defer stopWatch()

	if err := dialed.client.Auth(sasl.NewPlainClient("", named.Email, authCode)); err != nil {
		return VerifyResult{}, classifySMTPStage(StageAuth, err, dialed.server, dialed.endpoint, false).WithContext(metaCtx)
	}
	extensions := collectExtensions(dialed.client)
	_ = dialed.client.Noop()
	_ = dialed.client.Quit()

	res := VerifyResult{
		Server:       dialed.server,
		Endpoint:     dialed.endpoint,
		Transport:    dialed.transport,
		FallbackUsed: dialed.fallbackUsed,
		Attempts:     attempts,
		Extensions:   extensions,
		DurationMS:   time.Since(start).Milliseconds(),
	}
	netx.RecordConnection(ctx, netx.ConnectionInfo{
		Protocol:     "smtp",
		Attempts:     attempts,
		Stage:        "smtp_verified",
		Endpoint:     dialed.endpoint,
		Server:       dialed.server,
		Transport:    dialed.transport,
		FallbackUsed: dialed.fallbackUsed,
		ResolvedIPs:  dialed.resolvedIPs,
	})
	return res, nil
}

func collectExtensions(client *smtp.Client) []string {
	known := []string{"8BITMIME", "AUTH", "ENHANCEDSTATUSCODES", "PIPELINING", "SIZE", "SMTPUTF8", "STARTTLS"}
	out := make([]string, 0, len(known))
	for _, name := range known {
		if ok, _ := client.Extension(name); ok {
			out = append(out, name)
		}
	}
	return out
}

func sendWithOptions(ctx context.Context, named account.Named, authCode string, draft Draft, raw []byte, opts transportOptions) error {
	// Only pre-auth connection setup (DNS, TCP, transient TLS/greeting/EHLO
	// failures) is eligible for bounded retry or secure port fallback.
	// Auth, MAIL/RCPT/DATA, certificate errors, rate limits, and uncertain
	// delivery are never retried or failed over.
	dialed, attempts, err := netx.Retry(ctx, opts.retry, func(attemptCtx context.Context, _ int) (*dialedSMTP, error) {
		return dialAccount(attemptCtx, named, opts)
	})
	if err != nil {
		return err
	}
	defer func() { _ = dialed.client.Close() }()

	metaCtx := map[string]any{
		"server":        dialed.server,
		"endpoint":      dialed.endpoint,
		"transport":     dialed.transport,
		"attempts":      attempts,
		"fallback_used": dialed.fallbackUsed,
	}

	// Defense in depth: refuse to authenticate if TLS is not active and verified.
	if dialed.tlsState == nil {
		return (&errmap.Error{
			Kind:       errmap.TLS,
			Message:    "SMTP 连接未建立有效 TLS 加密，拒绝认证",
			Suggestion: "检查服务器端口和 TLS 配置；CLI 禁止明文认证",
			Context:    map[string]any{"stage": StageTLS, "error_category": netx.CategoryTLSProtocol},
		}).WithRetryable(false).WithContext(metaCtx)
	}
	if state, ok := dialed.tlsState(); !ok || !state.HandshakeComplete {
		return (&errmap.Error{
			Kind:       errmap.TLS,
			Message:    "SMTP 连接未建立有效 TLS 加密，拒绝认证",
			Suggestion: "检查服务器端口和 TLS 配置；CLI 禁止明文认证",
			Context:    map[string]any{"stage": StageTLS, "error_category": netx.CategoryTLSProtocol},
		}).WithRetryable(false).WithContext(metaCtx)
	}

	cmdTimeout := opts.cmdTimeout
	if cmdTimeout <= 0 {
		cmdTimeout = 60 * time.Second
	}
	dialed.client.CommandTimeout = boundDuration(ctx, cmdTimeout)
	dialed.client.SubmissionTimeout = boundDuration(ctx, 120*time.Second)

	stopWatch := context.AfterFunc(ctx, func() { _ = dialed.conn.Close() })
	defer stopWatch()

	if err := dialed.client.Auth(sasl.NewPlainClient("", named.Email, authCode)); err != nil {
		return classifySMTPStage(StageAuth, err, dialed.server, dialed.endpoint, false).WithContext(metaCtx)
	}
	if err := dialed.client.SendMail(draft.From.Address, Recipients(draft), bytes.NewReader(raw)); err != nil {
		// A transport failure during submission may happen after server acceptance.
		// Never present it as a retryable network error to an Agent.
		var smtpErr *smtp.SMTPError
		if errors.As(err, &smtpErr) {
			return classifySMTPStage(StageData, err, dialed.server, dialed.endpoint, false).WithContext(metaCtx)
		}
		return (&errmap.Error{
			Kind:       errmap.DeliveryUncertain,
			Message:    "SMTP 投递结果不确定，禁止自动重试",
			Suggestion: "请人工核对已发送邮件或收件端后再决定是否重发",
			Context:    map[string]any{"stage": StageData, "error_category": "delivery_uncertain"},
			Cause:      err,
		}).WithRetryable(false).WithContext(metaCtx)
	}
	netx.RecordConnection(ctx, netx.ConnectionInfo{
		Protocol:     "smtp",
		Attempts:     attempts,
		Stage:        "smtp_sent",
		Endpoint:     dialed.endpoint,
		Server:       dialed.server,
		Transport:    dialed.transport,
		FallbackUsed: dialed.fallbackUsed,
		ResolvedIPs:  dialed.resolvedIPs,
	})
	// The server has already accepted the message. A failed QUIT must not
	// turn an accepted delivery into a retryable failure.
	_ = dialed.client.Quit()
	return nil
}

// tlsOverride is a test seam: production code never sets it, tests inject a
// config that trusts the local fixture certificate.
var tlsOverride *tls.Config

func dialAccount(ctx context.Context, named account.Named, opts transportOptions) (*dialedSMTP, error) {
	tlsConfig := netx.StrictTLSConfig(named.SMTPHost, opts.tlsOverride)
	fallbackPort := opts.fallbackPort
	if fallbackPort == 0 && FallbackAllowed(named) {
		fallbackPort = 587
	}
	if fallbackPort > 0 {
		return dialWithFallbackOptions(ctx, named.SMTPHost, named.SMTPPort, fallbackPort, tlsConfig, opts)
	}
	implicit := named.SMTPPort == 465
	if opts.implicitTLS != nil {
		implicit = *opts.implicitTLS
	}
	return dialEndpointOptions(ctx, named.SMTPHost, named.SMTPPort, tlsConfig, implicit, opts)
}

// dialWithFallback tries the implicit-TLS primary port and falls back to
// STARTTLS on the secondary ONLY when the primary fails with a transient
// pre-authentication connection/handshake error. Certificate errors, TLS
// alerts, rate limits, and authentication/submission failures are never failed
// over.
func dialWithFallback(ctx context.Context, host, primaryPort, fallbackPort string, tlsConfig *tls.Config) (*smtp.Client, error) {
	pPort, err := strconv.Atoi(primaryPort)
	if err != nil {
		return nil, err
	}
	fPort, err := strconv.Atoi(fallbackPort)
	if err != nil {
		return nil, err
	}
	dialed, err := dialWithFallbackOptions(ctx, host, pPort, fPort, tlsConfig, transportOptions{})
	if err != nil {
		return nil, err
	}
	return dialed.client, nil
}

func dialWithFallbackOptions(ctx context.Context, host string, primaryPort, fallbackPort int, tlsConfig *tls.Config, opts transportOptions) (*dialedSMTP, error) {
	primary, err := dialEndpointOptions(ctx, host, primaryPort, tlsConfig, true, opts)
	if err == nil {
		return primary, nil
	}
	// Never fall back on certificate errors, TLS protocol errors, rate limits,
	// or explicit cancellation.
	if !netx.IsRetryable(err) || ctx.Err() != nil {
		return nil, err
	}
	fallback, fallbackErr := dialEndpointOptions(ctx, host, fallbackPort, tlsConfig, false, opts)
	if fallbackErr == nil {
		fallback.fallbackUsed = true
		return fallback, nil
	}
	primaryClassified := errmap.Classify(err)
	fallbackClassified := errmap.Classify(fallbackErr)
	extra := map[string]any{
		"fallback_attempted": true,
		"primary_server":     net.JoinHostPort(host, strconv.Itoa(primaryPort)),
	}
	if stage, ok := primaryClassified.Context["stage"]; ok {
		extra["primary_stage"] = stage
	}
	return nil, fallbackClassified.WithContext(extra)
}

func dialEndpointOptions(ctx context.Context, host string, port int, tlsConfig *tls.Config, implicitTLS bool, opts transportOptions) (*dialedSMTP, error) {
	server := net.JoinHostPort(host, strconv.Itoa(port))
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
	cmdTimeout := opts.cmdTimeout
	if cmdTimeout <= 0 {
		cmdTimeout = 20 * time.Second
	}

	if implicitTLS {
		res, err := netx.DialTLS(ctx, dialCfg)
		if err != nil {
			return nil, mapNetStageToSMTP(err, "implicit_tls")
		}
		tracker := &smtpStageConn{Conn: res.Conn}
		client := smtp.NewClient(tracker)
		client.CommandTimeout = boundDuration(ctx, cmdTimeout)
		stopWatch := context.AfterFunc(ctx, func() { _ = res.Conn.Close() })
		err = client.Hello("localhost")
		stopped := stopWatch()
		if err != nil {
			_ = client.Close()
			return nil, classifySMTPStage(tracker.stage(), err, server, res.Endpoint, true).WithContext(map[string]any{"transport": "implicit_tls"})
		}
		if !stopped {
			_ = client.Close()
			return nil, classifySMTPStage(tracker.stage(), ctx.Err(), server, res.Endpoint, true).WithContext(map[string]any{"transport": "implicit_tls"})
		}
		return &dialedSMTP{
			client:      client,
			conn:        res.Conn,
			tlsState:    func() (tls.ConnectionState, bool) { return res.Conn.ConnectionState(), true },
			server:      server,
			endpoint:    res.Endpoint,
			transport:   "implicit_tls",
			resolvedIPs: res.ResolvedIPs,
		}, nil
	}

	tcpRes, err := netx.DialTCP(ctx, dialCfg)
	if err != nil {
		return nil, mapNetStageToSMTP(err, "starttls")
	}
	hsCtx, cancelHS := context.WithTimeout(ctx, boundDuration(ctx, cmdTimeout))
	defer cancelHS()
	stopWatch := context.AfterFunc(hsCtx, func() { _ = tcpRes.Conn.Close() })

	tracker := &smtpStageConn{Conn: tcpRes.Conn}
	client, err := smtp.NewClientStartTLS(tracker, tlsCfg)
	if err != nil {
		_ = stopWatch()
		_ = tcpRes.Conn.Close()
		if strings.Contains(strings.ToLower(err.Error()), "doesn't support starttls") {
			return nil, (&errmap.Error{
				Kind:       errmap.TLS,
				Message:    "SMTP 服务器不支持 STARTTLS，拒绝明文认证",
				Suggestion: "确认服务器端口支持 STARTTLS 或改用 465 隐式 TLS；CLI 禁止明文认证",
				Context:    map[string]any{"stage": StageSTARTTLS, "server": server, "endpoint": tcpRes.Endpoint, "transport": "starttls", "error_category": "starttls_unsupported"},
				Cause:      err,
			}).WithRetryable(false)
		}
		return nil, classifySMTPStage(tracker.stage(), err, server, tcpRes.Endpoint, true).WithContext(map[string]any{"transport": "starttls"})
	}
	client.CommandTimeout = boundDuration(hsCtx, cmdTimeout)
	// Trigger the post-STARTTLS TLS handshake and EHLO immediately so TLS
	// certificate or handshake failures are caught before Auth.
	if err := client.Hello("localhost"); err != nil {
		_ = stopWatch()
		state, ok := client.TLSConnectionState()
		_ = client.Close()
		if !ok || !state.HandshakeComplete {
			return nil, mapNetStageToSMTP(netx.ClassifyTLSError(err, server, tcpRes.Endpoint), "starttls")
		}
		return nil, classifySMTPStage(StageEHLO, err, server, tcpRes.Endpoint, true).WithContext(map[string]any{"transport": "starttls"})
	}
	if !stopWatch() {
		_ = client.Close()
		return nil, mapNetStageToSMTP(netx.ClassifyTLSError(hsCtx.Err(), server, tcpRes.Endpoint), "starttls")
	}
	if state, ok := client.TLSConnectionState(); !ok || !state.HandshakeComplete {
		_ = client.Close()
		return nil, (&errmap.Error{
			Kind:       errmap.TLS,
			Message:    "SMTP STARTTLS 升级未完成，拒绝明文认证",
			Suggestion: "检查服务器 STARTTLS 与证书配置",
			Context:    map[string]any{"stage": StageTLS, "server": server, "endpoint": tcpRes.Endpoint, "transport": "starttls", "error_category": netx.CategoryTLSProtocol},
		}).WithRetryable(false)
	}
	return &dialedSMTP{
		client:      client,
		conn:        tcpRes.Conn,
		tlsState:    client.TLSConnectionState,
		server:      server,
		endpoint:    tcpRes.Endpoint,
		transport:   "starttls",
		resolvedIPs: tcpRes.ResolvedIPs,
	}, nil
}

// smtpStageConn observes client writes to distinguish greeting, EHLO, and
// STARTTLS failures without re-implementing go-smtp's wire parser.
type smtpStageConn struct {
	net.Conn
	phase atomic.Int32 // 0 = greeting, 1 = ehlo, 2 = starttls
}

func (c *smtpStageConn) Write(p []byte) (int, error) {
	upper := strings.ToUpper(strings.TrimSpace(string(p)))
	switch {
	case strings.HasPrefix(upper, "STARTTLS"):
		c.phase.Store(2)
	case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
		c.phase.Store(1)
	}
	return c.Conn.Write(p)
}

func (c *smtpStageConn) stage() string {
	switch c.phase.Load() {
	case 2:
		return StageSTARTTLS
	case 1:
		return StageEHLO
	default:
		return StageGreeting
	}
}

func mapNetStageToSMTP(err error, transport string) *errmap.Error {
	app := errmap.Classify(err)
	extra := map[string]any{"transport": transport}
	if stage, ok := app.Context["stage"].(string); ok {
		switch stage {
		case netx.StageDNS:
			extra["stage"] = StageDNS
		case netx.StageTCP:
			extra["stage"] = StageTCP
		case netx.StageTLS:
			extra["stage"] = StageTLS
		}
	}
	return app.WithContext(extra)
}

func boundDuration(ctx context.Context, fallback time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if rem := time.Until(deadline); rem > 0 && rem < fallback {
			return rem
		}
	}
	return fallback
}

func classifySMTPStage(stage string, cause error, server, endpoint string, preAuth bool) *errmap.Error {
	base := errmap.Classify(classifySMTP(cause))
	ctx := map[string]any{"stage": stage}
	if server != "" {
		ctx["server"] = server
	}
	if endpoint != "" {
		ctx["endpoint"] = endpoint
	}
	var smtpErr *smtp.SMTPError
	if errors.As(cause, &smtpErr) {
		ctx["smtp_code"] = smtpErr.Code
		if base.Kind == errmap.RateLimited {
			ctx["error_category"] = "rate_limited"
		} else if base.Kind == errmap.AuthFailed {
			ctx["error_category"] = "auth_failed"
		} else if smtpErr.Code >= 500 {
			ctx["error_category"] = "smtp_permanent_error"
			base = base.WithRetryable(false)
		} else {
			ctx["error_category"] = "smtp_temporary_error"
			if !preAuth {
				base = base.WithRetryable(false)
			}
		}
	} else if base.Kind == errmap.Network {
		if netx.IsTimeoutError(cause) {
			ctx["error_category"] = stage + "_timeout"
		} else {
			ctx["error_category"] = stage + "_network"
		}
		if !preAuth {
			base = base.WithRetryable(false)
		}
	}
	return base.WithContext(ctx)
}

func classifySMTP(cause error) error {
	var smtpError *smtp.SMTPError
	if errors.As(cause, &smtpError) {
		message := strings.ToLower(smtpError.Message)
		if smtpError.Code == 421 || smtpError.Code == 450 || smtpError.Code == 451 || smtpError.Code == 452 || smtpError.Code == 454 || strings.Contains(message, "rate") || strings.Contains(message, "frequency") || strings.Contains(message, "too many") || strings.Contains(message, "limit") {
			return &errmap.Error{Kind: errmap.RateLimited, Message: "SMTP 发送被服务器限流", Suggestion: "停止重试并等待 10-15 分钟", Cause: cause}
		}
		if smtpError.Code == 530 || smtpError.Code == 534 || smtpError.Code == 535 {
			return &errmap.Error{Kind: errmap.AuthFailed, Message: "SMTP 认证失败", Suggestion: "确认 QQ 邮箱 SMTP 已开启且使用的是授权码", Cause: cause}
		}
		return (&errmap.Error{Kind: errmap.Network, Message: "SMTP 服务器拒绝发送请求", Cause: cause}).WithRetryable(false)
	}
	if netx.IsCertError(cause) || netx.IsTLSProtocolError(cause) {
		return (&errmap.Error{Kind: errmap.TLS, Message: "SMTP TLS 握手或证书校验失败", Suggestion: "检查系统时间、证书链或企业中间人代理", Cause: cause}).WithRetryable(false)
	}
	lower := strings.ToLower(cause.Error())
	if strings.Contains(lower, "x509") || strings.Contains(lower, "certificate") || (strings.Contains(lower, "tls") && !netx.IsTimeoutError(cause) && !netx.IsTransportError(cause)) {
		return (&errmap.Error{Kind: errmap.TLS, Message: "SMTP TLS 握手或证书校验失败", Suggestion: "检查系统时间、证书链或企业中间人代理", Cause: cause}).WithRetryable(false)
	}
	if strings.Contains(lower, "rate") || strings.Contains(lower, "too many") {
		return &errmap.Error{Kind: errmap.RateLimited, Message: "SMTP 发送被服务器限流", Suggestion: "停止重试并等待 10-15 分钟", Cause: cause}
	}
	return &errmap.Error{Kind: errmap.Network, Message: "SMTP 连接或发送失败", Cause: cause}
}
