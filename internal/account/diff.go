package account

import (
	"fmt"
	"sort"
	"strings"

	"github.com/situker/qqmail-cli/internal/errmap"
)

type ChangeImpact string

const (
	ImpactExpands   ChangeImpact = "expands"
	ImpactRestricts ChangeImpact = "restricts"
	ImpactNeutral   ChangeImpact = "neutral"
)

type PolicyChange struct {
	Account string       `json:"account,omitempty"`
	Field   string       `json:"field"`
	Action  string       `json:"action"`
	Impact  ChangeImpact `json:"impact"`
	Before  any          `json:"before,omitempty"`
	After   any          `json:"after,omitempty"`
	Reason  string       `json:"reason"`
}

type DiffSummary struct {
	Total     int `json:"total"`
	Expands   int `json:"expands"`
	Restricts int `json:"restricts"`
	Neutral   int `json:"neutral"`
}

type PolicyDiff struct {
	CurrentPath        string           `json:"current_path"`
	ProposedPath       string           `json:"proposed_path"`
	AccountFilter      string           `json:"account_filter,omitempty"`
	HasChanges         bool             `json:"has_changes"`
	ExpandsPermissions bool             `json:"expands_permissions"`
	Changes            []PolicyChange   `json:"changes"`
	Summary            DiffSummary      `json:"summary"`
	CurrentValidation  ValidationReport `json:"current_validation"`
	ProposedValidation ValidationReport `json:"proposed_validation"`
	PolicyBoundaryNote string           `json:"policy_boundary_note"`
}

// DiffFiles strictly validates both currentPath and proposedPath and compares
// their effective policies so a human can review whether the proposed file
// expands permissions before applying any edit manually. It never writes files.
func DiffFiles(currentPath, proposedPath, accountFilter string) (PolicyDiff, error) {
	resolvedCurrent, err := ResolveConfigPath(currentPath)
	if err != nil {
		return PolicyDiff{}, err
	}
	if strings.TrimSpace(proposedPath) == "" {
		return PolicyDiff{}, &errmap.Error{Kind: errmap.Usage, Message: "缺少必填参数 --proposed"}
	}

	currentReport, err := ValidateFile(resolvedCurrent)
	if err != nil {
		return PolicyDiff{}, err
	}
	proposedReport, err := ValidateFile(proposedPath)
	if err != nil {
		return PolicyDiff{}, err
	}

	if !currentReport.Valid || !proposedReport.Valid {
		var msg string
		switch {
		case !currentReport.Valid && !proposedReport.Valid:
			msg = fmt.Sprintf("当前配置（%d 处错误）与建议配置（%d 处错误）均未通过校验", currentReport.Summary.Errors, proposedReport.Summary.Errors)
		case !currentReport.Valid:
			msg = fmt.Sprintf("当前配置未通过校验（%d 处错误）", currentReport.Summary.Errors)
		default:
			msg = fmt.Sprintf("建议配置未通过校验（%d 处错误）", proposedReport.Summary.Errors)
		}
		return PolicyDiff{}, &errmap.Error{
			Kind:       errmap.Config,
			Message:    msg,
			Suggestion: "先分别运行 qqmail-cli config validate 检查并修正配置错误",
			Context: map[string]any{
				"current_path":        currentReport.ConfigPath,
				"proposed_path":       proposedReport.ConfigPath,
				"current_validation":  currentReport,
				"proposed_validation": proposedReport,
			},
		}
	}

	currentCfg, _, err := Load(currentReport.ConfigPath)
	if err != nil {
		return PolicyDiff{}, err
	}
	proposedCfg, _, err := Load(proposedReport.ConfigPath)
	if err != nil {
		return PolicyDiff{}, err
	}

	return diffConfigs(currentCfg, proposedCfg, currentReport, proposedReport, accountFilter)
}

