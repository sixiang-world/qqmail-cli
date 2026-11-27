package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/output"
	keyring "github.com/zalando/go-keyring"
)

func withTestSeams(t *testing.T, goos string, env map[string]string, existing map[string]error, uid int) {
	t.Helper()
	origGOOS := currentGOOS
	origEnv := lookupEnv
	origStat := statPath
	origUID := currentUID
	origBackend := keyringBackendImpl
	origTimeout := keyringTimeout
	t.Cleanup(func() {
		currentGOOS = origGOOS
		lookupEnv = origEnv
		statPath = origStat
		currentUID = origUID
		keyringBackendImpl = origBackend
		keyringTimeout = origTimeout
	})
	currentGOOS = func() string { return goos }
	lookupEnv = func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
	statPath = func(p string) (os.FileInfo, error) {
		if err, ok := existing[p]; ok {
			return nil, err
		}
		return nil, fs.ErrNotExist
	}
	currentUID = func() int { return uid }
}

func TestDiagnoseEnvironmentLinux(t *testing.T) {
	t.Run("missing env and default socket", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{}, map[string]error{}, 1000)
		got := DiagnoseEnvironment()
		if got.Ready || got.Reason != ReasonNoSessionBus {
			t.Fatalf("got %+v, want not ready / no_session_bus", got)
		}
		if got.DBusSessionBusSet == nil || *got.DBusSessionBusSet {
			t.Fatalf("expected dbus_session_bus_set=false, got %+v", got.DBusSessionBusSet)
		}
		if got.DBusSocketExists == nil || *got.DBusSocketExists {
			t.Fatalf("expected dbus_socket_exists=false, got %+v", got.DBusSocketExists)
		}
		for _, want := range []string{"gnome-keyring", "libsecret-tools", "dbus-run-session", "--auth-code-env"} {
			if !strings.Contains(got.Suggestion, want) {
				t.Fatalf("suggestion missing %q: %s", want, got.Suggestion)
			}
		}
	})

	t.Run("configured unix socket missing", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{
			"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus",
		}, map[string]error{}, 1000)
		got := DiagnoseEnvironment()
		if got.Ready || got.Reason != ReasonNoSessionBus {
			t.Fatalf("got %+v, want not ready / no_session_bus", got)
		}
		if got.DBusSessionBusSet == nil || !*got.DBusSessionBusSet {
			t.Fatalf("expected dbus_session_bus_set=true")
		}
		if got.DBusSocketExists == nil || *got.DBusSocketExists {
			t.Fatalf("expected dbus_socket_exists=false")
		}
	})

	t.Run("configured unix socket exists", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{
			"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus,guid=1234",
		}, map[string]error{"/run/user/1000/bus": nil}, 1000)
		got := DiagnoseEnvironment()
		if !got.Ready || got.Reason != "" {
			t.Fatalf("got %+v, want ready", got)
		}
		if got.DBusSessionBusSet == nil || !*got.DBusSessionBusSet {
			t.Fatalf("expected dbus_session_bus_set=true")
		}
		if got.DBusSocketExists == nil || !*got.DBusSocketExists {
			t.Fatalf("expected dbus_socket_exists=true")
		}
	})

	t.Run("unset env falls back to default runtime socket", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{}, map[string]error{"/run/user/1000/bus": nil}, 1000)
		got := DiagnoseEnvironment()
		if !got.Ready {
			t.Fatalf("got %+v, want ready via default socket", got)
		}
		if got.DBusSessionBusSet == nil || *got.DBusSessionBusSet {
			t.Fatalf("expected dbus_session_bus_set=false")
		}
		if got.DBusSocketExists == nil || !*got.DBusSocketExists {
			t.Fatalf("expected dbus_socket_exists=true")
		}
	})

	t.Run("disabled address", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "disabled:"}, map[string]error{}, 1000)
		got := DiagnoseEnvironment()
		if got.Ready || got.Reason != ReasonNoSessionBus {
			t.Fatalf("got %+v, want no_session_bus", got)
		}
	})

	t.Run("socket permission denied", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{
			"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus",
		}, map[string]error{"/run/user/1000/bus": os.ErrPermission}, 1000)
		got := DiagnoseEnvironment()
		if got.Ready || got.Reason != ReasonPermissionDenied {
			t.Fatalf("got %+v, want permission_denied", got)
		}
	})
}

