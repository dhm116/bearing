package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
	"bearing.example/pkg/store"
)

// TestAuditEntriesAreWrittenAsRecords applies a Change from the template with
// and without its audit entries, and reads the log back: the store must write
// one record for each entry, a mint, a binding and a status change for each of
// the Change's five facts, and none when the benchmark strips them.
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
		n     int
		audit bool
		want  []modelv1alpha1.AuditAction
	}{
		{"off", 11, false, nil},
		{"on", 12, true, []modelv1alpha1.AuditAction{
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
			at = at.Add(time.Minute)
			ev := s.changeEvent(tc.n, 0, at)
			cs, err := tpl.build(tc.n, at, person.GetSubjectId(), "user7", ev.Observation.GetData())
			if err != nil {
				t.Fatal(err)
			}
			cs.EventId = ev.ID
			head, err := st.Graph.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			withBase(cs, head)
			if len(cs.GetAudit()) != 7 {
				t.Fatalf("the template's ChangeSet has %d audit entries, want 7", len(cs.GetAudit()))
			}
			if got := stripAudit(cs, tc.audit); got != len(tc.want) {
				t.Fatalf("stripAudit: got %d entries, want %d", got, len(tc.want))
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

// TestAuditCostRunsWithTheLogOffAndOn runs the single consumer on the
// in-memory store and reads the results back: a run with the log off writes
// no records, a run with it on writes the template's seven for each Change.
func TestAuditCostRunsWithTheLogOffAndOn(t *testing.T) {
	var out, log bytes.Buffer
	var f flags
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	f.register(fs)
	if err := fs.Parse([]string{"--store", "mem://", "--declarations", testDeclarations, "--repos", "12", "--people", "8", "--teams", "3", "--changes-per-day", "6"}); err != nil {
		t.Fatal(err)
	}
	w, err := newWorld(context.Background(), f, &out, &log)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := w.load(loadOptions{Events: 120}); err != nil {
		t.Fatalf("load: %v\n%s", err, log.String())
	}
	out.Reset()
	if err := w.measureAudit("test", 0, suiteOptions{AuditRun: 200 * time.Millisecond, AuditRounds: 2}); err != nil {
		t.Fatalf("measureAudit: %v\n%s", err, log.String())
	}
	if !w.audit {
		t.Fatal("the world's audit setting changed during the runs")
	}
	var runs []auditRun
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var r struct {
			Kind string   `json:"kind"`
			Data auditRun `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("result line %q: %v", line, err)
		}
		if r.Kind == "apply_audit" {
			runs = append(runs, r.Data)
		}
	}
	if len(runs) != 4 {
		t.Fatalf("got %d runs, want 4 (two rounds of off and on)", len(runs))
	}
	for _, r := range runs {
		if r.Applied == 0 {
			t.Fatalf("run %+v applied nothing", r)
		}
		if !r.Audit && r.AuditRecords != 0 {
			t.Fatalf("log off: got %d records, want 0", r.AuditRecords)
		}
		if want := 7 * r.Applied; r.Audit && r.AuditRecords != want {
			t.Fatalf("log on: got %d records for %d applies, want %d", r.AuditRecords, r.Applied, want)
		}
	}
	if runs[0].Audit == runs[2].Audit {
		t.Fatalf("rounds 1 and 2 ran in the same order, want them alternated")
	}
}

// TestAuditWriterRefusesAnEmptyOrg checks that a store with nobody loaded is
// reported, not divided by.
func TestAuditWriterRefusesAnEmptyOrg(t *testing.T) {
	w := &world{ctx: context.Background(), stream: newStream(defaultOrg()), now: time.Now}
	if r := w.auditWriter(&readSet{}, time.Millisecond, true, 1, &extraSeq{}); r.Sample == "" || r.Applied != 0 {
		t.Fatalf("got %+v, want a sample error and nothing applied", r)
	}
}

// TestAuditCostFailsWhenARunDoes checks that a run that applied nothing, here
// because the context was cancelled, is an error and not a result.
func TestAuditCostFailsWhenARunDoes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, log bytes.Buffer
	var f flags
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	f.register(fs)
	if err := fs.Parse([]string{"--store", "mem://", "--declarations", testDeclarations, "--repos", "12", "--people", "8", "--teams", "3"}); err != nil {
		t.Fatal(err)
	}
	w, err := newWorld(ctx, f, &out, &log)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := w.load(loadOptions{Events: 60}); err != nil {
		t.Fatal(err)
	}
	rs, err := w.readSet(w.pastRecorded())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := w.auditCost("test", 0, rs, &extraSeq{}, time.Second, 1); err == nil {
		t.Fatal("got no error from a run that was cancelled, want one")
	}
}