func diffConfigs(currentCfg, proposedCfg *Config, currentReport, proposedReport ValidationReport, accountFilter string) (PolicyDiff, error) {
	changes := []PolicyChange{}

	if accountFilter != "" {
		_, inCurrent := currentCfg.Accounts[accountFilter]
		_, inProposed := proposedCfg.Accounts[accountFilter]
		if !inCurrent && !inProposed {
			return PolicyDiff{}, &errmap.Error{
				Kind:       errmap.Config,
				Message:    "账号在当前配置与建议配置中均不存在：" + accountFilter,
				Suggestion: "检查 --account 名称，或省略 --account 以对比全部账号",
			}
		}
	}

	if accountFilter == "" && currentCfg.DefaultAccount != proposedCfg.DefaultAccount {
		impact := classifyDefaultAccountChange(currentCfg, proposedCfg)
		reason := "默认账号变更"
		if impact == ImpactExpands {
			reason = "默认账号切换为具有更宽发送或自动发送权限的账号"
		}
		changes = append(changes, PolicyChange{
			Field:  "default_account",
			Action: "changed",
			Impact: impact,
			Before: currentCfg.DefaultAccount,
			After:  proposedCfg.DefaultAccount,
			Reason: reason,
		})
	}

	names := collectAccountNames(currentCfg, proposedCfg, accountFilter)
	for _, name := range names {
		curRaw, inCurrent := currentCfg.Accounts[name]
		propRaw, inProposed := proposedCfg.Accounts[name]
		switch {
		case !inCurrent && inProposed:
			propNamed, _ := proposedCfg.Resolve(name)
			pol := accountPolicyFrom(propNamed, propRaw, false)
			impact := ImpactNeutral
			reason := "新增账号（默认官方服务器，白名单为空且未开启自动发送）"
			if pol.AllowlistReady || pol.AutoSend || pol.CustomIMAP || pol.CustomSMTP {
				impact = ImpactExpands
				reason = "新增具有发送权限、自动发送或自定义服务器配置的账号"
			}
			changes = append(changes, PolicyChange{
				Account: name,
				Field:   "account",
				Action:  "added",
				Impact:  impact,
				After:   accountSummaryMap(pol),
				Reason:  reason,
			})
		case inCurrent && !inProposed:
			curNamed, _ := currentCfg.Resolve(name)
			pol := accountPolicyFrom(curNamed, curRaw, false)
			impact := ImpactNeutral
			reason := "移除无发送权限的默认配置账号"
			if pol.AllowlistReady || pol.AutoSend || pol.CustomIMAP || pol.CustomSMTP {
				impact = ImpactRestricts
				reason = "移除具有发送权限、自动发送或自定义服务器配置的账号"
			}
			changes = append(changes, PolicyChange{
				Account: name,
				Field:   "account",
				Action:  "removed",
				Impact:  impact,
				Before:  accountSummaryMap(pol),
				Reason:  reason,
			})
		case inCurrent && inProposed:
			curNamed, _ := currentCfg.Resolve(name)
			propNamed, _ := proposedCfg.Resolve(name)
			curPol := accountPolicyFrom(curNamed, curRaw, false)
			propPol := accountPolicyFrom(propNamed, propRaw, false)
			changes = append(changes, diffAccountPolicies(curPol, propPol)...)
		}
	}

	var summary DiffSummary
	summary.Total = len(changes)
	for _, ch := range changes {
		switch ch.Impact {
		case ImpactExpands:
			summary.Expands++
		case ImpactRestricts:
			summary.Restricts++
		case ImpactNeutral:
			summary.Neutral++
		}
	}

	return PolicyDiff{
		CurrentPath:        currentReport.ConfigPath,
		ProposedPath:       proposedReport.ConfigPath,
		AccountFilter:      accountFilter,
		HasChanges:         len(changes) > 0,
		ExpandsPermissions: summary.Expands > 0,
		Changes:            changes,
		Summary:            summary,
		CurrentValidation:  currentReport,
		ProposedValidation: proposedReport,
		PolicyBoundaryNote: PolicyBoundaryNote,
	}, nil
}

func classifyDefaultAccountChange(currentCfg, proposedCfg *Config) ChangeImpact {
	propNamed, err := proposedCfg.Resolve(proposedCfg.DefaultAccount)
	if err != nil {
		return ImpactNeutral
	}
	propPol := accountPolicyFrom(propNamed, proposedCfg.Accounts[proposedCfg.DefaultAccount], false)
	curNamed, err := currentCfg.Resolve(currentCfg.DefaultAccount)
	if err != nil {
		if propPol.AllowlistReady || propPol.AutoSend || propPol.CustomIMAP || propPol.CustomSMTP {
			return ImpactExpands
		}
		return ImpactNeutral
	}
	curPol := accountPolicyFrom(curNamed, currentCfg.Accounts[currentCfg.DefaultAccount], false)
	if propPol.AutoSend && !curPol.AutoSend {
		return ImpactExpands
	}
	if propPol.AutoSend && curPol.AutoSend && propPol.DailyAutoLimit > curPol.DailyAutoLimit {
		return ImpactExpands
	}
	if (propPol.CustomIMAP && !curPol.CustomIMAP) || (propPol.CustomSMTP && !curPol.CustomSMTP) {
		return ImpactExpands
	}
	for _, entry := range normalizeSortedSet(propPol.SendAllowlist) {
		if !allowlistEntryCovered(entry, curPol.SendAllowlist) {
			return ImpactExpands
		}
	}
	if propPol.AutoSend {
		propBlack := toSet(normalizeSortedSet(propPol.SendBlacklist))
		for _, blocked := range normalizeSortedSet(curPol.SendBlacklist) {
			if AllowlistMatches(blocked, propPol.SendAllowlist) && !propBlack[blocked] {
				return ImpactExpands
			}
		}
	}
	return ImpactNeutral
}

