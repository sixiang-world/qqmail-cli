package cli

import (
    "encoding/json"
    "testing"

    "github.com/situker/qqmail-cli/internal/imapx"
)

func TestServerSearchPagination(t *testing.T) {
    reader := &envelopeFilterReader{}
    out := runCLIReader(t, reader, "search", "发票", "--server", "--before-uid", "99", "--json")
    if len(reader.filters) != 1 || reader.filters[0].BeforeUID != 99 || reader.filters[0].Text != "发票" {
        t.Fatalf("missing server search filter: %+v", reader.filters)
    }
    var result struct {
        Data struct {
            Page struct {
                NextBeforeUID uint32 `json:"next_before_uid"`
            } `json:"page"`
        } `json:"data"`
    }
    if err := json.Unmarshal([]byte(out), &result); err != nil { t.Fatal(err) }
    if result.Data.Page.NextBeforeUID != 1 { t.Fatalf("unexpected cursor: %+v", result.Data.Page) }
    _ = imapx.SearchFilter{}
}

func TestLocalSearchRejectsServerCursor(t *testing.T) {
    runExit(t, 2, "search", "invoice", "--local", "--before-uid", "4", "--json")
}
