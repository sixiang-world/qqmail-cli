package secrets

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/output"
	keyring "github.com/zalando/go-keyring"
)

const (
	ServiceName           = "qqmail-cli"
	EnvName               = "QQMAIL_CLI_AUTH_CODE"
	DefaultKeyringTimeout = 15 * time.Second
)

type keyringBackend interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyringBackend struct{}

func (osKeyringBackend) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyringBackend) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (osKeyringBackend) Delete(service, user string) error { return keyring.Delete(service, user) }

var (
	keyringBackendImpl keyringBackend = osKeyringBackend{}
	keyringTimeout                    = DefaultKeyringTimeout
)

func callWithTimeout[T any](timeout time.Duration, fn func() (T, error)) (T, error) {
	var zero T
	if timeout <= 0 {
		return fn()
	}
	type result struct {
		value T
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{value: v, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.value, res.err
	case <-timer.C:
		return zero, fmt.Errorf("%w after %s", errKeyringTimeout, timeout)
	}
}

type Provider interface {
	Get(account string) (string, error)
	Set(account, code string) error
	Delete(account string) error
	Source() string
}

type Keyring struct{}

func (Keyring) Get(account string) (string, error) {
	backend := keyringBackendImpl
	value, err := callWithTimeout(keyringTimeout, func() (string, error) {
		return backend.Get(ServiceName, account)
	})
	if errors.Is(err, keyring.ErrNotFound) {
		return "", &errmap.Error{Kind: errmap.AuthCodeMissing, Message: "系统凭据管理器中没有此账号的授权码", Suggestion: "重新运行 qqmail-cli auth login"}
	}
	if err != nil {
		diag := ClassifyKeyringError(err)
		return "", &errmap.Error{
			Kind:       errmap.Config,
			Message:    keyringErrorMessage("无法访问系统凭据管理器", diag.Reason),
			Suggestion: diag.Suggestion,
			Context:    diag.Context(),
			Cause:      err,
		}
	}
	output.RegisterSecret(value)
	return value, nil
}

func (Keyring) Set(account, code string) error {
	output.RegisterSecret(code)
	backend := keyringBackendImpl
	_, err := callWithTimeout(keyringTimeout, func() (struct{}, error) {
		return struct{}{}, backend.Set(ServiceName, account, code)
	})
	if err != nil {
		diag := ClassifyKeyringError(err)
		return &errmap.Error{
			Kind:       errmap.Config,
			Message:    keyringErrorMessage("无法写入系统凭据管理器", diag.Reason),
			Suggestion: diag.Suggestion,
			Context:    diag.Context(),
			Cause:      err,
		}
	}
	return nil
}

func (Keyring) Delete(account string) error {
	backend := keyringBackendImpl
	_, err := callWithTimeout(keyringTimeout, func() (struct{}, error) {
		return struct{}{}, backend.Delete(ServiceName, account)
	})
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		diag := ClassifyKeyringError(err)
		return &errmap.Error{
			Kind:       errmap.Config,
			Message:    keyringErrorMessage("无法删除系统凭据", diag.Reason),
			Suggestion: diag.Suggestion,
			Context:    diag.Context(),
			Cause:      err,
		}
	}
	return nil
}

func (Keyring) Source() string { return "keyring" }

type Environment struct{}

func (Environment) Get(_ string) (string, error) {
	value := strings.TrimSpace(os.Getenv(EnvName))
	if value == "" {
		return "", &errmap.Error{Kind: errmap.AuthCodeMissing, Message: EnvName + " 未设置"}
	}
	output.RegisterSecret(value)
	return value, nil
}

func (Environment) Set(_, _ string) error {
	return &errmap.Error{Kind: errmap.PolicyDenied, Message: "环境变量 provider 不允许持久化凭证"}
}

func (Environment) Delete(_ string) error { return nil }
func (Environment) Source() string        { return "env" }

type Memory struct{ Values map[string]string }

func (m *Memory) Get(account string) (string, error) {
	value, ok := m.Values[account]
	if !ok {
		return "", &errmap.Error{Kind: errmap.AuthCodeMissing, Message: "missing test secret"}
	}
	output.RegisterSecret(value)
	return value, nil
}
func (m *Memory) Set(account, code string) error {
	if m.Values == nil {
		m.Values = make(map[string]string)
	}
	m.Values[account] = code
	output.RegisterSecret(code)
	return nil
}
func (m *Memory) Delete(account string) error { delete(m.Values, account); return nil }
func (m *Memory) Source() string              { return "memory" }
