package e2e

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/fakes"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/vocab"
)

var (
	removesPred = vocab.Full(fakes.OperatorNamespace, "removes")
	// retireRule is the operator's guess: a merged pull request that deletes
	// a manifest removes the workload it declared, in Acme's one cluster.
	// shortcut: the first manifest only, and the cluster is fixed in the
	// template; a PR deleting several, or a repo deploying to several
	// clusters, needs one fact per manifest and a cluster per repo.
	retireRule = guess("r-retire", vocab.Full(fakes.GitHubNamespace, "PullRequest"), removesPred, deployKind,
		`{{ range $i, $m := .removed_manifests }}{{ if eq $i 0 }}kubernetes:workload/`+fakes.ClusterID+`/{{ $m.namespace }}/{{ $m.kind | lower }}/{{ $m.name }}{{ end }}{{ end }}`)
	retirer = duckstore.Retire{Rule: "p-retire@1", Removes: removesPred, At: vocab.Full(fakes.GitHubNamespace, "merged_at")}
)

// retireWorld is Acme's cluster synced with r-team, the GitHub module and
// the guess rule booted, and t0 the sync's time.
type retireWorld struct {
	*vocabWorld
	t0 time.Time
}

func newRetireWorld(t *testing.T) *retireWorld {
	t.Helper()
	v := newVocabWorld(t, nil)
	v.Modules = append(v.Modules, fakes.GitHubModule(), fakes.RetireOperator())
	v.Sources = map[string]string{fakes.GitHubNamespace: fakes.GitHubSource}
	v.Settings = []vocab.Mapping{teamRule(1, teamKey), retireRule}
	v.boot()
	w := &retireWorld{vocabWorld: v, t0: v.clock.Now()}
	v.sync()
	return w
}

// pr ingests the pull request merged at merged, observed now.
func (w *retireWorld) pr(merged time.Time) string {
	w.t.Helper()
	o := fakes.RetirePR(w.clock.Now(), merged)
	if err := model.ValidateObservation(w.reg, o); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.store.Ingest(w.t.Context(), graph.IngestRequest{Source: fakes.GitHubSource, Observations: []*eventv1alpha1.Observation{o}}); err != nil {
		w.t.Fatal(err)
	}
	return o.GetId()
}

func (w *retireWorld) propose(want duckstore.RetireResult) {
	w.t.Helper()
	got, err := w.store.ProposeCloses(w.t.Context(), retirer)
	if err != nil {
		w.t.Fatal(err)
	}
	if got != want {
		w.t.Errorf("propose closes = %+v, want %+v", got, want)
	}
}

// owned is payments-api's one owned_by fact at t0.
func (w *retireWorld) owned() graph.Fact {
	w.t.Helper()
	fs := ownedAt(w.vocabWorld, paymentsWorkload, w.t0)
	if len(fs) != 1 {
		w.t.Fatalf("%d owned_by facts at t0, want 1", len(fs))
	}
	return fs[0]
}

// close is the fact's one proposed close.
func (w *retireWorld) close(fact string) duckstore.Close {
	w.t.Helper()
	cs, err := w.store.Closes(w.t.Context(), fact)
	if err != nil {
		w.t.Fatal(err)
	}
	if len(cs) != 1 {
		w.t.Fatalf("closes = %+v, want one", cs)
	}
	return cs[0]
}

// owner is the asserted answer to "who owns payments-api" now, "" for none.
func (w *retireWorld) owner() string {
	w.t.Helper()
	o, err := w.q.Owners(w.t.Context(), paymentsWorkload, w.clock.Now(), false)
	if err != nil {
		w.t.Fatal(err)
	}
	if len(o.Owners) == 0 || o.Owners[0].Object == nil {
		return ""
	}
	return o.Owners[0].Object.Name
}

