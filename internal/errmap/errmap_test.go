package errmap

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"

	"github.com/situker/qqmail-cli/internal/output"
)

func TestExitCodeTable(t *testing.T) {
	cases := []struct {
		kind Kind
		code int
	}{
		{Usage, output.ExitUsage}, {Config, output.ExitConfig}, {AuthFailed, output.ExitAuth},
		{ServiceNotEnabled, output.ExitServiceNotEnabled}, {Network, output.ExitNetwork},
		{TLS, output.ExitTLS}, {RateLimited, output.ExitRateLimited}, {NotFound, output.ExitNotFound},
		{PolicyDenied, output.ExitPolicyDenied}, {ParseError, output.ExitParse}, {Internal, output.ExitInternal},
	}
	for _, tc := range cases {
		_, got := Details(&Error{Kind: tc.kind, Message: "x", Cause: errors.New("fixture")})
		if got != tc.code {
			t.Errorf("%s: got %d want %d", tc.kind, got, tc.code)
		}
	}
}

func TestRetryableOverrideAndTLSClassification(t *testing.T) {
	err := (&Error{Kind: Network, Message: "permanent rejection"}).WithRetryable(false)
	detail, code := Details(err)
	if code != output.ExitNetwork || detail.Retryable {
		t.Fatalf("expected exit 20 non-retryable, got code=%d detail=%+v", code, detail)
	}
	for _, tlsErr := range []error{
		&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
		x509.HostnameError{Host: "imap.qq.com"},
		x509.CertificateInvalidError{Reason: x509.Expired},
	} {
		d, c := Details(tlsErr)
		if c != output.ExitTLS || d.Code != string(TLS) || d.Retryable {
			t.Fatalf("expected non-retryable TLS exit 21 for %T, got code=%d detail=%+v", tlsErr, c, d)
		}
	}
}
