package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/mailmodel"
)

type generationReader struct{ fakeReader }

func (generationReader) Examine(context.Context, string) (uint32, uint32, error) {
	return 777, 2, nil
}
func (generationReader) Search(_ context.Context, f imapx.SearchFilter) ([]uint32, error) {
	if f.BeforeUID == 1 { return []uint32{}, nil }
	uids := []uint32{42, 1}
	filtered := []uint32{}
	for _, uid := range uids {
		if f.BeforeUID > 0 && uid >= f.BeforeUID { continue }
		if f.AfterUID > 0 && uid <= f.AfterUID { continue }
		filtered = append(filtered, uid)
		if f.Limit > 0 && len(filtered) >= f.Limit { break }
	}
	return filtered, nil
}
func (generationReader) FetchEnvelopes(_ context.Context, folder string, validity uint32, uids []uint32) ([]mailmodel.Envelope,error) {
	result := []mailmodel.Envelope{}
	for _, uid := range uids {
		result = append(result, mailmodel.Envelope{ID:mailmodel.MsgID{Folder:folder,UIDValidity:validity,UID:uid}.String(),Folder:folder,UIDValidity:validity,UID:uid})
	}
	return result,nil
}
func (r generationReader) FetchBodyPeek(ctx context.Context, id mailmodel.MsgID, n int64) ([]byte,bool,error) {
	if id.UIDValidity != 777 { return nil,false,fmt.Errorf("stale_id: got generation %d",id.UIDValidity) }
	return r.fakeReader.FetchBodyPeek(ctx,id,n)
}

func TestServerSearchGenerationAndShowChain(t *testing.T) {
	reader := generationReader{}
	before := uint32(0)
	seen := map[uint32]bool{}
	for page:=0;page<3;page++ {
		args:=[]string{"search","receipt","--server","--json","--limit","1"}
		if before>0 {args=append(args,"--before-uid",strconv.FormatUint(uint64(before),10))}
		out := runCLIReader(t,reader,args...)
		var doc struct { Data struct {
			Envelopes []mailmodel.Envelope `json:"envelopes"`
			Page struct { Next uint32 `json:"next_before_uid"`; UIDValidity uint32 `json:"uidvalidity"` } `json:"page"`
		} `json:"data"` }
		if err:=json.Unmarshal([]byte(out),&doc);err!=nil{t.Fatal(err)}
		if doc.Data.Page.UIDValidity!=777 {t.Fatalf("search used message count as UIDVALIDITY: %s",out)}
		for _,envelope:=range doc.Data.Envelopes {
			if envelope.UIDValidity!=777 || seen[envelope.UID]{t.Fatalf("stale or duplicated envelope: %+v",envelope)}
			seen[envelope.UID]=true
			show:=runCLIReader(t,reader,"message","show",envelope.ID,"--json")
			var message struct { Data struct { Messages []json.RawMessage `json:"messages"` } `json:"data"` }
			if err:=json.Unmarshal([]byte(show),&message);err!=nil||len(message.Data.Messages)!=1 {
				t.Fatalf("search->show failed: %s %v",show,err)
			}
		}
		if doc.Data.Page.Next==0 {break}
		before=doc.Data.Page.Next
	}
	if !seen[42] || !seen[1] || len(seen)!=2 {t.Fatalf("pagination lost messages: %v",seen)}
	if out:=runCLIReader(t,reader,"search","receipt","--server","--json","--before-uid","1"); !strings.Contains(out,`"envelopes":[]`) {t.Fatalf("cursor exhausted: %s",out)}
}

func TestSearchRejectsStalePaginationGeneration(t *testing.T) {
	reader:=generationReader{}
	_,err:=tryRunCLI(t,reader,[]string{"search","receipt","--server","--json","--before-uid","42","--expected-uidvalidity","776"})
	if err==nil{t.Fatal("old UIDVALIDITY cursor accepted")}
	_,code:=errmap.Details(err)
	if code!=40{t.Fatalf("wrong stale cursor code: %d, %v",code,err)}
	_,err=tryRunCLI(t,reader,[]string{"envelope","list","--json","--expected-uidvalidity","776"})
	if err==nil{t.Fatal("old envelope cursor accepted")}
}
