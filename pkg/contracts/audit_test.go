package contracts

import (
	"errors"
	"strings"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestCheckAuditFilter(t *testing.T) {
	ok := AuditFilter{Limit: 10}
	t0 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for name, f := range map[string]AuditFilter{
		"a full filter": {
			After: 5, Limit: MaxAuditQueryRecords, EventID: "github-acme/1", ActorID: "system:resolver",
			Actions:    []modelv1alpha1.AuditAction{modelv1alpha1.AuditAction_AUDIT_ACTION_MINT},
			TargetKind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, TargetID: "x", From: t0, To: t0.Add(time.Second),
		},
		"a target kind alone": {Limit: 1, TargetKind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS},
		"only a from":         {Limit: 1, From: t0},
	} {
		if err := CheckAuditFilter(f); err != nil {
			t.Errorf("%s: got %v, want it accepted", name, err)
		}
	}
	bad := map[string]AuditFilter{
		"no limit":           {},
		"a negative limit":   {Limit: -1},
		"too big a limit":    {Limit: MaxAuditQueryRecords + 1},
		"an unset action":    {Limit: 1, Actions: []modelv1alpha1.AuditAction{0}},
		"an unknown action":  {Limit: 1, Actions: []modelv1alpha1.AuditAction{999}},
		"an unknown kind":    {Limit: 1, TargetKind: 999},
		"an ID without kind": {Limit: 1, TargetID: "x"},
		"a long event ID":    {Limit: 1, EventID: strings.Repeat("e", MaxEventIDBytes+1)},
		"a long actor ID":    {Limit: 1, ActorID: strings.Repeat("a", MaxAuditIDBytes+1)},
		"a long target ID":   {Limit: 1, TargetKind: 1, TargetID: strings.Repeat("a", MaxAuditIDBytes+1)},
		"from after to":      {Limit: 1, From: t0.Add(time.Second), To: t0},
		"from equal to":      {Limit: 1, From: t0, To: t0},
		"too many actions":   {Limit: 1, Actions: make([]modelv1alpha1.AuditAction, len(modelv1alpha1.AuditAction_name)+1)},
	}
	for i := range bad["too many actions"].Actions {
		bad["too many actions"].Actions[i] = 1
	}
	for name, f := range bad {
		if err := CheckAuditFilter(f); !errors.Is(err, ErrInvalidAuditQuery) {
			t.Errorf("%s: got %v, want ErrInvalidAuditQuery", name, err)
		}
	}
	if err := CheckAuditFilter(ok); err != nil {
		t.Error(err)
	}
}

func TestCheckTraceID(t *testing.T) {
	for _, id := range []string{"", "0af7651916cd43dd8448eb211c80319c", "00000000000000000000000000000001"} {
		if err := CheckTraceID(id); err != nil {
			t.Errorf("%q: got %v, want it accepted", id, err)
		}
	}
	for _, id := range []string{
		strings.Repeat("0", 32), "0AF7651916CD43DD8448EB211C80319C", "0af7651916cd43dd8448eb211c80319", "0af7651916cd43dd8448eb211c80319cc",
		"0af7651916cd43dd8448eb211c80319g", "0af7651916cd43dd8448eb211c80319\n", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	} {
		if err := CheckTraceID(id); err == nil {
			t.Errorf("%q: got no error, want one", id)
		}
	}
}