func diffAccountPolicies(before, after AccountPolicy) []PolicyChange {
	changes := []PolicyChange{}
	name := before.Name

	if !strings.EqualFold(strings.TrimSpace(before.Email), strings.TrimSpace(after.Email)) {
		changes = append(changes, PolicyChange{
			Account: name,
			Field:   "email",
			Action:  "changed",
			Impact:  ImpactNeutral,
			Before:  before.Email,
			After:   after.Email,
			Reason:  "账号邮箱地址变更",
		})
	}

	beforeIMAP := fmt.Sprintf("%s:%d", strings.ToLower(before.IMAPHost), before.IMAPPort)
	afterIMAP := fmt.Sprintf("%s:%d", strings.ToLower(after.IMAPHost), after.IMAPPort)
	if beforeIMAP != afterIMAP {
		impact := ImpactExpands
		reason := "引入或变更自定义 IMAP 服务器端点，可能改变凭证发送目标"
		if !after.CustomIMAP {
			impact = ImpactRestricts
			reason = fmt.Sprintf("IMAP 服务器恢复为默认官方端点 %s:%d", DefaultIMAPHost, DefaultIMAPPort)
		}
		changes = append(changes, PolicyChange{
			Account: name,
			Field:   "imap_endpoint",
			Action:  "changed",
			Impact:  impact,
			Before:  fmt.Sprintf("%s:%d", before.IMAPHost, before.IMAPPort),
			After:   fmt.Sprintf("%s:%d", after.IMAPHost, after.IMAPPort),
			Reason:  reason,
		})
	}

	beforeSMTP := fmt.Sprintf("%s:%d", strings.ToLower(before.SMTPHost), before.SMTPPort)
	afterSMTP := fmt.Sprintf("%s:%d", strings.ToLower(after.SMTPHost), after.SMTPPort)
	if beforeSMTP != afterSMTP {
		impact := ImpactExpands
		reason := "引入或变更自定义 SMTP 服务器端点，可能改变凭证与外发邮件投递目标"
		if !after.CustomSMTP {
			impact = ImpactRestricts
			reason = fmt.Sprintf("SMTP 服务器恢复为默认官方端点 %s:%d", DefaultSMTPHost, DefaultSMTPPort)
		}
		changes = append(changes, PolicyChange{
			Account: name,
			Field:   "smtp_endpoint",
			Action:  "changed",
			Impact:  impact,
			Before:  fmt.Sprintf("%s:%d", before.SMTPHost, before.SMTPPort),
			After:   fmt.Sprintf("%s:%d", after.SMTPHost, after.SMTPPort),
			Reason:  reason,
		})
	}

	beforeAllow := normalizeSortedSet(before.SendAllowlist)
	afterAllow := normalizeSortedSet(after.SendAllowlist)
	beforeAllowMap := toSet(beforeAllow)
	afterAllowMap := toSet(afterAllow)

	for _, entry := range afterAllow {
		if beforeAllowMap[entry] {
			continue
		}
		impact := ImpactExpands
		reason := "新增允许发送的收件人"
		if strings.HasPrefix(entry, "*@") {
			reason = "新增域名通配符白名单条目"
			if publicMailDomains[entry[2:]] {
				reason = "新增覆盖公共邮箱域的通配符白名单条目"
			}
		} else if allowlistEntryCovered(entry, beforeAllow) {
			impact = ImpactNeutral
			reason = "新增精确白名单条目（已被原白名单通配符覆盖）"
		}
		changes = append(changes, PolicyChange{
			Account: name,
			Field:   "send_allowlist",
			Action:  "added",
			Impact:  impact,
			After:   entry,
			Reason:  reason,
		})
	}
	for _, entry := range beforeAllow {
		if afterAllowMap[entry] {
			continue
		}
		impact := ImpactRestricts
		reason := "移除允许发送的白名单条目"
		if !strings.HasPrefix(entry, "*@") && allowlistEntryCovered(entry, afterAllow) {
			impact = ImpactNeutral
			reason = "移除精确白名单条目（仍被新白名单通配符覆盖）"
		}
		changes = append(changes, PolicyChange{
			Account: name,
			Field:   "send_allowlist",
			Action:  "removed",
			Impact:  impact,
			Before:  entry,
			Reason:  reason,
		})
	}

	if before.AutoSend != after.AutoSend {
		if after.AutoSend {
			changes = append(changes, PolicyChange{
				Account: name,
				Field:   "auto_send",
				Action:  "changed",
				Impact:  ImpactExpands,
				Before:  false,
				After:   true,
				Reason:  "开启自动发送（允许白名单内邮件跳过人工 TTY 确认）",
			})
		} else {
			changes = append(changes, PolicyChange{
				Account: name,
				Field:   "auto_send",
				Action:  "changed",
				Impact:  ImpactRestricts,
				Before:  true,
				After:   false,
				Reason:  "关闭自动发送（恢复全部发送的人工 TTY 确认）",
			})
		}
	}

	if before.DailyAutoLimit != after.DailyAutoLimit {
		if after.DailyAutoLimit > before.DailyAutoLimit {
			changes = append(changes, PolicyChange{
				Account: name,
				Field:   "daily_auto_limit",
				Action:  "changed",
				Impact:  ImpactExpands,
				Before:  before.DailyAutoLimit,
				After:   after.DailyAutoLimit,
				Reason:  "提高每日自动发送上限",
			})
		} else {
			changes = append(changes, PolicyChange{
				Account: name,
				Field:   "daily_auto_limit",
				Action:  "changed",
				Impact:  ImpactRestricts,
				Before:  before.DailyAutoLimit,
				After:   after.DailyAutoLimit,
				Reason:  "降低每日自动发送上限",
			})
		}
	}

	beforeBlack := normalizeSortedSet(before.SendBlacklist)
	afterBlack := normalizeSortedSet(after.SendBlacklist)
	beforeBlackMap := toSet(beforeBlack)
	afterBlackMap := toSet(afterBlack)

	for _, entry := range beforeBlack {
		if afterBlackMap[entry] {
			continue
		}
		impact := ImpactExpands
		reason := "移除自动发送黑名单条目（该地址仍在白名单中，开启 auto_send 时将不再强制回落人工 TTY 确认）"
		if !AllowlistMatches(entry, afterAllow) {
			impact = ImpactNeutral
			reason = "移除自动发送黑名单条目（该地址不在新白名单中，仍被白名单门禁拒发）"
		}
		changes = append(changes, PolicyChange{
			Account: name,
			Field:   "send_blacklist",
			Action:  "removed",
			Impact:  impact,
			Before:  entry,
			Reason:  reason,
		})
	}
	for _, entry := range afterBlack {
		if beforeBlackMap[entry] {
			continue
		}
		impact := ImpactRestricts
		reason := "新增自动发送黑名单条目（强制该地址回落人工 TTY 确认）"
		if !AllowlistMatches(entry, afterAllow) {
			impact = ImpactNeutral
			reason = "新增自动发送黑名单条目（该地址不在新白名单中，当前无额外限制效果）"
		}
		changes = append(changes, PolicyChange{
			Account: name,
			Field:   "send_blacklist",
			Action:  "added",
			Impact:  impact,
			After:   entry,
			Reason:  reason,
		})
	}

	return changes
}

