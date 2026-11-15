package vocab

import (
	"reflect"
	"testing"
)

func TestFromProtoMakesTheAdapterTheOwnerWhateverWasSent(t *testing.T) {
	t.Parallel()
	m := Core() // owner core, verified by build
	m.Suggests = []Mapping{{ID: "a", Subject: "x.y:A", Predicate: Broader, Object: "z.w:B", Status: Asserted, Author: "someone"}}
	got := FromProto(ToProto(m))
	want := m
	want.Owner, want.Source.VerifiedBy = OwnerAdapter, ""
	want.Suggests = []Mapping{{ID: "a", Subject: "x.y:A", Predicate: Broader, Object: "z.w:B", Status: Proposed}}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("got %+v, want %+v", *got, want)
	}
	if FromProto(nil) != nil {
		t.Error("got a module from nothing")
	}
}
