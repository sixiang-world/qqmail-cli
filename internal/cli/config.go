package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/policy"
	"github.com/spf13/cobra"
)

func newConfigCommand(rt *Runtime) *cobra.Command {
	cmd := requireSubcommand(&cobra.Command{
		Use:   "config",
		Short: "Validate configuration and review effective send policies (read-only)",
	})
	cmd.AddCommand(
		newConfigValidateCommand(rt),
		newConfigPolicyCommand(rt),
		newConfigDiffCommand(rt),
	)
	return cmd
}

func resolveConfigFlagPath(cmd *cobra.Command, rt *Runtime, fileFlag string) (string, error) {
	configFlag := cmd.Root().PersistentFlags().Lookup("config")
	configChanged := configFlag != nil && configFlag.Changed
	fileChanged := cmd.Flags().Changed("file")
	if configChanged && fileChanged {
		return "", &errmap.Error{
			Kind:       errmap.Usage,
			Message:    "--config 与 --file 不能同时使用",
			Suggestion: "只通过 --config 或 --file 之一指定要检查的配置文件路径",
		}
	}
	if fileChanged {
		if strings.TrimSpace(fileFlag) == "" {
			return "", &errmap.Error{Kind: errmap.Usage, Message: "--file 路径不能为空"}
		}
		return fileFlag, nil
	}
	return account.ResolveConfigPath(rt.ConfigPath)
}

func newConfigValidateCommand(rt *Runtime) *cobra.Command {
	var filePath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Strictly parse and validate the TOML configuration file",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&filePath, "file", "", "validate a specific TOML config file instead of --config / default path")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		targetPath, err := resolveConfigFlagPath(cmd, rt, filePath)
		if err != nil {
			return err
		}
		report, err := account.ValidateFile(targetPath)
		if err != nil {
			return err
		}
		if !report.Valid {
			if !rt.JSON {
				printValidationReport(rt.Err, report)
			}
			msg := fmt.Sprintf("配置校验失败：发现 %d 处错误", report.Summary.Errors)
			if !report.Exists {
				msg = "配置文件不存在"
			}
			suggestion := "按上述诊断修正 config.toml 后重新运行 qqmail-cli config validate"
			if !report.Exists {
				suggestion = "运行 qqmail-cli auth login 初始化配置，或通过 --config/--file 指定现有配置文件"
			}
			return &errmap.Error{
				Kind:       errmap.Config,
				Message:    msg,
				Suggestion: suggestion,
				Context: map[string]any{
					"config_path": report.ConfigPath,
					"exists":      report.Exists,
					"valid":       false,
					"diagnostics": report.Diagnostics,
					"summary":     report.Summary,
				},
			}
		}
		return writeResult(rt, cmd, report, func(w io.Writer) error {
			printValidationReport(w, report)
			return nil
		})
	}
	return cmd
}

func newConfigPolicyCommand(rt *Runtime) *cobra.Command {
	var filePath string
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Preview effective send, autosend, and server endpoint policies",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&filePath, "file", "", "preview policy from a specific TOML config file instead of --config / default path")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		targetPath, err := resolveConfigFlagPath(cmd, rt, filePath)
		if err != nil {
			return err
		}
		if _, statErr := os.Stat(targetPath); errors.Is(statErr, os.ErrNotExist) {
			return &errmap.Error{
				Kind:       errmap.Config,
				Message:    "配置文件不存在",
				Suggestion: "运行 qqmail-cli auth login 初始化配置，或通过 --config/--file 指定配置文件",
				Context: map[string]any{
					"config_path": targetPath,
					"exists":      false,
				},
			}
		}
		cfg, loadedPath, err := account.Load(targetPath)
		if err != nil {
			app := errmap.Classify(err)
			if app.Suggestion == "" {
				copied := *app
				copied.Suggestion = "运行 qqmail-cli config validate 查看详细配置错误"
				return &copied
			}
			return err
		}
		preview, err := account.BuildPolicyPreview(cfg, loadedPath, rt.Account, policy.Readonly())
		if err != nil {
			return err
		}
		return writeResult(rt, cmd, preview, func(w io.Writer) error {
			printPolicyPreview(w, preview)
			return nil
		})
	}
	return cmd
}

