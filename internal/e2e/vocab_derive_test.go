package e2e

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/vocab"
)

// syncWithoutOwners is vocabWorld.sync with the adapter's own owned_by
// relations dropped, so a derives rule is the only thing that says who owns
// a workload.
func syncWithoutOwners(v *vocabWorld) []*eventv1alpha1.Observation {
	v.t.Helper()
	var all []*eventv1alpha1.Observation
	cur := ""
	for range 100 {
		res, err := v.a.Sync(v.t.Context(), adapter.SyncParams{Config: v.cfg, Cursor: cur})
		if err != nil {
			v.t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if !res.Done {
			cur = res.NextCursor
			continue
		}
		for _, o := range all {
			d := o.GetData()
			d.Relations = slices.DeleteFunc(d.Relations, func(r *modelv1alpha1.Relation) bool { return r.GetType() == string(model.RelOwnedBy) })
			if err := model.ValidateObservation(v.reg, o); err != nil {
				v.t.Fatalf("%s: %v", o.GetId(), err)
			}
		}
		if _, err := v.store.Ingest(v.t.Context(), graph.IngestRequest{
			Source: "kubernetes-acme", Observations: all, Authoritative: authoritative(v.t, "kubernetes"),
		}); err != nil {
			v.t.Fatal(err)
		}
		return all
	}
	v.t.Fatal("sync did not finish")
	return nil
}

func teamRule(version int, key string) vocab.Mapping {
	return vocab.Mapping{
		ID: "r-team", Version: version, Subject: "io.k8s:Deployment", Predicate: vocab.Derives,
		Object: vocab.CoreNamespace + ":owned_by", Status: vocab.Asserted,
		Target: &vocab.Target{Kind: vocab.CoreNamespace + ":Team", Key: key},
	}
}

// TestVocabDeriveTeamLabel is ADR 13's derives rule on Acme: with the
// adapter's own owner relation off, a rule over the team label makes
// payments-api owned by payments, the evidence is the observation and the
// rule, and a second version of the rule supports the same fact.
func TestVocabDeriveTeamLabel(t *testing.T) {
	t.Parallel()
	v := newVocabWorld(t, nil)
	v.boot()
	obs := syncWithoutOwners(v)
	ctx := t.Context()

	if _, err := vocab.Compile([]vocab.Mapping{teamRule(1, `kubernetes:team/{{ .labels.team | lower `)}); !errors.Is(err, vocab.ErrMapping) || !strings.Contains(err.Error(), "r-team@1") {
		t.Fatalf("bad template: got %v, want an error naming r-team@1", err)
	}

	r1, err := vocab.Compile([]vocab.Mapping{teamRule(1, `kubernetes:team/{{ .labels.team | lower }}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.store.Derive(ctx, "kubernetes-acme", obs, r1); err != nil {
		t.Fatal(err)
	}
	o, err := v.q.Owners(ctx, paymentsWorkload, v.clock.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Owners) != 1 || o.Owners[0].Object == nil || o.Owners[0].Object.Name != "payments" {
		t.Fatalf("owners = %+v, want the payments team", o.Owners)
	}
	if s := o.Owners[0].Sources; len(s) != 1 || s[0].ObservationID == "" {
		t.Errorf("evidence = %+v, want the observation", s)
	}
	counts, err := v.store.RuleCounts(ctx)
	if err != nil || counts["r-team@1"] == 0 || counts["r-team@2"] != 0 {
		t.Fatalf("rule counts = %v, %v; want r-team@1 rows", counts, err)
	}
	before := ownedBy(t, v)

	// No label, no fact: every workload with a team label has one fact.
	labelled := 0
	for _, o := range obs {
		if e := o.GetData().GetEntity(); e.GetKind() == "io.k8s:Deployment" && e.GetAttributes()["labels"].GetStructValue().GetFields()["team"] != nil {
			labelled++
		}
	}
	if labelled == 0 || len(before) != labelled {
		t.Errorf("%d owned_by facts for %d labelled deployments", len(before), labelled)
	}

	r2, err := vocab.Compile([]vocab.Mapping{teamRule(2, `kubernetes:team/{{ .labels.team | trim | lower }}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := v.store.Derive(ctx, "kubernetes-acme", obs, r2)
	if err != nil || res.Opened != 0 || res.Closed != 0 || res.Supported != labelled {
		t.Fatalf("derive v2 = %+v, %v; want %d supported, none opened or closed", res, err, labelled)
	}
	after := ownedBy(t, v)
	if !maps.EqualFunc(before, after, func(a, b graph.Fact) bool {
		return a.ID == b.ID && a.ValidFrom.Equal(b.ValidFrom) && a.ValidTo.IsZero() && b.ValidTo.IsZero()
	}) {
		t.Errorf("facts changed: before %v, after %v", before, after)
	}
	counts, err = v.store.RuleCounts(ctx)
	if err != nil || counts["r-team@1"] != labelled || counts["r-team@2"] != labelled {
		t.Errorf("rule counts = %v, %v; want %d rows each for r-team@1 and r-team@2", counts, err, labelled)
	}

	// A version that makes a different fact closes what only the old ones made.
	r3, err := vocab.Compile([]vocab.Mapping{teamRule(3, `kubernetes:team/other`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err = v.store.Derive(ctx, "kubernetes-acme", obs, r3)
	if err != nil || res.Opened != labelled+1 || res.Closed != labelled {
		// The key has no label field, so the unlabelled deployment gets a fact too.
		t.Fatalf("derive v3 = %+v, %v; want %d opened and %d closed", res, err, labelled+1, labelled)
	}
}

func ownedBy(t *testing.T, v *vocabWorld) map[string]graph.Fact {
	t.Helper()
	fs, err := v.store.Facts(t.Context(), graph.FactFilter{Predicate: string(model.RelOwnedBy), At: v.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]graph.Fact{}
	for _, f := range fs {
		out[string(f.Subject)] = f
	}
	return out
}
