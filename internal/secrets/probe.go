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

// ProbeAccount is the non-email prefix used exclusively by `auth keyring-test`.
// Each invocation appends a random suffix so concurrent self-tests or late
// timed-out writes never collide with each other or with real mailbox entries.
const ProbeAccount = "__qqmail-cli-keyring-probe__"

const (
	CleanupStatusCleaned   = "cleaned"
	CleanupStatusNotNeeded = "not_needed"
	CleanupStatusFailed    = "failed"
	CleanupStatusUncertain = "uncertain"
)

// ProbeStep records the outcome and timing of one keyring self-test step.
type ProbeStep struct {
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Outcome    string `json:"outcome,omitempty"`
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
	CleanupStatus    string           `json:"cleanup_status"`
	OutcomeUncertain bool             `json:"outcome_uncertain"`
	FailedStep       string           `json:"failed_step,omitempty"`
	Diagnosis        *Diagnosis       `json:"-"`
	FailureErr       error            `json:"-"`
}

// ProbeKeyring executes an explicit, cleanable write/read/compare/delete test
// against the given Provider using a unique non-email probe account and random
// probe value. It never reads or modifies real account credentials and never
// contacts the network.
func ProbeKeyring(provider Provider) ProbeResult {
	if provider == nil {
		provider = Keyring{}
	}
	env := DiagnoseEnvironment()
	store := provider.Source()
	if store == "" {
		store = "keyring"
	}
	probeAccount, acctErr := generateProbeAccount()
	if acctErr != nil {
		probeAccount = ProbeAccount
	}
	res := ProbeResult{
		CredentialStore: store,
		Service:         ServiceName,
		ProbeAccount:    probeAccount,
		Platform:        env.Platform,
		Environment:     env,
		CleanupStatus:   CleanupStatusNotNeeded,
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
		res.Steps.Set = ProbeStep{Status: "fail", Outcome: OutcomeFailed, Reason: diag.Reason, Detail: diag.Observed}
		res.FailedStep = "set"
		res.CleanupOK = true
		res.CleanupStatus = CleanupStatusNotNeeded
		res.Diagnosis = &diag
		res.FailureErr = err
		return res
	}
	output.RegisterSecret(probeValue)

	start := time.Now()
	err = provider.Set(probeAccount, probeValue)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		diag := ClassifyKeyringError(err)
		if diag.CredentialStore == "" {
			diag.CredentialStore = store
		}
		uncertain := IsTimeout(err) || diag.Reason == ReasonTimeout
		outcome := OutcomeFailed
		if uncertain {
			outcome = OutcomeUncertain
		}
		res.Steps.Set = ProbeStep{Status: "fail", Outcome: outcome, DurationMS: dur, Reason: diag.Reason, Detail: diag.Observed}
		res.FailedStep = "set"
		res.CleanupAttempted = false
		if uncertain {
			res.CleanupOK = false
			res.CleanupStatus = CleanupStatusUncertain
			res.OutcomeUncertain = true
		} else {
			res.CleanupOK = true
			res.CleanupStatus = CleanupStatusNotNeeded
		}
		res.Diagnosis = &diag
		res.FailureErr = err
		return res
	}
	res.Steps.Set = ProbeStep{Status: "pass", Outcome: OutcomeSuccess, DurationMS: dur}

	runCleanup := func() (Diagnosis, error) {
		res.CleanupAttempted = true
		delStart := time.Now()
		delErr := provider.Delete(probeAccount)
		delDur := time.Since(delStart).Milliseconds()
		if delErr != nil {
			delDiag := ClassifyKeyringError(delErr)
			if delDiag.CredentialStore == "" {
				delDiag.CredentialStore = store
			}
			uncertain := IsTimeout(delErr) || delDiag.Reason == ReasonTimeout
			outcome := OutcomeFailed
			status := CleanupStatusFailed
			if uncertain {
				outcome = OutcomeUncertain
				status = CleanupStatusUncertain
				res.OutcomeUncertain = true
			}
			res.Steps.Delete = ProbeStep{Status: "fail", Outcome: outcome, DurationMS: delDur, Reason: delDiag.Reason, Detail: delDiag.Observed}
			res.CleanupOK = false
			res.CleanupStatus = status
			return delDiag, delErr
		}
		res.Steps.Delete = ProbeStep{Status: "pass", Outcome: OutcomeSuccess, DurationMS: delDur}
		res.CleanupOK = true
		res.CleanupStatus = CleanupStatusCleaned
		return Diagnosis{}, nil
	}

	start = time.Now()
	got, err := provider.Get(probeAccount)
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
		res.Steps.Get = ProbeStep{Status: "fail", Outcome: OutcomeFailed, DurationMS: dur, Reason: diag.Reason, Detail: diag.Observed}
		res.FailedStep = "get"
		res.Diagnosis = &diag
		res.FailureErr = err
		_, _ = runCleanup()
		return res
	}
	output.RegisterSecret(got)
	res.Steps.Get = ProbeStep{Status: "pass", Outcome: OutcomeSuccess, DurationMS: dur}

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
		res.Steps.Compare = ProbeStep{Status: "fail", Outcome: OutcomeFailed, DurationMS: dur, Reason: diag.Reason, Detail: diag.Observed}
		res.FailedStep = "compare"
		res.Diagnosis = &diag
		res.FailureErr = errors.New("keyring probe value mismatch")
		_, _ = runCleanup()
		return res
	}
	res.Steps.Compare = ProbeStep{Status: "pass", Outcome: OutcomeSuccess, DurationMS: dur}

	if delDiag, delErr := runCleanup(); delErr != nil {
		res.FailedStep = "delete"
		res.Diagnosis = &delDiag
		res.FailureErr = delErr
		return res
	}

	res.OK = true
	return res
}

func generateProbeAccount() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate probe account suffix: %w", err)
	}
	return ProbeAccount + "-" + hex.EncodeToString(buf), nil
}

func generateProbeValue() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate probe token: %w", err)
	}
	return "qqmail-cli-probe-" + hex.EncodeToString(buf), nil
}
