package cli

import (
	"context"
	"fmt"

	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/spf13/cobra"
)

// searchModeForField mirrors imapx.ServerSearchField to the meta.search_mode
// value: TEXT reports server_text, BODY reports server_body.
func searchModeForField() string {
	if imapx.ServerSearchField == "BODY" {
		return "server_body"
	}
	return "server_text"
}

// runServerSearch executes `search <query> --server` against the live IMAP
// mailbox. Security invariant: the query travels only inside the SEARCH
// command — never into logs, audit records or error messages, whose wording is
// fixed and does not echo the query.
func runServerSearch(rt *Runtime, cmd *cobra.Command, query string, limit int) error {
	ctx, cancel := rt.context()
	defer cancel()
	reader, named, err := rt.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Logout(context.Background()) }()
	_, uidValidity, err := reader.Examine(ctx, rt.Folder)
	if err != nil {
		return err
	}
	uids, err := reader.Search(ctx, imapx.SearchFilter{Text: query, Limit: limit})
	if err != nil {
		// A tagged NO/BAD is the server refusing the search criterion, not a
		// transient fault: report it as a policy denial so the agent switches
		// strategy instead of retrying (docs/compat/qq-20261006.md). Network-
		// class errors pass through unchanged to the existing classification.
		return imapx.WrapSearchReject(err)
	}
	envelopes, err := reader.FetchEnvelopes(ctx, rt.Folder, uidValidity, uids)
	if err != nil {
		return err
	}
	data := map[string]any{"envelopes": envelopes}
	if rt.JSON {
		return writeDetailed(rt, cmd, data, nil, output.Meta{Account: named.Name, SearchMode: searchModeForField()})
	}
	for _, envelope := range envelopes {
		from := ""
		if len(envelope.From) > 0 {
			from = envelope.From[0].Email
		}
		if _, err := fmt.Fprintf(rt.Out, "%10d  %-25s  %s\n", envelope.UID, output.SanitizeHuman(from), output.SanitizeHuman(envelope.Subject)); err != nil {
			return err
		}
	}
	return nil
}
