package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/policy"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/spf13/cobra"
)

func authLoginCredentialSaveError(source string, err error) error {
	kind := errmap.Config
	if errmap.Classify(err).Kind == errmap.PolicyDenied {
		kind = errmap.PolicyDenied
	}
	diag := secrets.ClassifyKeyringError(err)
	uncertain := secrets.IsTimeout(err) || diag.Reason == secrets.ReasonTimeout
	ctx := diag.Context()
	if source != "" && ctx["credential_store"] == "" {
		ctx["credential_store"] = source
	}
	ctx["stage"] = "credential_store"
	ctx["imap_verified"] = true
	ctx["config_saved"] = false
	ctx["outcome_uncertain"] = uncertain
	if uncertain {
		// Underlying platform keyring APIs cannot be canceled on timeout, so
		// the write may still finish late; never assert credential_saved=false.
		ctx["credential_save_state"] = secrets.OutcomeUncertain
	} else {
		ctx["credential_saved"] = false
		ctx["credential_save_state"] = secrets.OutcomeFailed
	}

	reasonPart := ""
	if label := diag.Reason.Label(); label != "" {
		reasonPart = "（" + label + "）"
	}
	var msg string
	if uncertain {
		msg = fmt.Sprintf("授权码已通过 IMAP 验证，但写入系统凭据管理器超时%s，无法确认凭证是否已保存；登录尚未完成，账号配置未写入", reasonPart)
	} else {
		msg = fmt.Sprintf("授权码已通过 IMAP 验证，但无法写入系统凭据管理器%s；登录尚未完成，账号配置未写入", reasonPart)
	}

	suggestion := diag.Suggestion
	if suggestion == "" {
		suggestion = secrets.SuggestionFor(diag.Platform, diag.Reason)
	}
	if uncertain && !strings.Contains(suggestion, "auth status") {
		suggestion += "；请先排查密钥环解锁/守护进程状态并核对系统凭据管理器（或运行 qqmail-cli auth status --json），确认后再重新运行 qqmail-cli auth login"
	} else if !strings.Contains(suggestion, "auth login") {
		suggestion += "；修复凭据管理器后请重新运行 qqmail-cli auth login"
	}

	return &errmap.Error{
		Kind:       kind,
		Message:    msg,
		Suggestion: suggestion,
		Context:    ctx,
		Cause:      err,
	}
}

func authLogoutCredentialDeleteError(source string, err error) error {
	app := errmap.Classify(err)
	diag := secrets.ClassifyKeyringError(err)
	uncertain := secrets.IsTimeout(err) || diag.Reason == secrets.ReasonTimeout
	if !uncertain {
		return err
	}
	ctx := diag.Context()
	if source != "" && ctx["credential_store"] == "" {
		ctx["credential_store"] = source
	}
	ctx["stage"] = "credential_store"
	ctx["local_credential_deleted_state"] = secrets.OutcomeUncertain
	ctx["outcome_uncertain"] = true
	ctx["config_updated"] = false
	suggestion := app.Suggestion
	if suggestion == "" {
		suggestion = diag.Suggestion
	}
	if !strings.Contains(suggestion, "auth status") {
		suggestion += "；请先运行 qqmail-cli auth status --json 或在系统凭据管理器中核对凭据是否已移除，再决定是否重试 qqmail-cli auth logout"
	}
	return &errmap.Error{
		Kind:       app.Kind,
		Message:    "删除系统凭据超时，无法确认本地凭据是否已移除；账号配置尚未修改",
		Suggestion: suggestion,
		Context:    ctx,
		Cause:      err,
	}
}

func populateAuthStatusCredentialStore(data map[string]any, secretErr error) {
	if secretErr == nil {
		data["credential_store_status"] = "ok"
		return
	}
	appErr := errmap.Classify(secretErr)
	if appErr.Kind == errmap.AuthCodeMissing {
		data["credential_store_status"] = "not_found"
		return
	}
	diag := secrets.ClassifyKeyringError(secretErr)
	data["credential_store_status"] = "error"
	data["credential_store_reason"] = string(diag.Reason)
	data["credential_store_platform"] = diag.Platform
	suggestion := diag.Suggestion
	if suggestion == "" {
		suggestion = appErr.Suggestion
	}
	if suggestion != "" {
		data["credential_store_suggestion"] = suggestion
	}
	if diag.Observed != "" {
		data["credential_store_observed"] = diag.Observed
	}
	if diag.DBusSessionBusSet != nil {
		data["dbus_session_bus_set"] = *diag.DBusSessionBusSet
	}
}

func writeAuthStatusHuman(w io.Writer, named account.Named, source string, present bool, data map[string]any) error {
	if _, err := fmt.Fprintf(
		w,
		"%s <%s> configured=%t credential=%t (%s)\n",
		output.SanitizeLine(named.Name),
		output.SanitizeLine(named.Email),
		true,
		present,
		output.SanitizeLine(source),
	); err != nil {
		return err
	}
	status, _ := data["credential_store_status"].(string)
	if status != "error" {
		return nil
	}
	reason, _ := data["credential_store_reason"].(string)
	if _, err := fmt.Fprintf(w, "凭据存储状态：%s（reason=%s）\n", output.SanitizeLine(status), output.SanitizeLine(reason)); err != nil {
		return err
	}
	if suggestion, _ := data["credential_store_suggestion"].(string); suggestion != "" {
		if _, err := fmt.Fprintf(w, "建议：%s\n", output.SanitizeHuman(output.RedactString(suggestion))); err != nil {
			return err
		}
	}
	return nil
}

