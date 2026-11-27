package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/netx"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/policy"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
	"github.com/spf13/cobra"
)

type doctorCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

func newDoctorCommand(rt *Runtime) *cobra.Command {
	var checkSMTP, smtpOnly, noAuth bool
	cmd := &cobra.Command{Use: "doctor", Short: "Diagnose configuration, credentials, TLS, login, IMAP and SMTP capabilities", Args: cobra.NoArgs}
	cmd.Flags().BoolVar(&checkSMTP, "smtp", false, "also run stage-by-stage SMTP diagnostics (never sends mail)")
	cmd.Flags().BoolVar(&smtpOnly, "smtp-only", false, "run stage-by-stage SMTP diagnostics only (skip IMAP)")
	cmd.Flags().BoolVar(&noAuth, "no-auth", false, "run unauthenticated network/protocol probes without reading credentials or logging in")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		runIMAP := !smtpOnly
		runSMTP := checkSMTP || smtpOnly
		authMode := "authenticated"
		if noAuth {
			authMode = "unauthenticated"
		}

		checks := []doctorCheck{}
		completed := []string{}
		warnings := []output.Warning{}

		start := time.Now()
		_, _, named, err := rt.loadAccount()
		if err != nil {
			if !noAuth {
				return doctorFailureWithMeta(err, "config", completed, map[string]any{"auth_mode": authMode})
			}
			// Unauthenticated probe works even before an account is configured by
			// targeting default QQ IMAP/SMTP endpoints.
			named = account.Named{
				Name:     "default",
				IMAPHost: "imap.qq.com",
				IMAPPort: 993,
				SMTPHost: "smtp.qq.com",
				SMTPPort: 465,
			}
			checks = append(checks, doctorCheck{
				Name:       "config",
				Status:     "info",
				DurationMS: time.Since(start).Milliseconds(),
				Detail:     "unconfigured; probing default endpoints (imap.qq.com:993, smtp.qq.com:465)",
			})
		} else {
			checks = append(checks, doctorCheck{
				Name:       "config",
				Status:     "pass",
				DurationMS: time.Since(start).Milliseconds(),
				Detail:     named.Name + " <" + named.Email + ">",
			})
			completed = append(completed, "config")
		}

		authCode := ""
		if noAuth {
			checks = append(checks, doctorCheck{
				Name:       "credential",
				Status:     "info",
				DurationMS: 0,
				Detail:     "skipped (--no-auth unauthenticated probe)",
			})
		} else {
			start = time.Now()
			provider := rt.Secrets
			if rt.AuthCodeEnv {
				provider = secrets.Environment{}
			}
			authCode, err = provider.Get(named.Email)
			if err != nil {
				return doctorFailureWithMeta(err, "credential", completed, map[string]any{"auth_mode": authMode})
			}
			checks = append(checks, doctorCheck{
				Name:       "credential",
				Status:     "pass",
				DurationMS: time.Since(start).Milliseconds(),
				Detail:     provider.Source(),
			})
			completed = append(completed, "credential")
		}

		ctx, cancel := rt.context()
		defer cancel()

		before := []string{}
		after := []string{}
		var imapSection map[string]any

		if runIMAP {
			if noAuth || rt.ProbeIMAP != nil {
				probeFn := rt.ProbeIMAP
				if probeFn == nil {
					probeFn = imapx.Probe
				}
				imapRes, imapErr := probeFn(ctx, named, authCode, imapx.ProbeOptions{NoAuth: noAuth, Version: rt.Build.Version})
				if imapErr != nil {
					stage, _ := errmap.Classify(imapErr).Context["stage"].(string)
					failedCompleted := append(append([]string(nil), completed...), completedStageNames(imapRes.Stages)...)
					if len(imapRes.Stages) == 0 {
						failedCompleted = append(append([]string(nil), completed...), imapx.CompletedDialStages(stage)...)
					}
					extra := map[string]any{"auth_mode": authMode, "protocol": "imap"}
					if len(imapRes.Stages) > 0 {
						extra["stages"] = imapRes.Stages
					}
					return doctorFailureWithMeta(imapErr, stage, failedCompleted, extra)
				}
				before = nonNilStrings(imapRes.CapabilitiesBeforeLogin)
				after = nonNilStrings(imapRes.CapabilitiesAfterLogin)
				completed = append(completed, completedStageNames(imapRes.Stages)...)
				checks = append(checks, doctorCheck{
					Name:       "tls_login",
					Status:     "pass",
					DurationMS: imapRes.DurationMS,
					Detail:     doctorEndpointDetail(imapRes.Server, imapRes.Endpoint, imapRes.Transport, authMode),
				})
				checks = append(checks, doctorCheck{
					Name:       "capability",
					Status:     "pass",
					DurationMS: 0,
					Detail:     fmt.Sprintf("before=%v after=%v", before, after),
				})
				imapSection = map[string]any{
					"checked":                   true,
					"ok":                        true,
					"auth_mode":                 imapRes.AuthMode,
					"server":                    imapRes.Server,
					"endpoint":                  imapRes.Endpoint,
					"transport":                 imapRes.Transport,
					"duration_ms":               imapRes.DurationMS,
					"stages":                    imapRes.Stages,
					"capabilities_before_login": before,
					"capabilities_after_login":  after,
				}
			} else {
				start = time.Now()
				reader, dialErr := rt.Dial(ctx, named, authCode)
				if dialErr != nil {
					stage, _ := errmap.Classify(dialErr).Context["stage"].(string)
					failedCompleted := append(append([]string(nil), completed...), imapx.CompletedDialStages(stage)...)
					return doctorFailureWithMeta(dialErr, stage, failedCompleted, map[string]any{"auth_mode": authMode, "protocol": "imap"})
				}
				defer func() { _ = reader.Logout(context.Background()) }()
				b, a := reader.Capabilities()
				before = nonNilStrings(b)
				after = nonNilStrings(a)
				durMS := time.Since(start).Milliseconds()
				completed = append(completed, "dns", "tcp", "tls", "imap_greeting", "imap_capability_before_login", "imap_login", "imap_capability_after_login")
				server := named.IMAPHost + ":" + fmt.Sprint(named.IMAPPort)
				endpoint := ""
				if info, ok := imapx.ConnectionInfoOf(reader); ok {
					endpoint = info.Endpoint
				}
				checks = append(checks, doctorCheck{Name: "tls_login", Status: "pass", DurationMS: durMS, Detail: server})
				checks = append(checks, doctorCheck{Name: "capability", Status: "pass", DurationMS: 0, Detail: fmt.Sprintf("before=%v after=%v", before, after)})
				imapSection = map[string]any{
					"checked":                   true,
					"ok":                        true,
					"auth_mode":                 authMode,
					"server":                    server,
					"endpoint":                  endpoint,
					"transport":                 "implicit_tls",
					"duration_ms":               durMS,
					"capabilities_before_login": before,
					"capabilities_after_login":  after,
				}
			}
			if named.IMAPHost != "imap.qq.com" || named.IMAPPort != 993 {
				warnings = append(warnings, output.Warning{Code: "custom_server", Message: "当前账号使用非默认 IMAP 服务器，请确认配置未被篡改", Retryable: false})
			}
		} else {
			imapSection = map[string]any{
				"checked":   false,
				"ok":        false,
				"auth_mode": authMode,
				"server":    named.IMAPHost + ":" + fmt.Sprint(named.IMAPPort),
			}
		}

		var smtpSection map[string]any
		if runSMTP {
			smtpRes, smtpErr := rt.ProbeSMTP(ctx, named, authCode, sendmail.ProbeOptions{NoAuth: noAuth})
			if smtpErr != nil {
				classified := errmap.Classify(smtpErr)
				stage, _ := classified.Context["stage"].(string)
				transport, _ := classified.Context["transport"].(string)
				failedCompleted := append(append([]string(nil), completed...), completedStageNames(smtpRes.Stages)...)
				if len(smtpRes.Stages) == 0 && stage != "" {
					failedCompleted = append(append([]string(nil), completed...), sendmail.CompletedProbeStages(transport, stage)...)
				}
				extra := map[string]any{"auth_mode": authMode, "protocol": "smtp"}
				if len(smtpRes.Stages) > 0 {
					extra["stages"] = smtpRes.Stages
				}
				if len(smtpRes.PrimaryStages) > 0 {
					extra["primary_stages"] = smtpRes.PrimaryStages
				}
				if smtpRes.PrimaryError != "" {
					extra["primary_error"] = smtpRes.PrimaryError
				}
				return doctorFailureWithMeta(classified, stage, failedCompleted, extra)
			}
			checks = append(checks, doctorCheck{
				Name:       "smtp",
				Status:     "pass",
				DurationMS: smtpRes.DurationMS,
				Detail:     doctorEndpointDetail(smtpRes.Server, smtpRes.Endpoint, smtpRes.Transport, smtpRes.AuthMode),
			})
			for _, st := range smtpRes.Stages {
				checks = append(checks, doctorCheck{
					Name:       st.Stage,
					Status:     st.Status,
					DurationMS: st.DurationMS,
					Detail:     doctorStageDetail(st),
				})
			}
			smtpSection = map[string]any{
				"checked":       true,
				"ok":            true,
				"auth_mode":     smtpRes.AuthMode,
				"server":        smtpRes.Server,
				"endpoint":      smtpRes.Endpoint,
				"transport":     smtpRes.Transport,
				"fallback_used": smtpRes.FallbackUsed,
				"duration_ms":   smtpRes.DurationMS,
				"stages":        smtpRes.Stages,
				"extensions":    nonNilStrings(smtpRes.Extensions),
			}
			if len(smtpRes.PrimaryStages) > 0 {
				smtpSection["primary_stages"] = smtpRes.PrimaryStages
			}
			if smtpRes.PrimaryError != "" {
				smtpSection["primary_error"] = smtpRes.PrimaryError
			}
			if named.SMTPHost != "smtp.qq.com" || (named.SMTPPort != 465 && named.SMTPPort != 587) {
				warnings = append(warnings, output.Warning{Code: "custom_smtp_server", Message: "当前账号使用非默认 SMTP 服务器，请确认配置未被篡改", Retryable: false})
			}
		} else {
			smtpSection = map[string]any{
				"checked":   false,
				"ok":        false,
				"auth_mode": authMode,
				"server":    named.SMTPHost + ":" + fmt.Sprint(named.SMTPPort),
			}
		}

		credEnv := secrets.DiagnoseEnvironment()
		if !credEnv.Ready {
			checks = append(checks, doctorCheck{
				Name:       "credential_store",
				Status:     "info",
				DurationMS: 0,
				Detail:     fmt.Sprintf("%s (%s): %s", credEnv.CredentialStore, credEnv.Reason, credEnv.Detail),
				Suggestion: credEnv.Suggestion,
			})
		}

		if runtime.GOOS == "windows" {
			checks = append(checks, doctorCheck{Name: "console_utf8", Status: "info", Detail: "PowerShell 5.1 中文异常时设置 [Console]::OutputEncoding = [Text.Encoding]::UTF8"})
		}

		data := map[string]any{
			"checks":                    checks,
			"capabilities_before_login": before,
			"capabilities_after_login":  after,
			"readonly":                  policy.Readonly(),
			"auth_mode":                 authMode,
			"credential_store":          credEnv,
			"imap":                      imapSection,
			"smtp":                      smtpSection,
			"collection_scope_note":     "网页端收取选项会影响 IMAP 可见范围；具体影响待专用测试账号实测",
		}
		if rt.JSON {
			return writeDetailed(rt, cmd, data, warnings, output.Meta{Account: named.Name})
		}
		rt.notePartial(warnings)
		return writeResult(rt, cmd, data, func(w io.Writer) error {
			for _, check := range checks {
				if _, err := fmt.Fprintf(w, "%-28s %-5s %4dms %s\n", check.Name, check.Status, check.DurationMS, output.SanitizeLine(check.Detail)); err != nil {
					return err
				}
			}
			for _, warning := range warnings {
				_, _ = fmt.Fprintf(rt.Err, "警告：%s\n", output.SanitizeLine(warning.Message))
			}
			return nil
		})
	}
	return cmd
}

