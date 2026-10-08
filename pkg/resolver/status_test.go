package resolver

import (
	"fmt"
	"strings"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// The worked table of docs/spec/data-model.md, "Confidence".
func TestNoisyORMatchesTheSpecTable(t *testing.T) {
	tests := []struct {
		name   string
		groups []uint32
		want   uint32
	}{
		{"one system", []uint32{1_000_000}, 1_000_000},
		{"two systems", []uint32{800_000, 700_000}, 940_000},
		{"two weak systems", []uint32{600_000, 500_000}, 800_000},
		{"rounds half to even down", []uint32{333_333, 333_333}, 555_555},
		{"none", nil, 0},
		// The product of four groups, (5e5)^4, is beyond 64 bits.
		{"four systems", []uint32{500_000, 500_000, 500_000, 500_000}, 937_500},
		{"twenty systems stay exact", []uint32{100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000, 100_000}, 878_423},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := noisyOR(tc.groups); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNoisyORRoundsHalfToEven(t *testing.T) {
	// P = 1 * 500000; 0.5 rounds to 0, so C = 1000000. P = 3 * 500000; 1.5
	// rounds to 2, so C = 999998. P = 5 * 500000; 2.5 rounds to 2, so C =
	// 999998.
	for _, tc := range []struct {
		groups []uint32
		want   uint32
	}{
		{[]uint32{999_999, 500_000}, 1_000_000},
		{[]uint32{999_997, 500_000}, 999_998},
		{[]uint32{999_995, 500_000}, 999_998},
	} {
		if got := noisyOR(tc.groups); got != tc.want {
			t.Errorf("noisyOR(%v) = %d, want %d", tc.groups, got, tc.want)
		}
	}
}

func sup(group string, from, to int64, conf uint32) support {
	return support{group: group, from: from, to: to, conf: conf}
}

func rules(conflict modelv1alpha1.ConflictPolicy, relation bool) predicateRules {
	return predicateRules{conflict: conflict, relation: relation, threshold: DefaultThreshold}
}

func showSpans(spans [][]span) string {
	var out []string
	for i, ss := range spans {
		for _, s := range ss {
			out = append(out, fmt.Sprintf("%d:[%s,%s)=%s/%s/%d", i, bound(s.from), bound(s.to),
				strings.TrimPrefix(s.status.String(), "FACT_STATUS_"), strings.TrimPrefix(s.reason.String(), "STATUS_REASON_"), s.conf))
		}
	}
	return strings.Join(out, " ")
}

const (
	noneC = modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE
	oneC  = modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_ONE
	setC  = modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET
)

func TestStatusesNeedAThresholdAndFollowValidTime(t *testing.T) {
	facts := []candidateFact{
		{object: "a", supports: []support{sup("github", hour(0), hour(10), 1_000_000)}},
		{object: "b", supports: []support{sup("github", hour(0), posInf, 800_000), sup("catalog", hour(5), posInf, 700_000)}},
	}
	got := showSpans(statuses(rules(noneC, true), facts, nil))
	want := "0:[0,10)=ASSERTED/NONE/1000000 1:[0,5)=CANDIDATE/BELOW_THRESHOLD/800000 1:[5,+)=ASSERTED/NONE/940000"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestStatusesLeaveOutTimeWithoutSupport(t *testing.T) {
	facts := []candidateFact{{object: "a", supports: []support{sup("g", hour(2), hour(4), 1_000_000), sup("g", hour(6), hour(8), 1_000_000)}}}
	got := showSpans(statuses(rules(noneC, false), facts, nil))
	want := "0:[2,4)=ASSERTED/NONE/1000000 0:[6,8)=ASSERTED/NONE/1000000"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestStatusesNeedAnObservedObjectWhereConflictsCanHappen(t *testing.T) {
	facts := []candidateFact{{object: "T", supports: []support{sup("catalog", hour(0), posInf, 1_000_000)}}}
	seen := map[string][]interval{"T": {{from: hour(3), to: posInf}}}
	observed := func(o string) []interval { return seen[o] }
	got := showSpans(statuses(rules(setC, true), facts, observed))
	want := "0:[0,3)=CANDIDATE/UNOBSERVED_OBJECT/1000000 0:[3,+)=ASSERTED/NONE/1000000"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	// A relation that can't conflict needs no observed object.
	if got := showSpans(statuses(rules(noneC, true), facts, nil)); got != "0:[0,+)=ASSERTED/NONE/1000000" {
		t.Fatalf("got %s, want the fact asserted without its object observed", got)
	}
}

func TestStatusesFindConflictsAndLetAuthorityDecide(t *testing.T) {
	always := func(group string, auth bool, conf uint32) support {
		s := sup(group, negInf, posInf, conf)
		s.authoritative = auth
		return s
	}
	tests := []struct {
		name     string
		conflict modelv1alpha1.ConflictPolicy
		facts    []candidateFact
		want     string
	}{
		{
			"one object is no conflict", oneC,
			[]candidateFact{{object: "main", supports: []support{always("github", false, 1_000_000)}}},
			"0:[-,+)=ASSERTED/NONE/1000000",
		},
		{"two objects of a one predicate conflict", oneC, []candidateFact{
			{object: "main", supports: []support{always("github", false, 1_000_000)}},
			{object: "master", supports: []support{always("catalog", false, 1_000_000)}},
		}, "0:[-,+)=CONFLICTED/CONFLICT/1000000 1:[-,+)=CONFLICTED/CONFLICT/1000000"},
		{"an authoritative system decides", oneC, []candidateFact{
			{object: "main", supports: []support{always("github", true, 1_000_000)}},
			{object: "master", supports: []support{always("catalog", false, 1_000_000)}},
		}, "0:[-,+)=ASSERTED/AUTHORITY/1000000 1:[-,+)=CANDIDATE/AUTHORITY/1000000"},
		{"two authoritative systems that disagree leave it standing", oneC, []candidateFact{
			{object: "main", supports: []support{always("github", true, 1_000_000)}},
			{object: "master", supports: []support{always("catalog", true, 1_000_000)}},
		}, "0:[-,+)=CONFLICTED/CONFLICT/1000000 1:[-,+)=CONFLICTED/CONFLICT/1000000"},
		{"a set predicate keeps what every system agrees on", setC, []candidateFact{
			{object: "T1", supports: []support{always("github", false, 1_000_000), always("catalog", false, 1_000_000)}},
			{object: "T2", supports: []support{always("github", false, 1_000_000)}},
			{object: "T3", supports: []support{always("catalog", false, 1_000_000)}},
		}, "0:[-,+)=ASSERTED/NONE/1000000 1:[-,+)=CONFLICTED/CONFLICT/1000000 2:[-,+)=CONFLICTED/CONFLICT/1000000"},
		{"systems that agree on a set don't conflict", setC, []candidateFact{
			{object: "T1", supports: []support{always("github", false, 1_000_000), always("catalog", false, 1_000_000)}},
		}, "0:[-,+)=ASSERTED/NONE/1000000"},
		{"a candidate takes no part in a conflict", oneC, []candidateFact{
			{object: "main", supports: []support{always("github", false, 1_000_000)}},
			{object: "master", supports: []support{always("catalog", false, 300_000)}},
		}, "0:[-,+)=ASSERTED/NONE/1000000 1:[-,+)=CANDIDATE/BELOW_THRESHOLD/300000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := showSpans(statuses(rules(tc.conflict, false), tc.facts, nil))
			if got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}