func newConfigDiffCommand(rt *Runtime) *cobra.Command {
	var proposedPath string
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Compare current effective policy against a proposed TOML file without writing",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&proposedPath, "proposed", "", "path to the proposed TOML config file to review")
	_ = cmd.MarkFlagRequired("proposed")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		diff, err := account.DiffFiles(rt.ConfigPath, proposedPath, rt.Account)
		if err != nil {
			if !rt.JSON {
				printDiffValidationErrors(rt.Err, err)
			}
			return err
		}
		return writeResult(rt, cmd, diff, func(w io.Writer) error {
			printPolicyDiff(w, diff)
			return nil
		})
	}
	return cmd
}

func printValidationReport(w io.Writer, report account.ValidationReport) {
	status := "VALID"
	if !report.Valid {
		status = "INVALID"
	}
	_, _ = fmt.Fprintf(w, "Config: %s (%s: %d error(s), %d warning(s), %d info)\n",
		output.RedactString(output.SanitizeLine(report.ConfigPath)),
		status,
		report.Summary.Errors,
		report.Summary.Warnings,
		report.Summary.Info,
	)
	for _, d := range report.Diagnostics {
		loc := ""
		if d.Line > 0 {
			loc = fmt.Sprintf(" (line %d, col %d)", d.Line, d.Column)
		}
		pathPart := ""
		if d.Path != "" {
			pathPart = " " + output.RedactString(output.SanitizeLine(d.Path))
		}
		_, _ = fmt.Fprintf(w, "  [%s] %s%s%s: %s\n",
			strings.ToUpper(string(d.Severity)),
			output.SanitizeLine(d.Code),
			pathPart,
			loc,
			output.RedactString(output.SanitizeLine(d.Message)),
		)
	}
}

func printPolicyPreview(w io.Writer, preview account.PolicyPreview) {
	_, _ = fmt.Fprintf(w, "Config: %s\n", output.RedactString(output.SanitizeLine(preview.ConfigPath)))
	_, _ = fmt.Fprintf(w, "Default account: %s | Readonly: %t\n", output.SanitizeLine(preview.DefaultAccount), preview.Readonly)
	if len(preview.UnknownKeys) > 0 {
		_, _ = fmt.Fprintf(w, "Top-level unknown keys: %s\n", output.RedactString(output.SanitizeLine(strings.Join(preview.UnknownKeys, ", "))))
	}
	if len(preview.Accounts) == 0 {
		_, _ = fmt.Fprintln(w, "Accounts: (none)")
	}
	for _, acct := range preview.Accounts {
		defMarker := ""
		if acct.IsDefault {
			defMarker = " (default)"
		}
		_, _ = fmt.Fprintf(w, "\n[%s]%s <%s>\n", output.SanitizeLine(acct.Name), defMarker, output.SanitizeLine(acct.Email))
		_, _ = fmt.Fprintf(w, "  IMAP: %s:%d (custom=%t)\n", output.SanitizeLine(acct.IMAPHost), acct.IMAPPort, acct.CustomIMAP)
		_, _ = fmt.Fprintf(w, "  SMTP: %s:%d (custom=%t)\n", output.SanitizeLine(acct.SMTPHost), acct.SMTPPort, acct.CustomSMTP)
		allowText := "(empty; all sends denied)"
		if len(acct.SendAllowlist) > 0 {
			allowText = strings.Join(sanitizeSlice(acct.SendAllowlist), ", ")
		}
		_, _ = fmt.Fprintf(w, "  send_allowlist (ready=%t): %s\n", acct.AllowlistReady, allowText)
		blackText := "(none)"
		if len(acct.SendBlacklist) > 0 {
			blackText = strings.Join(sanitizeSlice(acct.SendBlacklist), ", ")
		}
		_, _ = fmt.Fprintf(w, "  send_blacklist: %s\n", blackText)
		limitNote := ""
		if acct.DailyAutoLimitDefault {
			limitNote = " (default)"
		}
		_, _ = fmt.Fprintf(w, "  auto_send: %t (effective=%t) | daily_auto_limit: %d%s\n", acct.AutoSend, acct.AutoSendEffective, acct.DailyAutoLimit, limitNote)
		if len(acct.UnknownKeys) > 0 {
			_, _ = fmt.Fprintf(w, "  unknown_keys (fail-closed blocks auto_send): %s\n", output.RedactString(output.SanitizeLine(strings.Join(acct.UnknownKeys, ", "))))
		}
		if len(acct.AutoSendBlockReasons) > 0 {
			_, _ = fmt.Fprintf(w, "  auto_send_block_reasons: %s\n", strings.Join(sanitizeSlice(acct.AutoSendBlockReasons), ", "))
		}
	}
	_, _ = fmt.Fprintf(w, "\nNote: %s\n", output.SanitizeHuman(preview.PolicyBoundaryNote))
}

