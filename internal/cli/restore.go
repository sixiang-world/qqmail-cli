package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/situker/qqmail-cli/internal/cleaner"
	"github.com/situker/qqmail-cli/internal/cleanupplan"
	"github.com/situker/qqmail-cli/internal/errmap"
	exporter "github.com/situker/qqmail-cli/internal/export"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/policy"
	"github.com/spf13/cobra"
)

// restore is the deliberate inverse of clean: driven by the same plan and the
// same verified backup manifest, it finds each message in the server trash by
// Message-ID + RFC822.SIZE and moves it back to its original folder. It is the
// regret window for an over-eager cleanup — as long as the QQ trash auto-purge
// cycle has not emptied the copy yet.
func newRestoreCommand(rt *Runtime) *cobra.Command {
	var planPath string
	var execute bool
	cmd := &cobra.Command{Use: "restore", Short: "Move cleaned messages back out of the server trash using the same plan", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&planPath, "plan", "", "required plan.json that was previously backed up and cleaned")
	cmd.Flags().BoolVar(&execute, "execute", false, "perform the restores after TTY count confirmation")
	_ = cmd.MarkFlagRequired("plan")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if execute {
			if err := policy.RequireMutationAllowed(); err != nil {
				return err
			}
		}
		plan, err := cleanupplan.Load(planPath)
		if err != nil {
			return &errmap.Error{Kind: errmap.ParseError, Message: "清理计划校验失败", Cause: err}
		}
		if strings.TrimSpace(plan.BackupRoot) == "" {
			return &errmap.Error{Kind: errmap.Usage, Message: "计划没有 backup_root；restore 依赖 backup 产出的 manifest 提供 Message-ID"}
		}
		if len(plan.Items) == 0 {
			return &errmap.Error{Kind: errmap.Usage, Message: "清理计划为空，没有可还原的邮件"}
		}
		ctx, cancel := rt.context()
		mutator, named, err := rt.connectMutator(ctx)
		if err != nil {
			cancel()
			return err
		}
		defer func() { _ = mutator.Logout(context.Background()) }()
		manifest, _, err := exporter.LoadVerified(plan.BackupRoot, named, rt.Secrets)
		if err != nil {
			cancel()
			return &errmap.Error{Kind: errmap.PolicyDenied, Message: "备份 manifest 校验失败，拒绝还原", Cause: err}
		}
		if manifest.Account != named.Name {
			cancel()
			return &errmap.Error{Kind: errmap.PolicyDenied, Message: "manifest 与当前账号不匹配"}
		}
		byID := map[string]exporter.Entry{}
		for _, entry := range manifest.Messages {
			byID[entry.ID] = entry
		}
		trash, err := cleaner.TrashFolder(ctx, mutator)
		if err != nil {
			cancel()
			return &errmap.Error{Kind: errmap.NotFound, Message: "无法确定服务器已删除文件夹", Cause: err}
		}
		type located struct {
			planID   string
			trashID  mailmodel.MsgID
			folder   string
			identity imapx.MessageIdentity
			match    string
		}
		found := []located{}
		notFound := []map[string]any{}
		warnings := []output.Warning{}
		// One batched trash scan for the whole plan instead of a per-message
		// EXAMINE + SEARCH + fetch storm: QQ rejects dense per-message traffic
		// non-deterministically (docs/compat/qq-20260902.md §4a), and restore
		// is the regret path — it must not fail for the same reason clean did.
		scan, err := scanTrash(ctx, mutator, trash)
		if err != nil {
			cancel()
			return err
		}
		for _, item := range plan.Items {
			original, parseErr := mailmodel.ParseMsgID(item.ID)
			if parseErr != nil {
				warnings = append(warnings, output.Warning{Code: "usage", Message: "计划条目 id 无法解析", ID: item.ID})
				continue
			}
			entry, ok := byID[item.ID]
			if !ok {
				notFound = append(notFound, map[string]any{"id": item.ID, "reason": "manifest 中没有该邮件的备份记录"})
				continue
			}
			trashID, match, hit, missReason := locateInScan(ctx, mutator, scan, trash, entry)
			if !hit {
				notFound = append(notFound, map[string]any{"id": item.ID, "reason": missReason})
				continue
			}
			identity := imapx.MessageIdentity{MessageID: entry.MessageID, SizeBytes: entry.Size, SHA256: entry.SHA256}
			found = append(found, located{planID: item.ID, trashID: trashID, folder: original.Folder, identity: identity, match: match})
		}
		cancel()
		locatedData := make([]map[string]any, 0, len(found))
		for _, item := range found {
			locatedData = append(locatedData, map[string]any{"id": item.planID, "trash_id": item.trashID.String(), "restore_to": item.folder, "match": item.match})
		}
		if !execute {
			data := map[string]any{"dry_run": true, "execute": false, "requested": len(plan.Items), "located": locatedData, "not_found": notFound, "trash_folder": trash}
			if rt.JSON {
				return writeDetailed(rt, cmd, data, warnings, output.Meta{Account: named.Name})
			}
			rt.notePartial(warnings)
			_, err = fmt.Fprintf(rt.Out, "DRY RUN：%d/%d 封可从 %s 还原；未执行任何写操作。\n", len(found), len(plan.Items), output.SanitizeLine(trash))
			return err
		}
		if len(found) == 0 {
			data := map[string]any{"dry_run": false, "execute": true, "requested": len(plan.Items), "completed": 0, "results": []map[string]any{}, "not_found": notFound, "trash_folder": trash}
			return writeResult(rt, cmd, data, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "没有可还原的邮件（%d 封在已删除文件夹中未找到）。\n", len(notFound))
				return err
			})
		}
		_, _ = fmt.Fprintf(rt.Err, "将把 %d 封邮件从 %s 还原回原文件夹。\n", len(found), output.SanitizeLine(trash))
		if err := confirmExactCount(rt, len(found)); err != nil {
			return err
		}
		execCtx, cancelExec := rt.context()
		defer cancelExec()
		store, err := rt.IndexOpen(named.Name, true)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		service := policy.New(mutator, store)
		results := []map[string]any{}
		for _, item := range found {
			result, moveErr := service.Move(execCtx, item.trashID, item.folder, item.identity, commandName(cmd), planPath)
			if moveErr != nil {
				failure, _ := errmap.Details(moveErr)
				warnings = append(warnings, output.Warning{Code: failure.Code, Message: failure.Message, ID: item.planID, Retryable: failure.Retryable})
				continue
			}
			results = append(results, map[string]any{"id": item.planID, "restored_to": item.folder, "result": result})
		}
		data := map[string]any{"dry_run": false, "execute": true, "requested": len(plan.Items), "completed": len(results), "results": results, "not_found": notFound, "trash_folder": trash}
		if rt.JSON {
			return writeDetailed(rt, cmd, data, warnings, output.Meta{Account: named.Name, Skipped: len(warnings)})
		}
		rt.notePartial(warnings)
		_, err = fmt.Fprintf(rt.Out, "还原完成：%d/%d。\n", len(results), len(found))
		for _, warning := range warnings {
			_, _ = fmt.Fprintf(rt.Err, "失败 %s：%s\n", warning.ID, output.SanitizeLine(warning.Message))
		}
		return err
	}
	return cmd
}