func TestDiagnoseEnvironmentNonLinux(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		wantReady bool
		wantWhy   Reason
	}{
		{"windows", true, ""},
		{"darwin", true, ""},
		{"plan9", false, ReasonServiceUnavailable},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			withTestSeams(t, tc.goos, map[string]string{}, map[string]error{}, -1)
			got := DiagnoseEnvironment()
			if got.Ready != tc.wantReady || got.Reason != tc.wantWhy {
				t.Fatalf("got %+v, want ready=%t reason=%q", got, tc.wantReady, tc.wantWhy)
			}
			if got.DBusSessionBusSet != nil {
				t.Fatalf("dbus_session_bus_set must be nil on %s", tc.goos)
			}
		})
	}
}

func TestClassifyKeyringErrorMatrix(t *testing.T) {
	cases := []struct {
		name     string
		goos     string
		env      map[string]string
		sockets  map[string]error
		err      error
		want     Reason
		wantDBus *bool
	}{
		{
			name:     "linux dbus-launch missing",
			goos:     "linux",
			env:      map[string]string{},
			err:      errors.New(`exec: "dbus-launch": executable file not found in $PATH`),
			want:     ReasonNoSessionBus,
			wantDBus: boolPtr(false),
		},
		{
			name:     "linux session bus address missing",
			goos:     "linux",
			env:      map[string]string{},
			err:      errors.New("dbus: couldn't determine address of session bus"),
			want:     ReasonNoSessionBus,
			wantDBus: boolPtr(false),
		},
		{
			name:     "linux generic error with unset bus falls back to no_session_bus",
			goos:     "linux",
			env:      map[string]string{},
			err:      errors.New("exit status 1"),
			want:     ReasonNoSessionBus,
			wantDBus: boolPtr(false),
		},
		{
			name:     "linux stale unix socket",
			goos:     "linux",
			env:      map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			err:      errors.New("dial unix /run/user/1000/bus: connect: no such file or directory"),
			want:     ReasonNoSessionBus,
			wantDBus: boolPtr(true),
		},
		{
			name:     "linux missing gnome-keyring secret service",
			goos:     "linux",
			env:      map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			sockets:  map[string]error{"/run/user/1000/bus": nil},
			err:      errors.New("The name org.freedesktop.secrets was not provided by any .service files"),
			want:     ReasonServiceUnavailable,
			wantDBus: boolPtr(true),
		},
		{
			name:     "linux locked login collection",
			goos:     "linux",
			env:      map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			sockets:  map[string]error{"/run/user/1000/bus": nil},
			err:      errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/login'"),
			want:     ReasonLocked,
			wantDBus: boolPtr(true),
		},
		{
			name:     "linux secret service is locked error",
			goos:     "linux",
			env:      map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			sockets:  map[string]error{"/run/user/1000/bus": nil},
			err:      errors.New("org.freedesktop.Secret.Error.IsLocked: Cannot create an item in a locked collection"),
			want:     ReasonLocked,
			wantDBus: boolPtr(true),
		},
		{
			name:     "linux dbus access denied",
			goos:     "linux",
			env:      map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			sockets:  map[string]error{"/run/user/1000/bus": nil},
			err:      errors.New("org.freedesktop.DBus.Error.AccessDenied: rejected"),
			want:     ReasonPermissionDenied,
			wantDBus: boolPtr(true),
		},
		{
			name:     "linux dbus timeout",
			goos:     "linux",
			env:      map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			sockets:  map[string]error{"/run/user/1000/bus": nil},
			err:      errors.New("org.freedesktop.DBus.Error.NoReply: Did not receive a reply"),
			want:     ReasonTimeout,
			wantDBus: boolPtr(true),
		},
		{
			name:     "linux context deadline exceeded",
			goos:     "linux",
			env:      map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			sockets:  map[string]error{"/run/user/1000/bus": nil},
			err:      context.DeadlineExceeded,
			want:     ReasonTimeout,
			wantDBus: boolPtr(true),
		},
		{
			name:     "linux unknown error with healthy bus",
			goos:     "linux",
			env:      map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			sockets:  map[string]error{"/run/user/1000/bus": nil},
			err:      errors.New("unexpected wire decode error"),
			want:     ReasonUnknown,
			wantDBus: boolPtr(true),
		},
		{
			name: "darwin locked keychain",
			goos: "darwin",
			err:  errors.New("security: SecKeychainSearchCopyNext: User interaction is not allowed."),
			want: ReasonLocked,
		},
		{
			name: "windows access denied",
			goos: "windows",
			err:  errors.New("Access is denied."),
			want: ReasonPermissionDenied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTestSeams(t, tc.goos, tc.env, tc.sockets, 1000)
			diag := ClassifyKeyringError(tc.err)
			if diag.Reason != tc.want {
				t.Fatalf("reason=%q, want %q (diag=%+v)", diag.Reason, tc.want, diag)
			}
			if diag.Platform != tc.goos || diag.CredentialStore != "keyring" {
				t.Fatalf("unexpected platform/store: %+v", diag)
			}
			if diag.Suggestion == "" {
				t.Fatalf("empty suggestion for %+v", diag)
			}
			ctx := diag.Context()
			if tc.wantDBus == nil {
				if _, ok := ctx["dbus_session_bus_set"]; ok {
					t.Fatalf("unexpected dbus_session_bus_set on %s", tc.goos)
				}
			} else {
				gotDBus, ok := ctx["dbus_session_bus_set"].(bool)
				if !ok || gotDBus != *tc.wantDBus {
					t.Fatalf("dbus_session_bus_set=%v (ok=%t), want %t", gotDBus, ok, *tc.wantDBus)
				}
			}
		})
	}
}

