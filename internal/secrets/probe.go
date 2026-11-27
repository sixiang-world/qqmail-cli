package secrets

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/output"
)

// ProbeAccount is a fixed, non-email account identifier used exclusively by
// `auth keyring-test` so self-tests never collide with real mailbox entries.
const ProbeAccount = "__qqmail-cli-keyring-probe__"

// ProbeStep records the outcome and timing of one keyring self-test step.
type ProbeStep struct {
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Reason     Reason `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// ProbeSteps holds per-step outcomes for a keyring round-trip self-test.
type ProbeSteps struct {
	Set     ProbeStep `json:"set"`
	Get     ProbeStep `json:"get"`
	Compare ProbeStep `json:"compare"`
	Delete  ProbeStep `json:"delete"`
}

// ProbeResult captures the full outcome of ProbeKeyring without ever storing
// or exposing the ephemeral probe value.
type ProbeResult struct {
	OK               bool             `json:"ok"`
	CredentialStore  string           `json:"credential_store"`
	Service          string           `json:"service"`
	ProbeAccount     string           `json:"probe_account"`
	Platform         string           `json:"platform"`
	Environment      EnvironmentCheck `json:"environment"`
	Steps            ProbeSteps       `json:"steps"`
	CleanupAttempted bool             `json:"cleanup_attempted"`
	CleanupOK        bool             `json:"cleanup_ok"`
	FailedStep       string           `json:"failed_step,omitempty"`
	Diagnosis        *Diagnosis       `json:"-"`
	FailureErr       error            `json:"-"`
}

// ProbeKeyring executes an explicit, cleanable write/read/compare/delete test
// against the given Provider using a random probe value under ProbeAccount.
// It never reads or modifies real account credentials and never contacts the network.
func ProbeKeyring(provider Provider) ProbeResult {
	if provider == nil {
		provider = Keyring{}
	}
	env := DiagnoseEnvironment()
	store := provider.Source()
	if store == "" {
		store = "keyring"
	}
	res := ProbeResult{
		CredentialStore: store,
		Service:         ServiceName,
		ProbeAccount:    ProbeAccount,
		Platform:        env.Platform,
		Environment:     env,
		Steps: ProbeSteps{
			Set:     ProbeStep{Status: "skipped"},
			Get:     ProbeStep{Status: "skipped"},
			Compare: ProbeStep{Status: "skipped"},
			Delete:  ProbeStep{Status: "skipped"},
		},
	}

	probeValue, err := generateProbeValue()
	if err != nil {
		diag := Diagnosis{
			CredentialStore:   store,
			Reason:            ReasonUnknown,
			Platform:          env.Platform,
			Observed:          sanitizeObserved(err.Error()),
			Suggestion:        SuggestionFor(env.Platform, ReasonUnknown),
			DBusSessionBusSet: env.DBusSessionBusSet,
		}
		res.Steps.Set = ProbeStep{Status: "fail", Reason: diag.Reason, Detail: diag.Observed}
		res.FailedStep = "set"
		res.CleanupOK = true
		res.Diagnosis = &diag
		res.FailureErr = err
		return res
	}
	output.RegisterSecret(probeValue)

	start := time.Now()
	err = provider.Set(ProbeAccount, probeValue)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		diag := ClassifyKeyringError(err)
		if diag.CredentialStore == "" {
			diag.CredentialStore = store
		}
		res.Steps.Set = ProbeStep{Status: "fail", DurationMS: dur, Reason: diag.Reason, Detail: diag.Observed}
		res.FailedStep = "set"
		res.CleanupAttempted = false
		res.CleanupOK = diag.Reason != ReasonTimeout
		res.Diagnosis = &diag
		res.FailureErr = err
		return res
	}
	res.Steps.Set = ProbeStep{Status: "pass", DurationMS: dur}

	runCleanup := func() (Diagnosis, error) {
		res.CleanupAttempted = true
		delStart := time.Now()
		delErr := provider.Delete(ProbeAccount)
		delDur := time.Since(delStart).Milliseconds()
		if delErr != nil {
			delDiag := ClassifyKeyringError(delErr)
			if delDiag.CredentialStore == "" {
				delDiag.CredentialStore = store
			}
			res.Steps.Delete = ProbeStep{Status: "fail", DurationMS: delDur, Reason: delDiag.Reason, Detail: delDiag.Observed}
			res.CleanupOK = false
			return delDiag, delErr
		}
		res.Steps.Delete = ProbeStep{Status: "pass", DurationMS: delDur}
		res.CleanupOK = true
		return Diagnosis{}, nil
	}

	start = time.Now()
	got, err := provider.Get(ProbeAccount)
	dur = time.Since(start).Milliseconds()
	if err != nil {
		var diag Diagnosis
		if errmap.Classify(err).Kind == errmap.AuthCodeMissing {
			diag = Diagnosis{
				CredentialStore:   store,
				Reason:            ReasonUnknown,
				Platform:          env.Platform,
				Observed:          "写入探针后未能从凭据管理器读回该项",
				Suggestion:        SuggestionFor(env.Platform, ReasonUnknown),
				DBusSessionBusSet: env.DBusSessionBusSet,
			}
		} else {
			diag = ClassifyKeyringError(err)
			if diag.CredentialStore == "" {
				diag.CredentialStore = store
			}
		}
		res.Steps.Get = ProbeStep{Status: "fail", DurationMS: dur, Reason: diag.Reason, Detail: diag.Observed}
		res.FailedStep = "get"
		res.Diagnosis = &diag
		res.FailureErr = err
		_, _ = runCleanup()
		return res
	}
	output.RegisterSecret(got)
	res.Steps.Get = ProbeStep{Status: "pass", DurationMS: dur}

	start = time.Now()
	matched := subtle.ConstantTimeCompare([]byte(got), []byte(probeValue)) == 1
	dur = time.Since(start).Milliseconds()
	if !matched {
		diag := Diagnosis{
			CredentialStore:   store,
			Reason:            ReasonUnknown,
			Platform:          env.Platform,
			Observed:          "读回的探针值与写入值不一致",
			Suggestion:        SuggestionFor(env.Platform, ReasonUnknown),
			DBusSessionBusSet: env.DBusSessionBusSet,
		}
		res.Steps.Compare = ProbeStep{Status: "fail", DurationMS: dur, Reason: diag.Reason, Detail: diag.Observed}
		res.FailedStep = "compare"
		res.Diagnosis = &diag
		res.FailureErr = errors.New("keyring probe value mismatch")
		_, _ = runCleanup()
		return res
	}
	res.Steps.Compare = ProbeStep{Status: "pass", DurationMS: dur}

	if delDiag, delErr := runCleanup(); delErr != nil {
		res.FailedStep = "delete"
		res.Diagnosis = &delDiag
		res.FailureErr = delErr
		return res
	}

	res.OK = true
	return res
}

func generateProbeValue() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate probe token: %w", err)
	}
	return "qqmail-cli-probe-" + hex.EncodeToString(buf), nil
}
