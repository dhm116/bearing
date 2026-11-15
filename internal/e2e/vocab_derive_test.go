package e2e

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"bearing.example/internal/fakes"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/vocab"
)

// teamKeyTrimmed is r-team's second key template: the same teams, with the
// label's whitespace trimmed first.
const teamKeyTrimmed = "kubernetes:team/{{ .labels.team | trim | lower }}"

// ownedAt is the owned_by facts of workload that held at at.
func ownedAt(v *vocabWorld, workloadKey string, at time.Time) []graph.Fact {
	v.t.Helper()
	e, err := v.store.Entity(v.t.Context(), workloadKey)
	if err != nil {
		v.t.Fatalf("entity %s: %v", workloadKey, err)
	}
	fs, err := v.store.Facts(v.t.Context(), graph.FactFilter{Subject: e.ID, Predicate: "owned_by", At: at})
	if err != nil {
		v.t.Fatal(err)
	}
	return fs
}

// bootRules boots a world whose settings hold the given r-team versions.
func bootRules(t *testing.T, versions ...vocab.Mapping) *vocabWorld {
	t.Helper()
	v := newVocabWorld(t, nil)
	v.Settings = versions
	v.boot()
	return v
}

// TestVocabDeriveTeamLabel is ADR 13's derives rule on Acme: the adapter
// emits only labels, and r-team makes payments-api owned by its team, citing
// the observation and rule@version, and closing on its source's evidence.
func TestVocabDeriveTeamLabel(t *testing.T) {
	t.Parallel()
	v := bootRules(t, teamRule(1, teamKey))
	ctx := t.Context()
	t0 := v.clock.Now()
	v.sync()

	// r-team@1 answers payments, with the rule and the observation behind it.
	o, err := v.q.Owners(ctx, paymentsWorkload, v.clock.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Owners) != 1 || o.Owners[0].Object == nil || o.Owners[0].Object.Name != "payments" {
		t.Fatalf("owners = %+v, want the payments team", o.Owners)
	}
	if got, want := o.Owners[0].Rules, []string{"r-team@1"}; !slices.Equal(got, want) {
		t.Errorf("rules = %v, want %v", got, want)
	}
	if s := o.Owners[0].Sources; len(s) == 0 || s[0].Source != "kubernetes-acme" || s[0].ObservationID == "" {
		t.Errorf("sources = %+v, want a kubernetes-acme observation", s)
	}
	first := ownedAt(v, paymentsWorkload, v.clock.Now())
	if len(first) != 1 {
		t.Fatalf("%d owned_by facts, want 1", len(first))
	}

	// The same rule on a later sync keeps the fact: same id and valid_from,
	// nothing closed.
	v.clock.Advance(time.Hour)
	v.sync()
	assertSameFact(t, "same rule re-synced", first[0], ownedAt(v, paymentsWorkload, t0))
	if got := ownedAt(v, paymentsWorkload, v.clock.Now()); len(got) != 1 {
		t.Errorf("%d facts hold now, want 1", len(got))
	}

	// r-team@2 is a new mapping row. The foundation has no way to close v1's
	// row (DecideMapping closes proposals only), so both versions run and
	// both are behind the same fact.
	v.clock.Advance(time.Hour)
	v.Settings = append(v.Settings, teamRule(2, teamKeyTrimmed))
	v.boot()
	v.sync()
	assertSameFact(t, "r-team@2 added", first[0], ownedAt(v, paymentsWorkload, t0))
	o, err = v.q.Owners(ctx, paymentsWorkload, v.clock.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Owners) != 1 || !slices.Equal(o.Owners[0].Rules, []string{"r-team@1", "r-team@2"}) {
		t.Errorf("owners = %+v, want one fact made by r-team@1 and r-team@2", o.Owners)
	}
}

// assertSameFact checks that got is exactly the fact want was: the same id
// and valid_from, still open.
func assertSameFact(t *testing.T, when string, want graph.Fact, got []graph.Fact) {
	t.Helper()
	if len(got) != 1 || got[0].ID != want.ID || !got[0].ValidFrom.Equal(want.ValidFrom) || !got[0].ValidTo.IsZero() {
		t.Errorf("%s: facts = %+v, want %s from %s, open", when, got, want.ID, want.ValidFrom)
	}
}

// TestVocabDeriveTeamLabelChanges covers a rule that stops making a fact
// (the label moved to another team), a workload with no label, and a
// workload that is deleted.
func TestVocabDeriveTeamLabelChanges(t *testing.T) {
	t.Parallel()

	t.Run("a label that makes a different fact opens it and closes the old one", func(t *testing.T) {
		t.Parallel()
		v := bootRules(t, teamRule(1, teamKey))
		t0 := v.clock.Now()
		v.sync()
		old := ownedAt(v, paymentsWorkload, t0)
		if len(old) != 1 {
			t.Fatalf("%d owned_by facts, want 1", len(old))
		}
		v.clock.Advance(time.Hour)
		if err := v.cluster.cluster.SetLabel(fakes.Ref{Kind: fakes.KindDeployment, Namespace: "payments", Name: "payments-api"}, "team", "Platform"); err != nil {
			t.Fatal(err)
		}
		v.sync()
		now := ownedAt(v, paymentsWorkload, v.clock.Now())
		if len(now) != 1 || now[0].ID == old[0].ID {
			t.Fatalf("facts now = %+v, want one new fact", now)
		}
		closed := ownedAt(v, paymentsWorkload, t0)
		if len(closed) != 1 || closed[0].ID != old[0].ID || closed[0].ValidTo.IsZero() {
			t.Errorf("old fact = %+v, want it closed", closed)
		}
	})

	t.Run("a workload without a team label gets no fact", func(t *testing.T) {
		t.Parallel()
		v := bootRules(t, teamRule(1, teamKey))
		v.sync()
		if got := ownedAt(v, workload("deployment", "billing", "legacy-billing"), v.clock.Now()); len(got) != 0 {
			t.Errorf("facts = %+v, want none", got)
		}
	})

	t.Run("a deleted workload closes its derived fact through the complete sync", func(t *testing.T) {
		t.Parallel()
		v := bootRules(t, teamRule(1, teamKey))
		t0 := v.clock.Now()
		v.sync()
		if len(ownedAt(v, paymentsWorkload, t0)) != 1 {
			t.Fatal("no owned_by fact before the delete")
		}
		v.clock.Advance(time.Hour)
		if err := v.cluster.cluster.DeleteWorkload(fakes.Ref{Kind: fakes.KindDeployment, Namespace: "payments", Name: "payments-api"}); err != nil {
			t.Fatal(err)
		}
		v.sync()
		if got := ownedAt(v, paymentsWorkload, v.clock.Now()); len(got) != 0 {
			t.Errorf("facts after the delete = %+v, want none", got)
		}
		if got := ownedAt(v, paymentsWorkload, t0); len(got) != 1 || got[0].ValidTo.IsZero() {
			t.Errorf("fact at the first sync = %+v, want it closed", got)
		}
	})
}

// TestVocabDeriveBadTemplate is a key template that does not parse failing
// at boot, naming the rule.
func TestVocabDeriveBadTemplate(t *testing.T) {
	t.Parallel()
	_, err := vocab.Compile([]vocab.Mapping{teamRule(1, "kubernetes:team/{{ .labels.team | lower ")})
	if !errors.Is(err, vocab.ErrMapping) || !strings.Contains(err.Error(), "r-team@1") {
		t.Fatalf("got %v, want an error naming r-team@1", err)
	}
}