func newAuthKeyringTestCommand(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keyring-test",
		Short: "Verify system credential store write, read, and delete with a temporary probe",
		Args:  cobra.NoArgs,
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := policy.RequireMutationAllowed(); err != nil {
			return err
		}
		if rt.AuthCodeEnv || rt.Secrets.Source() == "env" {
			return &errmap.Error{
				Kind:       errmap.Usage,
				Message:    "auth keyring-test 用于检测系统凭据管理器，不支持 --auth-code-env",
				Suggestion: "去掉 --auth-code-env 后重新运行 qqmail-cli auth keyring-test",
			}
		}

		res := secrets.ProbeKeyring(rt.Secrets)
		if res.OK {
			data := map[string]any{
				"credential_store":  res.CredentialStore,
				"service":           res.Service,
				"probe_account":     res.ProbeAccount,
				"platform":          res.Platform,
				"environment":       res.Environment,
				"steps":             res.Steps,
				"cleanup_attempted": res.CleanupAttempted,
				"cleanup_ok":        res.CleanupOK,
				"cleanup_status":    res.CleanupStatus,
				"outcome_uncertain": res.OutcomeUncertain,
			}
			return writeResult(rt, cmd, data, func(w io.Writer) error {
				return writeKeyringTestHuman(w, res)
			})
		}

		var diag secrets.Diagnosis
		if res.Diagnosis != nil {
			diag = *res.Diagnosis
		} else {
			diag = secrets.ClassifyKeyringError(res.FailureErr)
		}
		ctx := diag.Context()
		ctx["credential_store"] = res.CredentialStore
		ctx["service"] = res.Service
		ctx["probe_account"] = res.ProbeAccount
		ctx["failed_step"] = res.FailedStep
		ctx["steps"] = res.Steps
		ctx["cleanup_attempted"] = res.CleanupAttempted
		ctx["cleanup_ok"] = res.CleanupOK
		ctx["cleanup_status"] = res.CleanupStatus
		ctx["outcome_uncertain"] = res.OutcomeUncertain
		ctx["environment"] = res.Environment

		reasonPart := ""
		if label := diag.Reason.Label(); label != "" {
			reasonPart = "（" + label + "）"
		}
		var cleanupNote string
		switch res.CleanupStatus {
		case secrets.CleanupStatusNotNeeded:
			cleanupNote = "未写入临时测试项"
		case secrets.CleanupStatusCleaned:
			cleanupNote = "临时测试项已清理"
		case secrets.CleanupStatusUncertain:
			cleanupNote = fmt.Sprintf("操作超时且底层平台接口不支持取消，无法确认 %s / %s 是否残留，请人工核对并在必要时手动移除", res.Service, res.ProbeAccount)
		default:
			cleanupNote = fmt.Sprintf("临时测试项清理失败，请手动检查并移除 %s / %s", res.Service, res.ProbeAccount)
		}
		msg := fmt.Sprintf("系统凭据管理器自检在 %s 阶段失败%s；%s", res.FailedStep, reasonPart, cleanupNote)

		if !rt.JSON {
			_ = writeKeyringTestHuman(rt.Err, res)
		}
		return &errmap.Error{
			Kind:       errmap.Config,
			Message:    msg,
			Suggestion: diag.Suggestion,
			Context:    ctx,
			Cause:      res.FailureErr,
		}
	}
	return cmd
}

func writeKeyringTestHuman(w io.Writer, res secrets.ProbeResult) error {
	lines := []string{
		fmt.Sprintf("credential_store=%s platform=%s service=%s probe_account=%s", res.CredentialStore, res.Platform, res.Service, res.ProbeAccount),
	}
	if res.Environment.DBusSessionBusSet != nil {
		line := fmt.Sprintf("environment ready=%t dbus_session_bus_set=%t", res.Environment.Ready, *res.Environment.DBusSessionBusSet)
		if res.Environment.DBusSocketExists != nil {
			line += fmt.Sprintf(" dbus_socket_exists=%t", *res.Environment.DBusSocketExists)
		}
		lines = append(lines, line)
	} else {
		lines = append(lines, fmt.Sprintf("environment ready=%t", res.Environment.Ready))
	}
	for _, item := range []struct {
		name string
		step secrets.ProbeStep
	}{
		{"set", res.Steps.Set},
		{"get", res.Steps.Get},
		{"compare", res.Steps.Compare},
		{"delete", res.Steps.Delete},
	} {
		line := fmt.Sprintf("  %-8s %-7s (%dms)", item.name, item.step.Status, item.step.DurationMS)
		if item.step.Outcome != "" && item.step.Outcome != secrets.OutcomeSuccess {
			line += " outcome=" + item.step.Outcome
		}
		if item.step.Reason != "" {
			line += " reason=" + string(item.step.Reason)
		}
		lines = append(lines, line)
	}
	lines = append(lines, fmt.Sprintf("cleanup_attempted=%t cleanup_ok=%t cleanup_status=%s outcome_uncertain=%t", res.CleanupAttempted, res.CleanupOK, res.CleanupStatus, res.OutcomeUncertain))
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, output.SanitizeHuman(output.RedactString(line))); err != nil {
			return err
		}
	}
	return nil
}
