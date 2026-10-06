package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/policy"
	"github.com/spf13/cobra"
)

// refuseReservedFolderName rejects INBOX for every folder write verb. CREATE
// INBOX always fails on a compliant server, and RENAME INBOX is worse than a
// plain failure: RFC 3501 defines it as moving every message into the new
// folder. The guard is case-insensitive and covers both rename arguments, so
// the same semantics cannot be smuggled in through the new-name position.
func refuseReservedFolderName(names ...string) error {
	for _, name := range names {
		if strings.EqualFold(name, "INBOX") {
			return &errmap.Error{Kind: errmap.Usage, Message: "INBOX 是保留邮箱名，folder create/rename 不允许使用（RENAME INBOX 会把全部邮件移动到新文件夹）", Suggestion: "选择其他文件夹名"}
		}
	}
	return nil
}

func newFolderCreateCommand(rt *Runtime) *cobra.Command {
	var execute bool
	cmd := &cobra.Command{Use: "create <name>", Args: cobra.ExactArgs(1), Short: "Create a mail folder; dry-run unless --execute is confirmed"}
	cmd.Flags().BoolVar(&execute, "execute", false, "perform after TTY count confirmation")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := refuseReservedFolderName(name); err != nil {
			return err
		}
		if !execute {
			return writeMutationDryRun(rt, cmd, 1, map[string]any{"action": "folder_create", "name": name})
		}
		if err := policy.RequireMutationAllowed(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(rt.Err, "将创建文件夹 %s。\n", output.SanitizeHuman(name))
		if err := confirmExactCount(rt, 1); err != nil {
			return err
		}
		ctx, cancel := rt.context()
		defer cancel()
		mutator, named, err := rt.connectMutator(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = mutator.Logout(context.Background()) }()
		store, err := rt.IndexOpen(named.Name, true)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		service := policy.New(mutator, store)
		if err := service.CreateFolder(ctx, name, commandName(cmd)); err != nil {
			return err
		}
		return writeMutationResult(rt, cmd, named.Name, 1, []string{name}, nil, map[string]any{"action": "folder_create", "name": name})
	}
	return cmd
}

func newFolderRenameCommand(rt *Runtime) *cobra.Command {
	var execute bool
	cmd := &cobra.Command{Use: "rename <old> <new>", Args: cobra.ExactArgs(2), Short: "Rename a mail folder; dry-run unless --execute is confirmed"}
	cmd.Flags().BoolVar(&execute, "execute", false, "perform after TTY count confirmation")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		oldName, newName := args[0], args[1]
		if err := refuseReservedFolderName(oldName, newName); err != nil {
			return err
		}
		if !execute {
			return writeMutationDryRun(rt, cmd, 1, map[string]any{"action": "folder_rename", "old": oldName, "new": newName})
		}
		if err := policy.RequireMutationAllowed(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(rt.Err, "将把文件夹 %s 重命名为 %s。\n", output.SanitizeHuman(oldName), output.SanitizeHuman(newName))
		if err := confirmExactCount(rt, 1); err != nil {
			return err
		}
		ctx, cancel := rt.context()
		defer cancel()
		mutator, named, err := rt.connectMutator(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = mutator.Logout(context.Background()) }()
		store, err := rt.IndexOpen(named.Name, true)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		service := policy.New(mutator, store)
		if err := service.RenameFolder(ctx, oldName, newName, commandName(cmd)); err != nil {
			return err
		}
		return writeMutationResult(rt, cmd, named.Name, 1, []string{newName}, nil, map[string]any{"action": "folder_rename", "old": oldName, "new": newName})
	}
	return cmd
}