func TestClassifyKeyringErrorRedactsRegisteredSecrets(t *testing.T) {
	withTestSeams(t, "linux", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, map[string]error{"/run/user/1000/bus": nil}, 1000)
	const secret = "s3cr3tAuthCode99"
	output.RegisterSecret(secret)
	diag := ClassifyKeyringError(errors.New("keyring backend echoed " + secret))
	if strings.Contains(diag.Observed, secret) {
		t.Fatalf("registered secret leaked into Observed: %q", diag.Observed)
	}
	if !strings.Contains(diag.Observed, "***") {
		t.Fatalf("expected redacted marker in Observed: %q", diag.Observed)
	}
}

type fakeKeyringBackend struct {
	mu      sync.Mutex
	values  map[string]string
	getErr  error
	setErr  error
	delErr  error
	blockCh <-chan struct{}
	doneCh  chan struct{}
}

func (f *fakeKeyringBackend) Get(service, user string) (string, error) {
	if f.blockCh != nil {
		<-f.blockCh
		if f.doneCh != nil {
			close(f.doneCh)
		}
	}
	if f.getErr != nil {
		return "", f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[service+":"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyringBackend) Set(service, user, password string) error {
	if f.blockCh != nil {
		<-f.blockCh
		if f.doneCh != nil {
			close(f.doneCh)
		}
	}
	if f.setErr != nil {
		return f.setErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[service+":"+user] = password
	return nil
}

func (f *fakeKeyringBackend) Delete(service, user string) error {
	if f.blockCh != nil {
		<-f.blockCh
		if f.doneCh != nil {
			close(f.doneCh)
		}
	}
	if f.delErr != nil {
		return f.delErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.values[service+":"+user]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.values, service+":"+user)
	return nil
}

func TestKeyringGetSetDeleteSemanticsAndTimeout(t *testing.T) {
	t.Run("not found preserves AuthCodeMissing and Delete ignores ErrNotFound", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, map[string]error{"/run/user/1000/bus": nil}, 1000)
		keyringBackendImpl = &fakeKeyringBackend{}
		_, err := (Keyring{}).Get("user@qq.com")
		failure, code := errmap.Details(err)
		if failure.Code != string(errmap.AuthCodeMissing) || code != output.ExitAuth {
			t.Fatalf("got code=%s exit=%d, want auth_code_missing/%d", failure.Code, code, output.ExitAuth)
		}
		if err := (Keyring{}).Delete("user@qq.com"); err != nil {
			t.Fatalf("Delete on missing entry returned error: %v", err)
		}
	})

	t.Run("service unavailable returns Config with structured context", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, map[string]error{"/run/user/1000/bus": nil}, 1000)
		keyringBackendImpl = &fakeKeyringBackend{
			setErr: errors.New("The name org.freedesktop.secrets was not provided by any .service files"),
		}
		err := (Keyring{}).Set("user@qq.com", "ab12cd34ef56gh78")
		failure, code := errmap.Details(err)
		if failure.Code != string(errmap.Config) || code != output.ExitConfig {
			t.Fatalf("got code=%s exit=%d, want config/%d", failure.Code, code, output.ExitConfig)
		}
		if failure.Context["reason"] != string(ReasonServiceUnavailable) || failure.Context["credential_store"] != "keyring" || failure.Context["platform"] != "linux" {
			t.Fatalf("unexpected context: %+v", failure.Context)
		}
		if failure.Context["dbus_session_bus_set"] != true {
			t.Fatalf("expected dbus_session_bus_set=true, got %+v", failure.Context)
		}
	})

	t.Run("hanging call times out with reason timeout", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, map[string]error{"/run/user/1000/bus": nil}, 1000)
		unblock := make(chan struct{})
		done := make(chan struct{})
		t.Cleanup(func() {
			close(unblock)
			<-done
		})
		keyringBackendImpl = &fakeKeyringBackend{blockCh: unblock, doneCh: done}
		keyringTimeout = 40 * time.Millisecond

		start := time.Now()
		err := (Keyring{}).Set("user@qq.com", "ab12cd34ef56gh78")
		if time.Since(start) > 2*time.Second {
			t.Fatalf("keyring call did not respect timeout")
		}
		failure, code := errmap.Details(err)
		if failure.Code != string(errmap.Config) || code != output.ExitConfig {
			t.Fatalf("got code=%s exit=%d, want config/%d", failure.Code, code, output.ExitConfig)
		}
		if failure.Context["reason"] != string(ReasonTimeout) {
			t.Fatalf("reason=%v, want timeout", failure.Context["reason"])
		}
	})
}