func doctorFailureWithMeta(err error, stage string, completed []string, extra map[string]any) error {
	if completed == nil {
		completed = []string{}
	}
	ctx := map[string]any{"completed_checks": completed}
	if stage != "" {
		ctx["stage"] = stage
	}
	for k, v := range extra {
		ctx[k] = v
	}
	return errmap.Classify(err).WithContext(ctx)
}

func completedStageNames(stages []netx.StageReport) []string {
	out := make([]string, 0, len(stages))
	for _, st := range stages {
		if st.Status == netx.StageStatusPass {
			out = append(out, st.Stage)
		}
	}
	return out
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func doctorEndpointDetail(server, endpoint, transport, authMode string) string {
	detail := server
	if endpoint != "" && endpoint != server {
		detail += " (" + endpoint + ")"
	}
	if transport != "" {
		detail += " [" + transport + ", " + authMode + "]"
	}
	return detail
}

func doctorStageDetail(st netx.StageReport) string {
	parts := []string{}
	if st.Endpoint != "" {
		parts = append(parts, st.Endpoint)
	}
	if st.Transport != "" {
		parts = append(parts, st.Transport)
	}
	if st.Detail != "" {
		parts = append(parts, st.Detail)
	}
	return joinNonEmpty(parts, " ")
}

func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}