func printPolicyDiff(w io.Writer, diff account.PolicyDiff) {
	_, _ = fmt.Fprintf(w, "Current:  %s\n", output.RedactString(output.SanitizeLine(diff.CurrentPath)))
	_, _ = fmt.Fprintf(w, "Proposed: %s\n", output.RedactString(output.SanitizeLine(diff.ProposedPath)))
	if diff.AccountFilter != "" {
		_, _ = fmt.Fprintf(w, "Account:  %s\n", output.SanitizeLine(diff.AccountFilter))
	}
	_, _ = fmt.Fprintf(w, "Summary:  %d change(s) (expands=%d, restricts=%d, neutral=%d) | expands_permissions=%t\n",
		diff.Summary.Total, diff.Summary.Expands, diff.Summary.Restricts, diff.Summary.Neutral, diff.ExpandsPermissions)
	if !diff.HasChanges {
		_, _ = fmt.Fprintln(w, "  (no effective policy changes)")
	}
	for _, ch := range diff.Changes {
		scope := ch.Field
		if ch.Account != "" {
			scope = ch.Account + "." + ch.Field
		}
		detail := formatChangeValues(ch)
		_, _ = fmt.Fprintf(w, "  [%s] %s (%s)%s — %s\n",
			strings.ToUpper(string(ch.Impact)),
			output.SanitizeLine(scope),
			output.SanitizeLine(ch.Action),
			detail,
			output.RedactString(output.SanitizeLine(ch.Reason)),
		)
	}
	if diff.CurrentValidation.Summary.Warnings > 0 || diff.ProposedValidation.Summary.Warnings > 0 {
		_, _ = fmt.Fprintf(w, "Warnings: current=%d, proposed=%d\n",
			diff.CurrentValidation.Summary.Warnings, diff.ProposedValidation.Summary.Warnings)
	}
	_, _ = fmt.Fprintf(w, "Note: %s\n", output.SanitizeHuman(diff.PolicyBoundaryNote))
}

func formatChangeValues(ch account.PolicyChange) string {
	switch {
	case ch.Before != nil && ch.After != nil:
		return fmt.Sprintf(": %s -> %s", formatSanitizedAny(ch.Before), formatSanitizedAny(ch.After))
	case ch.After != nil:
		return fmt.Sprintf(": +%s", formatSanitizedAny(ch.After))
	case ch.Before != nil:
		return fmt.Sprintf(": -%s", formatSanitizedAny(ch.Before))
	default:
		return ""
	}
}

func formatSanitizedAny(v any) string {
	return output.RedactString(output.SanitizeLine(fmt.Sprint(v)))
}

func printDiffValidationErrors(w io.Writer, err error) {
	app := errmap.Classify(err)
	if app.Context == nil {
		return
	}
	if report, ok := app.Context["current_validation"].(account.ValidationReport); ok && !report.Valid {
		_, _ = fmt.Fprintln(w, "Current config validation errors:")
		printValidationReport(w, report)
	}
	if report, ok := app.Context["proposed_validation"].(account.ValidationReport); ok && !report.Valid {
		_, _ = fmt.Fprintln(w, "Proposed config validation errors:")
		printValidationReport(w, report)
	}
}

func sanitizeSlice(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = output.RedactString(output.SanitizeLine(value))
	}
	return out
}