func TestProbeKeyringLifecycle(t *testing.T) {
	t.Run("round trip succeeds and cleans up without leaking probe value", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, map[string]error{"/run/user/1000/bus": nil}, 1000)
		var captured string
		backend := &fakeKeyringBackend{values: map[string]string{ServiceName + ":real@qq.com": "real16charscode1"}}
		keyringBackendImpl = &captureBackend{inner: backend, onSet: func(_, user, pass string) {
			if user != ProbeAccount {
				t.Errorf("probe wrote unexpected account %q", user)
			}
			captured = pass
		}}
		res := ProbeKeyring(Keyring{})
		if !res.OK || !res.CleanupAttempted || !res.CleanupOK {
			t.Fatalf("expected clean pass, got %+v", res)
		}
		if res.Steps.Set.Status != "pass" || res.Steps.Get.Status != "pass" || res.Steps.Compare.Status != "pass" || res.Steps.Delete.Status != "pass" {
			t.Fatalf("unexpected step statuses: %+v", res.Steps)
		}
		if captured == "" {
			t.Fatal("probe did not write a value")
		}
		if _, left := backend.values[ServiceName+":"+ProbeAccount]; left {
			t.Fatal("probe entry was not deleted")
		}
		if backend.values[ServiceName+":real@qq.com"] != "real16charscode1" {
			t.Fatal("real credential was disturbed by probe")
		}
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), captured) || strings.Contains(string(raw), "real16charscode1") {
			t.Fatalf("probe or real secret leaked into ProbeResult JSON: %s", raw)
		}
	})

	t.Run("get failure still runs delete cleanup", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, map[string]error{"/run/user/1000/bus": nil}, 1000)
		backend := &fakeKeyringBackend{getErr: errors.New("org.freedesktop.Secret.Error.IsLocked")}
		keyringBackendImpl = backend
		res := ProbeKeyring(Keyring{})
		if res.OK || res.FailedStep != "get" {
			t.Fatalf("expected get failure, got %+v", res)
		}
		if res.Steps.Set.Status != "pass" || res.Steps.Get.Status != "fail" || res.Steps.Compare.Status != "skipped" || res.Steps.Delete.Status != "pass" {
			t.Fatalf("unexpected steps: %+v", res.Steps)
		}
		if !res.CleanupAttempted || !res.CleanupOK {
			t.Fatalf("expected cleanup to succeed after get failure: %+v", res)
		}
		if _, left := backend.values[ServiceName+":"+ProbeAccount]; left {
			t.Fatal("probe entry left behind after get failure")
		}
	})

	t.Run("compare mismatch runs cleanup and hides values", func(t *testing.T) {
		withTestSeams(t, "linux", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, map[string]error{"/run/user/1000/bus": nil}, 1000)
		backend := &fakeKeyringBackend{}
		keyringBackendImpl = &tamperGetBackend{inner: backend, returned: "tampered-probe-val"}
		res := ProbeKeyring(Keyring{})
		if res.OK || res.FailedStep != "compare" || !res.CleanupOK {
			t.Fatalf("expected compare failure with cleanup_ok=true, got %+v", res)
		}
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), "tampered-probe-val") {
			t.Fatalf("tampered value leaked into ProbeResult JSON: %s", raw)
		}
	})
}

type captureBackend struct {
	inner keyringBackend
	onSet func(service, user, pass string)
}

func (c *captureBackend) Get(service, user string) (string, error) {
	return c.inner.Get(service, user)
}
func (c *captureBackend) Set(service, user, pass string) error {
	if c.onSet != nil {
		c.onSet(service, user, pass)
	}
	return c.inner.Set(service, user, pass)
}
func (c *captureBackend) Delete(service, user string) error {
	return c.inner.Delete(service, user)
}

type tamperGetBackend struct {
	inner    keyringBackend
	returned string
}

func (t *tamperGetBackend) Get(_, _ string) (string, error) { return t.returned, nil }
func (t *tamperGetBackend) Set(s, u, p string) error        { return t.inner.Set(s, u, p) }
func (t *tamperGetBackend) Delete(s, u string) error        { return t.inner.Delete(s, u) }

func boolPtr(v bool) *bool { return &v }
