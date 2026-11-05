package cli

import (
    "os"
    "sync"
    "testing"
    "time"

    "github.com/situker/qqmail-cli/internal/account"
    "github.com/situker/qqmail-cli/internal/errmap"
)

func TestAutoSendCorruptStateFailsClosed(t *testing.T) {
    path := withAutoSendStateDir(t)
    if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil { t.Fatal(err) }
    named := account.Named{AutoSend: true, DailyAutoLimit: 3}
    eligible, err := autoSendEligible(named, []string{"allowed@example.com"}, time.Now())
    if eligible || err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
        t.Fatalf("corrupt state must deny autosend: eligible=%v err=%v", eligible, err)
    }
    if err := reserveAutoSend(named, time.Now()); err == nil {
        t.Fatal("reservation must fail closed on corrupt state")
    }
}

func TestAutoSendReservationsAreAtomic(t *testing.T) {
    withAutoSendStateDir(t)
    const requests, limit = 32, 7
    named := account.Named{AutoSend: true, DailyAutoLimit: limit}
    now := time.Now()
    var wg sync.WaitGroup
    outcomes := make(chan error, requests)
    for i := 0; i < requests; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            outcomes <- reserveAutoSend(named, now)
        }()
    }
    wg.Wait()
    close(outcomes)
    successes := 0
    for err := range outcomes {
        if err == nil {
            successes++
        } else if errmap.Classify(err).Kind != errmap.PolicyDenied {
            t.Fatalf("unexpected reservation error: %v", err)
        }
    }
    if successes != limit { t.Fatalf("reserved %d, want %d", successes, limit) }
    state, err := readAutoSendState(now)
    if err != nil || state.Count != limit { t.Fatalf("counter=%+v err=%v", state, err) }
}

func TestAutoSendFutureCounterDateFailsClosed(t *testing.T) {
	path := withAutoSendStateDir(t)
	now := time.Date(2026, 11, 6, 23, 59, 0, 0, time.Local)
	raw := []byte(`{"date":"2026-11-07","count":1}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil { t.Fatal(err) }
	named := account.Named{AutoSend:true,DailyAutoLimit:3}
	if err := reserveAutoSend(named,now);err==nil || errmap.Classify(err).Kind!=errmap.PolicyDenied {
		t.Fatalf("clock rollback must deny unattended send: %v",err)
	}
	if got,err:=os.ReadFile(path);err!=nil || string(got)!=string(raw) {
		t.Fatalf("future-dated counter overwritten: %q %v",got,err)
	}
}

func TestAutoSendSharedGlobalCounterAcrossAccounts(t *testing.T) {
	withAutoSendStateDir(t)
	now:=time.Now()
	first:=account.Named{AutoSend:true,DailyAutoLimit:2,Email:"one@qq.com"}
	second:=account.Named{AutoSend:true,DailyAutoLimit:2,Email:"two@qq.com"}
	if err:=reserveAutoSend(first,now);err!=nil{t.Fatal(err)}
	if err:=reserveAutoSend(second,now);err!=nil{t.Fatal(err)}
	if err:=reserveAutoSend(first,now);err==nil{
		t.Fatal("shared global daily cap bypassed using another account")
	}
}
