package netx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/situker/qqmail-cli/internal/errmap"
)

const (
	StageDNS = "dns"
	StageTCP = "tcp"
	StageTLS = "tls"
)

const (
	CategoryDNSTimeout       = "dns_timeout"
	CategoryDNSNotFound      = "dns_not_found"
	CategoryDNSError         = "dns_error"
	CategoryTCPTimeout       = "tcp_timeout"
	CategoryTCPRefused       = "tcp_refused"
	CategoryTCPNetwork       = "tcp_network"
	CategoryTLSCertificate   = "tls_certificate"
	CategoryTLSProtocol      = "tls_protocol"
	CategoryTLSTimeout       = "tls_timeout"
	CategoryTLSNetwork       = "tls_network"
	CategoryCanceled         = "canceled"
	CategoryDeadlineExceeded = "deadline_exceeded"
)

// IsCertError reports whether err is a TLS certificate verification failure.
// Certificate verification failures must never be retried or bypassed.
func IsCertError(err error) bool {
	if err == nil {
		return false
	}
	var (
		verifyErr  *tls.CertificateVerificationError
		unknownCA  x509.UnknownAuthorityError
		invalid    x509.CertificateInvalidError
		hostname   x509.HostnameError
		sysRoots   x509.SystemRootsError
		constraint x509.ConstraintViolationError
		insecure   x509.InsecureAlgorithmError
	)
	return errors.As(err, &verifyErr) ||
		errors.As(err, &unknownCA) ||
		errors.As(err, &invalid) ||
		errors.As(err, &hostname) ||
		errors.As(err, &sysRoots) ||
		errors.As(err, &constraint) ||
		errors.As(err, &insecure)
}

// IsTLSProtocolError reports whether err is a non-transient TLS protocol error
// such as a TLS alert or non-TLS record header.
func IsTLSProtocolError(err error) bool {
	if err == nil {
		return false
	}
	var (
		recordErr tls.RecordHeaderError
		alertErr  tls.AlertError
	)
	return errors.As(err, &recordErr) || errors.As(err, &alertErr)
}

// IsTimeoutError reports whether err represents a timeout or deadline expiration.
func IsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// IsTransportError reports whether err is an underlying network transport drop
// or timeout rather than an application/TLS verification rejection.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func isConnectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "connection refused") || strings.Contains(lower, "actively refused")
}

