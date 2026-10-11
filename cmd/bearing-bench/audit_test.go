package main

import (
	"context"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
	"bearing.example/pkg/store"
)

// TestAuditEntriesAreWrittenAsRecords applies a Change with and without the
// audit entries the benchmark adds, and reads the log back: the store must
// accept the entries and write one record for each, a mint, a binding and a
// status change for each of the Change's five facts.
func TestAuditEntriesAreWrittenAsRecords(t *testing.T) {
	ctx := context.Background()
	cfg, err := resolverConfig(testDeclarations)
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := newChangeTemplate(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, store.Config{Graph: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(ctx) }()
	r, err := resolver.New(cfg, st.Graph)
	if err != nil {
		t.Fatal(err)
	}
	s := newStream(defaultOrg())
	s.people = []*personState{{id: 7, login: "user7", name: "Person 7", email: "user7@acme.example"}}
	at := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err := r.Apply(ctx, s.personEvent(s.people[0], at.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	person, err := st.Graph.ResolveKey(ctx, model.Key(userKey(7)), at, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		audit bool
		want  []modelv1alpha1.AuditAction
	}{
		{"off", false, nil},
		{"on", true, []modelv1alpha1.AuditAction{
			modelv1alpha1.AuditAction_AUDIT_ACTION_MINT,
			modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN,
			modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED,
			modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED,
			modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := st.Audit.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			n := 10 + len(tc.name)
			at = at.Add(time.Minute)
			ev := s.changeEvent(n, 0, at)
			cs, err := tpl.build(n, at, person.GetSubjectId(), "user7", ev.Observation.GetData())
			if err != nil {
				t.Fatal(err)
			}
			cs.EventId = ev.ID
			head, err := st.Graph.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			withBase(cs, head)
			w := &world{audit: tc.audit}
			if got, err := w.addAudit(cs); err != nil || got != len(tc.want) {
				t.Fatalf("addAudit: got %d, %v, want %d entries", got, err, len(tc.want))
			}
			if _, err := st.Graph.Apply(ctx, cs); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			recs, err := st.Audit.Query(ctx, contracts.AuditFilter{After: before.Seq, Limit: contracts.MaxAuditQueryRecords})
			if err != nil {
				t.Fatal(err)
			}
			var got []modelv1alpha1.AuditAction
			for _, rec := range recs {
				got = append(got, rec.GetEntry().GetAction())
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got actions %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("record %d: got %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
