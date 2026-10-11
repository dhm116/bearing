package audit

import (
	"context"
	"strings"
	"testing"

	"bearing.example/pkg/contracts"
)

// headLog reports a head of its own choosing over a real chain.
type headLog struct {
	fakeLog
	head contracts.AuditHead
	err  error
}

func (l *headLog) Head(context.Context) (contracts.AuditHead, error) { return l.head, l.err }

func TestCheckedHeadReturnsTheHeadTheNewestRecordCarries(t *testing.T) {
	t.Parallel()
	l := chain(t, 3)
	head, err := CheckedHead(t.Context(), l)
	if err != nil || head.Seq != 3 || string(head.Hash) != string(l.recs[2].GetHash()) {
		t.Fatalf("got %+v, %v, want the head of record 3", head, err)
	}
	empty, err := CheckedHead(t.Context(), &fakeLog{})
	if err != nil || empty.Seq != 0 {
		t.Fatalf("got %+v, %v, want the zero head of an empty log", empty, err)
	}
}

func TestCheckedHeadRefusesAHeadNoRecordCarries(t *testing.T) {
	t.Parallel()
	good := chain(t, 3)
	flipped := append([]byte(nil), good.recs[2].GetHash()...)
	flipped[0] ^= 0xff
	edited := chain(t, 3)
	edited.recs[2].Entry.Reason = "edited after sealing"
	for name, tt := range map[string]struct {
		log  contracts.AuditLog
		want string
	}{
		"a head hash the newest record lacks":  {&headLog{fakeLog: *good, head: contracts.AuditHead{Seq: 3, Hash: flipped}}, "does not match its newest record"},
		"a head past the newest record":        {&headLog{fakeLog: *good, head: contracts.AuditHead{Seq: 4, Hash: good.recs[2].GetHash()}}, "does not match its newest record"},
		"a newest record edited after sealing": {edited, "does not match its hash"},
		"a head that cannot be read":           {&headLog{fakeLog: *good, err: context.DeadlineExceeded}, "deadline"},
		"records that cannot be read":          {&errLog{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := tt.log
			if e, ok := log.(*errLog); ok {
				e.recs = good.recs // Head works, Query fails
			}
			if _, err := CheckedHead(t.Context(), log); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
}
