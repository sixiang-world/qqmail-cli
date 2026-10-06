package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
)

func TestSearchRequiresExactlyOneMode(t *testing.T) {
	runExit(t, 2, "search", "x", "--json")                        // 无模式 → usage
	runExit(t, 2, "search", "x", "--local", "--server", "--json") // 双模式 → usage
}

func TestSearchServerReturnsEnvelopes(t *testing.T) {
	// envelopeFilterReader：SEARCH 命中 1 个 UID，FETCH 返回 1 封合成信封
	out := runCLIReader(t, &envelopeFilterReader{}, "search", "发票", "--server", "--json")
	var doc struct {
		Command string `json:"command"`
		Data    struct {
			Envelopes []json.RawMessage `json:"envelopes"`
		} `json:"data"`
		Meta struct {
			SearchMode string `json:"search_mode"`
		} `json:"meta"`
	}
	mustUnmarshal(t, out, &doc)
	if doc.Command != "search" || len(doc.Data.Envelopes) != 1 {
		t.Fatalf("unexpected: %s / %d", doc.Command, len(doc.Data.Envelopes))
	}
	if doc.Meta.SearchMode != searchModeForField() { // TEXT→"server_text"，BODY→"server_body"
		t.Fatalf("search_mode = %q", doc.Meta.SearchMode)
	}
}

// serverRejectReader hands the command a terminal policy_denied rejection. CLI
// tests cannot construct *imap.Error (TestOnlyIMAPXImportsGoIMAP keeps go-imap
// inside internal/imapx), so the fake emits the already-mapped error that
// WrapSearchReject produces for a tagged BAD; the BAD→policy_denied
// discrimination itself is covered by the imapx wire and unit tests.
type serverRejectReader struct {
	fakeReader
	err error
}

func (r *serverRejectReader) Search(context.Context, imapx.SearchFilter) ([]uint32, error) {
	return nil, r.err
}

func TestSearchServerRejectsWhenServerRefuses(t *testing.T) {
	rejection := &errmap.Error{Kind: errmap.PolicyDenied,
		Message:    "服务器拒绝了这个搜索条件（IMAP SEARCH 方言记录见 docs/compat/ 目录，专项记录待探针落档）",
		Suggestion: "改用 --from/--subject 过滤，或 qqmail-cli sync 后 search --local"}
	_, err := tryRunCLI(t, &serverRejectReader{err: rejection}, []string{"search", "发票", "--server", "--json"})
	if err == nil {
		t.Fatal("server rejection unexpectedly succeeded")
	}
	// Details/Classify 就是 Execute 渲染进 JSON error.message/suggestion 的来源，
	// 对它们断言等价于对失败信封内容断言。
	out, code := errmap.Details(err)
	if code != 50 {
		t.Fatalf("exit code %d, want 50 (policy_denied): %v", code, err)
	}
	if !strings.Contains(out.Message, "docs/compat/") || !strings.Contains(out.Suggestion, "search --local") {
		t.Fatalf("rejection lost the doc reference or the local-search fallback: %+v", out)
	}
	// 安全不变量：查询词只进 SEARCH 命令，绝不回显在错误消息里
	if strings.Contains(out.Message+err.Error(), "发票") {
		t.Fatalf("query echoed into error message: %s", err)
	}
}

func mustUnmarshal(t *testing.T, raw string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, raw)
	}
}