// TestVocabCloseFromPullRequest is ADR 13's inferred close on Acme: a merged
// pull request that deletes payments-api's manifest only proposes closing its
// rule-made owned_by fact. The asserted answer holds until a person confirms;
// a rejection is final for that evidence; and the Kubernetes adapter still
// reporting the workload after the merge contradicts the proposal.
func TestVocabCloseFromPullRequest(t *testing.T) {
	t.Parallel()

	t.Run("confirm closes the fact at the merge", func(t *testing.T) {
		t.Parallel()
		w := newRetireWorld(t)
		merged := w.t0.Add(30 * time.Minute)
		w.clock.Advance(time.Hour)
		prObs := w.pr(merged)
		w.propose(duckstore.RetireResult{Proposed: 1})

		// Unconfirmed: the deciding answer is unchanged, and the proposal shows
		// with its evidence path: the PR's observation and the guess rule.
		if got := w.owner(); got != "payments" {
			t.Fatalf("owner = %q, want payments until confirmed", got)
		}
		f := w.owned()
		c := w.close(f.ID)
		if c.Status != duckstore.CloseProposed || c.CauseKey != fakes.RetirePRKey || !c.At.Equal(merged) || c.Rule != retirer.Rule {
			t.Errorf("close = %+v, want proposed by %s at %s from %s", c, retirer.Rule, merged, fakes.RetirePRKey)
		}
		if want := []vocab.Cite{{Observation: prObs, Rule: "r-retire@1"}}; !slices.Equal(c.Cites, want) {
			t.Errorf("evidence = %+v, want %+v", c.Cites, want)
		}
		if want := "probably closed: " + fakes.RetirePRKey + " removed it at "; !strings.HasPrefix(c.Note(), want) || !strings.Contains(c.Note(), "unconfirmed") {
			t.Errorf("note = %q, want %q… unconfirmed", c.Note(), want)
		}
		if removes, err := w.store.Facts(t.Context(), graph.FactFilter{Predicate: removesPred}); err != nil || len(removes) != 0 {
			t.Errorf("asserted removes = %v, %v; want none: the rule only guesses", removes, err)
		}

		if err := w.store.DecideClose(t.Context(), duckstore.CloseDecision{ID: "d-1", Close: c.ID, Confirm: true}); !errors.Is(err, graph.ErrInvalidDecision) {
			t.Errorf("decision without an actor = %v, want ErrInvalidDecision", err)
		}
		d := duckstore.CloseDecision{ID: "d-1", Close: c.ID, Confirm: true, Actor: "doug", Reason: "payments-api was retired in PR 42"}
		if err := w.store.DecideClose(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		if got := w.owner(); got != "" {
			t.Errorf("owner after confirm = %q, want none", got)
		}
		if got := w.owned(); got.ID != f.ID || !got.ValidTo.Equal(merged) {
			t.Errorf("fact = %+v, want %s closed at the merge, %s", got, f.ID, merged)
		}
		c = w.close(f.ID)
		if c.Status != duckstore.CloseAsserted || c.Decision != "d-1" || c.Actor != "doug" || c.Reason != d.Reason || c.ValidTo == nil {
			t.Errorf("decided close = %+v, want asserted by doug with the reason", c)
		}
		// A replay changes nothing; a new decision on a decided close is refused.
		if err := w.store.DecideClose(t.Context(), d); err != nil {
			t.Errorf("replay = %v, want nil", err)
		}
		d.ID = "d-2"
		if err := w.store.DecideClose(t.Context(), d); !errors.Is(err, graph.ErrInvalidDecision) {
			t.Errorf("second decision = %v, want ErrInvalidDecision", err)
		}
	})

	t.Run("reject keeps the fact and the same PR does not propose again", func(t *testing.T) {
		t.Parallel()
		w := newRetireWorld(t)
		w.clock.Advance(time.Hour)
		w.pr(w.t0.Add(30 * time.Minute))
		w.propose(duckstore.RetireResult{Proposed: 1})
		f := w.owned()
		c := w.close(f.ID)
		if err := w.store.DecideClose(t.Context(), duckstore.CloseDecision{ID: "d-1", Close: c.ID, Actor: "doug", Reason: "the PR moved the manifest"}); err != nil {
			t.Fatal(err)
		}
		if got := w.owner(); got != "payments" {
			t.Errorf("owner after reject = %q, want payments", got)
		}
		// GitHub reports the PR again on a later sync: a new observation of the
		// same evidence.
		w.clock.Advance(time.Hour)
		w.pr(w.t0.Add(30 * time.Minute))
		w.propose(duckstore.RetireResult{})
		if c = w.close(f.ID); c.Status != duckstore.CloseRejected || c.Actor != "doug" {
			t.Errorf("close = %+v, want rejected by doug", c)
		}
		if got := w.owned(); !got.ValidTo.IsZero() {
			t.Errorf("fact = %+v, want open", got)
		}
	})

	// The adapter's own observation after the merge is direct evidence the
	// workload still runs; the PR is only an inference, so it loses.
	t.Run("the cluster still reporting the workload contradicts it", func(t *testing.T) {
		t.Parallel()
		w := newRetireWorld(t)
		merged := w.t0.Add(30 * time.Minute)
		w.clock.Advance(time.Hour)
		w.pr(merged)
		w.propose(duckstore.RetireResult{Proposed: 1})
		f := w.owned()
		c := w.close(f.ID)

		w.clock.Advance(time.Hour)
		var seen string
		for _, o := range w.sync() {
			if o.GetData().GetEntity().GetKey() == paymentsWorkload {
				seen = o.GetId()
			}
		}
		// Confirming before the pass is refused all the same.
		if err := w.store.DecideClose(t.Context(), duckstore.CloseDecision{ID: "d-1", Close: c.ID, Confirm: true, Actor: "doug"}); !errors.Is(err, graph.ErrInvalidDecision) || !strings.Contains(err.Error(), seen) {
			t.Errorf("confirm = %v, want ErrInvalidDecision naming %s", err, seen)
		}
		w.propose(duckstore.RetireResult{Contradicted: 1})
		if c = w.close(f.ID); c.Status != duckstore.CloseContradicted || c.Against != seen {
			t.Errorf("close = %+v, want contradicted by %s", c, seen)
		}
		if got := w.owner(); got != "payments" {
			t.Errorf("owner = %q, want payments", got)
		}
	})

	t.Run("a sync after the merge, before the PR arrives, contradicts it at once", func(t *testing.T) {
		t.Parallel()
		w := newRetireWorld(t)
		w.clock.Advance(time.Hour)
		w.sync()
		w.pr(w.t0.Add(30 * time.Minute))
		w.propose(duckstore.RetireResult{Proposed: 1, Contradicted: 1})
		if c := w.close(w.owned().ID); c.Status != duckstore.CloseContradicted {
			t.Errorf("close = %+v, want contradicted", c)
		}
	})

	t.Run("a fact the adapter closed first supersedes the proposal", func(t *testing.T) {
		t.Parallel()
		w := newRetireWorld(t)
		w.clock.Advance(time.Hour)
		w.pr(w.t0.Add(30 * time.Minute))
		w.propose(duckstore.RetireResult{Proposed: 1})
		if err := w.cluster.cluster.DeleteWorkload(fakes.Ref{Kind: fakes.KindDeployment, Namespace: "payments", Name: "payments-api"}); err != nil {
			t.Fatal(err)
		}
		w.clock.Advance(time.Hour)
		w.sync()
		w.propose(duckstore.RetireResult{Superseded: 1})
		if c := w.close(w.owned().ID); c.Status != duckstore.CloseSuperseded {
			t.Errorf("close = %+v, want superseded", c)
		}
	})
}
