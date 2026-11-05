package cli

import (
    "context"
    "encoding/json"
    "testing"

    "github.com/situker/qqmail-cli/internal/imapx"
)

type resumableWatchReader struct {
    watchReader
    searches []imapx.SearchFilter
}

func (r *resumableWatchReader) Search(_ context.Context, filter imapx.SearchFilter) ([]uint32, error) {
    r.searches = append(r.searches, filter)
    return []uint32{6}, nil
}

func TestWatchExplicitResumeCheckpoint(t *testing.T) {
    reader := &resumableWatchReader{}
    out := runCLIReader(t, reader, "watch", "--once", "--jsonl", "--resume-uid", "5", "--resume-uidvalidity", "10")
    if len(reader.searches)!=1 || reader.searches[0].AfterUID != 5 {
        t.Fatalf("resume cursor was not used: %+v", reader.searches)
    }
    var event struct {
        Event string `json:"event"`
        Envelope struct { UID uint32 `json:"uid"` } `json:"envelope"`
    }
    if err:=json.Unmarshal([]byte(out),&event);err!=nil {t.Fatal(err)}
    if event.Event!="new_message" || event.Envelope.UID!=6 {t.Fatalf("wrong event: %+v",event)}
}

func TestWatchResumeRequiresBothCoordinates(t *testing.T) {
    runExit(t,2,"watch","--jsonl","--once","--resume-uid","5")
    runExit(t,2,"watch","--jsonl","--once","--resume-uidvalidity","10")
}