// ClassifyDNSError classifies a DNS resolution error into a structured errmap.Error.
func ClassifyDNSError(err error, server string) *errmap.Error {
	ctx := map[string]any{"stage": StageDNS, "server": server}
	if errors.Is(err, context.Canceled) {
		ctx["error_category"] = CategoryCanceled
		return (&errmap.Error{
			Kind: errmap.Network, Message: "DNS 解析已取消", Context: ctx, Cause: err,
		}).WithRetryable(false)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound && !dnsErr.IsTimeout && !dnsErr.IsTemporary {
			ctx["error_category"] = CategoryDNSNotFound
			return (&errmap.Error{
				Kind:       errmap.Network,
				Message:    "无法解析服务器域名（域名不存在）",
				Suggestion: "检查账号配置中的服务器主机名与 DNS 设置",
				Context:    ctx,
				Cause:      err,
			}).WithRetryable(false)
		}
		if dnsErr.IsTimeout {
			ctx["error_category"] = CategoryDNSTimeout
			return (&errmap.Error{
				Kind:       errmap.Network,
				Message:    "DNS 解析超时",
				Suggestion: "检查网络连通性或 DNS 解析服务后重试",
				Context:    ctx,
				Cause:      err,
			}).WithRetryable(true)
		}
		ctx["error_category"] = CategoryDNSError
		return (&errmap.Error{
			Kind:       errmap.Network,
			Message:    "DNS 解析失败",
			Suggestion: "检查网络与 DNS 配置后重试；云端环境请确认目标域名在受支持的网络授权范围内",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(true)
	}
	if IsTimeoutError(err) {
		ctx["error_category"] = CategoryDNSTimeout
		return (&errmap.Error{
			Kind:       errmap.Network,
			Message:    "DNS 解析超时",
			Suggestion: "检查网络连通性或 DNS 解析服务后重试",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(true)
	}
	ctx["error_category"] = CategoryDNSError
	return (&errmap.Error{
		Kind:       errmap.Network,
		Message:    "DNS 解析失败",
		Suggestion: "检查网络与 DNS 配置后重试",
		Context:    ctx,
		Cause:      err,
	}).WithRetryable(true)
}

// ClassifyTCPError classifies a TCP dial error into a structured errmap.Error.
func ClassifyTCPError(err error, server, endpoint string) *errmap.Error {
	ctx := map[string]any{"stage": StageTCP, "server": server}
	if endpoint != "" {
		ctx["endpoint"] = endpoint
	}
	if errors.Is(err, context.Canceled) {
		ctx["error_category"] = CategoryCanceled
		return (&errmap.Error{
			Kind: errmap.Network, Message: "TCP 连接已取消", Context: ctx, Cause: err,
		}).WithRetryable(false)
	}
	if IsTimeoutError(err) {
		ctx["error_category"] = CategoryTCPTimeout
		return (&errmap.Error{
			Kind:       errmap.Network,
			Message:    "TCP 连接超时",
			Suggestion: "检查出站网络连通性与端口放行策略后重试；可用 doctor 排查阶段故障",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(true)
	}
	if isConnectionRefused(err) {
		ctx["error_category"] = CategoryTCPRefused
		return (&errmap.Error{
			Kind:       errmap.Network,
			Message:    "TCP 连接被拒绝",
			Suggestion: "确认服务器地址与端口正确且网络策略允许访问",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(true)
	}
	ctx["error_category"] = CategoryTCPNetwork
	return (&errmap.Error{
		Kind:       errmap.Network,
		Message:    "无法建立 TCP 连接",
		Suggestion: "检查网络连接后重试；可用 doctor 排查阶段故障",
		Context:    ctx,
		Cause:      err,
	}).WithRetryable(true)
}

// ClassifyTLSError separates certificate/protocol verification failures (exit 21,
// non-retryable) from transient transport drops/timeouts during TLS handshake
// (exit 20, retryable before authentication).
func ClassifyTLSError(err error, server, endpoint string) *errmap.Error {
	ctx := map[string]any{"stage": StageTLS, "server": server}
	if endpoint != "" {
		ctx["endpoint"] = endpoint
	}
	if IsCertError(err) {
		ctx["error_category"] = CategoryTLSCertificate
		return (&errmap.Error{
			Kind:       errmap.TLS,
			Message:    "TLS 证书校验失败",
			Suggestion: "检查系统时间、CA 证书链或企业中间人代理；CLI 不会跳过证书校验",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(false)
	}
	if IsTLSProtocolError(err) {
		ctx["error_category"] = CategoryTLSProtocol
		return (&errmap.Error{
			Kind:       errmap.TLS,
			Message:    "TLS 协议协商失败",
			Suggestion: "确认目标端口支持 TLS 1.2+ 且未被代理或防火墙篡改",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(false)
	}
	if errors.Is(err, context.Canceled) {
		ctx["error_category"] = CategoryCanceled
		return (&errmap.Error{
			Kind: errmap.Network, Message: "TLS 握手已取消", Context: ctx, Cause: err,
		}).WithRetryable(false)
	}
	if IsTimeoutError(err) {
		ctx["error_category"] = CategoryTLSTimeout
		return (&errmap.Error{
			Kind:       errmap.Network,
			Message:    "TLS 握手超时",
			Suggestion: "检查网络连通性后重试；可用 doctor 排查 DNS/TCP/TLS 阶段故障",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(true)
	}
	if IsTransportError(err) {
		ctx["error_category"] = CategoryTLSNetwork
		return (&errmap.Error{
			Kind:       errmap.Network,
			Message:    "TLS 握手期间连接中断",
			Suggestion: "检查网络连通性后重试；可用 doctor 排查阶段故障",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(true)
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "x509:") || strings.Contains(lower, "certificate") {
		ctx["error_category"] = CategoryTLSCertificate
		return (&errmap.Error{
			Kind:       errmap.TLS,
			Message:    "TLS 证书校验失败",
			Suggestion: "检查系统时间、CA 证书链或企业中间人代理；CLI 不会跳过证书校验",
			Context:    ctx,
			Cause:      err,
		}).WithRetryable(false)
	}
	ctx["error_category"] = CategoryTLSProtocol
	return (&errmap.Error{
		Kind:       errmap.TLS,
		Message:    "TLS 握手或证书校验失败",
		Suggestion: "检查系统时间、证书链或企业中间人代理",
		Context:    ctx,
		Cause:      err,
	}).WithRetryable(false)
}

// IsRetryable reports whether an error is eligible for bounded automatic retry.
// Auth failures, TLS certificate/protocol errors, rate limits, uncertain
// deliveries, policy denials, and explicit cancellations are never retried.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if IsCertError(err) || IsTLSProtocolError(err) {
		return false
	}
	app := errmap.Classify(err)
	switch app.Kind {
	case errmap.Network:
		if app.Retryable != nil {
			return *app.Retryable
		}
		return true
	default:
		return false
	}
}
