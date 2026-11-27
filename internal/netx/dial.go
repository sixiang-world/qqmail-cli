package netx

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/situker/qqmail-cli/internal/errmap"
)

const (
	DefaultDialTimeout = 10 * time.Second
	DefaultTLSTimeout  = 15 * time.Second
	MaxCandidateIPs    = 4
)

// Resolver abstracts DNS resolution for testing without bypassing normal OS
// resolution in production.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ConnectionInfo captures structured, credential-free connection telemetry.
type ConnectionInfo struct {
	Protocol     string   `json:"protocol,omitempty"`
	Attempts     int      `json:"attempts,omitempty"`
	Stage        string   `json:"stage,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Server       string   `json:"server,omitempty"`
	Transport    string   `json:"transport,omitempty"`
	FallbackUsed bool     `json:"fallback_used,omitempty"`
	ResolvedIPs  []string `json:"resolved_ips,omitempty"`
}

type connectionRecorderKey struct{}

// ConnectionRecorder collects the latest connection telemetry for a command
// invocation via context, avoiding changes to frozen protocol interfaces.
type ConnectionRecorder struct {
	mu   sync.Mutex
	info *ConnectionInfo
}

func WithConnectionRecorder(ctx context.Context, recorder *ConnectionRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, connectionRecorderKey{}, recorder)
}

func RecordConnection(ctx context.Context, info ConnectionInfo) {
	if ctx == nil {
		return
	}
	recorder, ok := ctx.Value(connectionRecorderKey{}).(*ConnectionRecorder)
	if !ok || recorder == nil {
		return
	}
	copyInfo := info
	if info.ResolvedIPs != nil {
		copyInfo.ResolvedIPs = append([]string(nil), info.ResolvedIPs...)
	}
	recorder.mu.Lock()
	recorder.info = &copyInfo
	recorder.mu.Unlock()
}

func (r *ConnectionRecorder) Latest() *ConnectionInfo {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.info == nil {
		return nil
	}
	copyInfo := *r.info
	if r.info.ResolvedIPs != nil {
		copyInfo.ResolvedIPs = append([]string(nil), r.info.ResolvedIPs...)
	}
	return &copyInfo
}

// DialConfig configures staged DNS -> TCP -> (optional) TLS dialing.
type DialConfig struct {
	Host        string
	Port        int
	TLSConfig   *tls.Config
	DialTimeout time.Duration
	TLSTimeout  time.Duration
	Resolver    Resolver
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

type TCPResult struct {
	Conn           net.Conn
	Server         string
	Endpoint       string
	ResolvedIPs    []string
	TriedEndpoints []string
	DNSDuration    time.Duration
	TCPDuration    time.Duration
}

type TLSResult struct {
	Conn           *tls.Conn
	Server         string
	Endpoint       string
	ResolvedIPs    []string
	TriedEndpoints []string
	DNSDuration    time.Duration
	TCPDuration    time.Duration
	TLSDuration    time.Duration
}

func (c DialConfig) serverAddr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func (c DialConfig) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return DefaultDialTimeout
}

func (c DialConfig) tlsTimeout() time.Duration {
	if c.TLSTimeout > 0 {
		return c.TLSTimeout
	}
	return DefaultTLSTimeout
}

func (c DialConfig) resolver() Resolver {
	if c.Resolver != nil {
		return c.Resolver
	}
	return net.DefaultResolver
}

func (c DialConfig) dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	if c.DialContext != nil {
		return c.DialContext(ctx, "tcp", addr)
	}
	dialer := &net.Dialer{Timeout: c.dialTimeout()}
	return dialer.DialContext(ctx, "tcp", addr)
}

// StrictTLSConfig returns a cloned TLS config with certificate verification
// enforced, ServerName set to host, and MinVersion >= TLS 1.2.
func StrictTLSConfig(host string, override *tls.Config) *tls.Config {
	var cfg *tls.Config
	if override == nil {
		cfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	} else {
		cfg = override.Clone()
		cfg.InsecureSkipVerify = false
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
		if cfg.MinVersion < tls.VersionTLS12 {
			cfg.MinVersion = tls.VersionTLS12
		}
	}
	return cfg
}

// ResolveHost resolves host via normal DNS (or returns the IP literal directly),
// preserving OS resolver ordering and capping candidate count.
func ResolveHost(ctx context.Context, resolver Resolver, host string, port int, timeout time.Duration) ([]string, time.Duration, error) {
	server := net.JoinHostPort(host, strconv.Itoa(port))
	if ip := net.ParseIP(host); ip != nil {
		return []string{host}, 0, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	dnsCtx, cancel := withBoundedTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	addrs, err := resolver.LookupIPAddr(dnsCtx, host)
	elapsed := time.Since(start)
	if err != nil {
		return nil, elapsed, ClassifyDNSError(err, server)
	}
	seen := make(map[string]struct{}, len(addrs))
	ips := make([]string, 0, min(len(addrs), MaxCandidateIPs))
	for _, addr := range addrs {
		ip := addr.String()
		if ip == "" {
			continue
		}
		if _, dup := seen[ip]; dup {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
		if len(ips) >= MaxCandidateIPs {
			break
		}
	}
	if len(ips) == 0 {
		return nil, elapsed, ClassifyDNSError(&net.DNSError{Err: "no such host", Name: host, IsNotFound: true}, server)
	}
	return ips, elapsed, nil
}

// DialTCP resolves the hostname and connects over TCP with multi-IP failover.
func DialTCP(ctx context.Context, cfg DialConfig) (*TCPResult, error) {
	server := cfg.serverAddr()
	ips, dnsDur, err := ResolveHost(ctx, cfg.resolver(), cfg.Host, cfg.Port, cfg.dialTimeout())
	if err != nil {
		return nil, err
	}
	var (
		lastErr *errmap.Error
		tried   []string
	)
	for i, ip := range ips {
		if ctx.Err() != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, ClassifyTCPError(ctx.Err(), server, net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
		}
		endpoint := net.JoinHostPort(ip, strconv.Itoa(cfg.Port))
		tried = append(tried, endpoint)
		budget := perCandidateBudget(ctx, cfg.dialTimeout(), len(ips)-i)
		dialCtx, cancel := withBoundedTimeout(ctx, budget)
		start := time.Now()
		conn, dialErr := cfg.dialTCP(dialCtx, endpoint)
		elapsed := time.Since(start)
		cancel()
		if dialErr == nil {
			return &TCPResult{
				Conn:           conn,
				Server:         server,
				Endpoint:       endpoint,
				ResolvedIPs:    ips,
				TriedEndpoints: tried,
				DNSDuration:    dnsDur,
				TCPDuration:    elapsed,
			}, nil
		}
		lastErr = ClassifyTCPError(dialErr, server, endpoint)
		if !IsRetryable(lastErr) {
			return nil, lastErr
		}
	}
	if lastErr == nil {
		lastErr = ClassifyTCPError(fmt.Errorf("no candidate addresses for %s", server), server, "")
	}
	return nil, lastErr
}

// DialTLS resolves the hostname, dials TCP, and performs a strict TLS handshake
// with multi-IP failover on transient TCP/TLS network failures. Certificate and
// TLS protocol errors fail immediately without trying additional IPs.
func DialTLS(ctx context.Context, cfg DialConfig) (*TLSResult, error) {
	server := cfg.serverAddr()
	ips, dnsDur, err := ResolveHost(ctx, cfg.resolver(), cfg.Host, cfg.Port, cfg.dialTimeout())
	if err != nil {
		return nil, err
	}
	tlsCfg := StrictTLSConfig(cfg.Host, cfg.TLSConfig)
	var (
		lastErr *errmap.Error
		tried   []string
	)
	for i, ip := range ips {
		if ctx.Err() != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, ClassifyTCPError(ctx.Err(), server, net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
		}
		endpoint := net.JoinHostPort(ip, strconv.Itoa(cfg.Port))
		tried = append(tried, endpoint)
		remainingCandidates := len(ips) - i
		tcpBudget := perCandidateBudget(ctx, cfg.dialTimeout(), remainingCandidates)
		tcpCtx, cancelTCP := withBoundedTimeout(ctx, tcpBudget)
		tcpStart := time.Now()
		plain, dialErr := cfg.dialTCP(tcpCtx, endpoint)
		tcpDur := time.Since(tcpStart)
		cancelTCP()
		if dialErr != nil {
			lastErr = ClassifyTCPError(dialErr, server, endpoint)
			if !IsRetryable(lastErr) {
				return nil, lastErr
			}
			continue
		}

		tlsBudget := perCandidateBudget(ctx, cfg.tlsTimeout(), remainingCandidates)
		tlsCtx, cancelTLS := withBoundedTimeout(ctx, tlsBudget)
		if deadline, ok := tlsCtx.Deadline(); ok {
			_ = plain.SetDeadline(deadline)
		}
		tlsStart := time.Now()
		tlsConn := tls.Client(plain, tlsCfg)
		hsErr := tlsConn.HandshakeContext(tlsCtx)
		tlsDur := time.Since(tlsStart)
		cancelTLS()
		if hsErr != nil {
			_ = plain.Close()
			lastErr = ClassifyTLSError(hsErr, server, endpoint)
			// Certificate and TLS protocol errors are never failed over across IPs.
			if !IsRetryable(lastErr) {
				return nil, lastErr
			}
			continue
		}
		_ = plain.SetDeadline(time.Time{})
		return &TLSResult{
			Conn:           tlsConn,
			Server:         server,
			Endpoint:       endpoint,
			ResolvedIPs:    ips,
			TriedEndpoints: tried,
			DNSDuration:    dnsDur,
			TCPDuration:    tcpDur,
			TLSDuration:    tlsDur,
		}, nil
	}
	if lastErr == nil {
		lastErr = ClassifyTCPError(fmt.Errorf("no candidate addresses for %s", server), server, "")
	}
	return nil, lastErr
}

func withBoundedTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.WithCancel(ctx)
		}
		if remaining < timeout {
			return context.WithDeadline(ctx, deadline)
		}
	}
	return context.WithTimeout(ctx, timeout)
}

// perCandidateBudget divides remaining context deadline fairly across remaining
// resolved IPs so one stalled address cannot consume the entire budget.
func perCandidateBudget(ctx context.Context, capTimeout time.Duration, remainingCandidates int) time.Duration {
	if remainingCandidates <= 1 {
		if deadline, ok := ctx.Deadline(); ok {
			if rem := time.Until(deadline); rem > 0 && rem < capTimeout {
				return rem
			}
		}
		return capTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		rem := time.Until(deadline)
		if rem <= 0 {
			return 0
		}
		share := rem / time.Duration(remainingCandidates)
		if share < capTimeout {
			return share
		}
	}
	return capTimeout
}