// trashScan is the per-folder identity index one restore run resolves against:
// every UID in the trash with its RFC822.SIZE and Message-ID, fetched in
// bounded chunks from a single EXAMINE.
type trashScan struct {
	uidValidity uint32
	uids        []uint32 // ascending
	sizeByUID   map[uint32]int64
	headerByUID map[uint32]string // normalized Message-ID, empty when absent
	sizeIndex   map[int64][]uint32
}

const restoreFetchChunk = 500

func scanTrash(ctx context.Context, reader imapx.Reader, trash string) (*trashScan, error) {
	uidValidity, _, err := reader.Examine(ctx, trash)
	if err != nil {
		return nil, err
	}
	ids, err := reader.Search(ctx, imapx.SearchFilter{})
	if err != nil {
		return nil, err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	scan := &trashScan{uidValidity: uidValidity, uids: ids, sizeByUID: map[uint32]int64{}, headerByUID: map[uint32]string{}, sizeIndex: map[int64][]uint32{}}
	for _, chunk := range chunkRestoreUIDs(ids, restoreFetchChunk) {
		envelopes, err := reader.FetchEnvelopes(ctx, trash, uidValidity, chunk)
		if err != nil {
			return nil, err
		}
		for _, envelope := range envelopes {
			scan.sizeByUID[envelope.UID] = envelope.Size
			scan.sizeIndex[envelope.Size] = append(scan.sizeIndex[envelope.Size], envelope.UID)
		}
	}
	for _, chunk := range chunkRestoreUIDs(ids, restoreFetchChunk) {
		headers, err := reader.FetchHeaderFields(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for _, header := range headers {
			scan.headerByUID[header.UID] = normalizeMessageIDValue(header.MessageID)
		}
	}
	return scan, nil
}

// locateInScan resolves one verified manifest entry against the trash index.
// On a match it returns the trash id and the match method ("message_id" or
// "sha256") with found=true; otherwise found=false and missReason explains the
// miss for the review report.
func locateInScan(ctx context.Context, reader imapx.Reader, scan *trashScan, trash string, entry exporter.Entry) (trashID mailmodel.MsgID, match string, found bool, missReason string) {
	if normalizeMessageIDValue(entry.MessageID) != "" {
		want := normalizeMessageIDValue(entry.MessageID)
		for _, uid := range scan.sizeIndex[entry.Size] {
			if scan.headerByUID[uid] == want {
				return mailmodel.MsgID{Folder: trash, UIDValidity: scan.uidValidity, UID: uid}, "message_id", true, ""
			}
		}
		return mailmodel.MsgID{}, "", false, "已删除文件夹中找不到该邮件（按 Message-ID + 大小定位；可能已被服务器回收站周期清空；本地 .eml 备份仍在 backup_root）"
	}
	if entry.SHA256 != "" {
		// The clean gate admits Message-ID-less wild mail on
		// UIDVALIDITY+UID+RFC822.SIZE, so restore must re-identify it without
		// the header: exact-size candidates, confirmed by full-body SHA-256
		// against the verified backup. Identical bytes are interchangeable —
		// moving any match back is correct.
		candidates := scan.sizeIndex[entry.Size]
		if len(candidates) == 0 {
			return mailmodel.MsgID{}, "", false, "已删除文件夹中找不到该邮件（按大小定位无候选；可能已被服务器回收站周期清空；本地 .eml 备份仍在 backup_root）"
		}
		fetched := 0
		for _, uid := range candidates {
			if fetched >= restoreFingerprintCap {
				return mailmodel.MsgID{}, "", false, fmt.Sprintf("已删除文件夹中同尺寸候选超过 %d 封，已停止指纹比对；本地 .eml 备份仍在 backup_root", restoreFingerprintCap)
			}
			fetched++
			raw, truncated, err := reader.FetchBodyPeek(ctx, mailmodel.MsgID{Folder: trash, UIDValidity: scan.uidValidity, UID: uid}, imapx.MaxMessageBytes)
			if err != nil || truncated {
				continue
			}
			if strings.EqualFold(imapx.SHA256Hex(raw), entry.SHA256) {
				return mailmodel.MsgID{Folder: trash, UIDValidity: scan.uidValidity, UID: uid}, "sha256", true, ""
			}
		}
		return mailmodel.MsgID{}, "", false, "已删除文件夹中的同尺寸候选均与备份指纹不符；本地 .eml 备份仍在 backup_root"
	}
	return mailmodel.MsgID{}, "", false, "manifest 记录缺少 Message-ID 与 SHA-256，无法在已删除文件夹中定位"
}

const restoreFingerprintCap = 16

func chunkRestoreUIDs(uids []uint32, size int) [][]uint32 {
	var chunks [][]uint32
	for start := 0; start < len(uids); start += size {
		end := start + size
		if end > len(uids) {
			end = len(uids)
		}
		chunks = append(chunks, uids[start:end])
	}
	return chunks
}

func normalizeMessageIDValue(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "<>"))
}