// allowlistEntryCovered reports whether entry is already covered by allowlist.
// A wildcard entry is only covered by an identical wildcard; an exact address
// is covered by itself or a matching *@domain entry.
func allowlistEntryCovered(entry string, allowlist []string) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if strings.HasPrefix(entry, "*@") {
		for _, candidate := range allowlist {
			if strings.ToLower(strings.TrimSpace(candidate)) == entry {
				return true
			}
		}
		return false
	}
	return AllowlistMatches(entry, allowlist)
}

func accountSummaryMap(pol AccountPolicy) map[string]any {
	return map[string]any{
		"email":            pol.Email,
		"imap_endpoint":    fmt.Sprintf("%s:%d", pol.IMAPHost, pol.IMAPPort),
		"smtp_endpoint":    fmt.Sprintf("%s:%d", pol.SMTPHost, pol.SMTPPort),
		"send_allowlist":   pol.SendAllowlist,
		"send_blacklist":   pol.SendBlacklist,
		"auto_send":        pol.AutoSend,
		"daily_auto_limit": pol.DailyAutoLimit,
	}
}

func collectAccountNames(currentCfg, proposedCfg *Config, accountFilter string) []string {
	if accountFilter != "" {
		return []string{accountFilter}
	}
	seen := make(map[string]bool, len(currentCfg.Accounts)+len(proposedCfg.Accounts))
	names := make([]string, 0, len(currentCfg.Accounts)+len(proposedCfg.Accounts))
	for name := range currentCfg.Accounts {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for name := range proposedCfg.Accounts {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func normalizeSortedSet(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		norm := strings.ToLower(strings.TrimSpace(value))
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	sort.Strings(out)
	return out
}

func toSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}
