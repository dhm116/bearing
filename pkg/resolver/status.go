package resolver

import (
	"maps"
	"math/big"
	"slices"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// DefaultThreshold is the confidence at which a fact is asserted, in parts
// per million (docs/spec/data-model.md, "Predicates").
const DefaultThreshold = 900_000

// noisyOR combines the confidences of source systems that agree: the exact
// integer noisy-OR of docs/spec/data-model.md, "Confidence". Each is the
// maximum within one group of systems.
func noisyOR(groups []uint32) uint32 {
	if len(groups) == 0 {
		return 0
	}
	million := big.NewInt(model.MaxConfidence)
	p := big.NewInt(1)
	for _, g := range groups {
		p.Mul(p, big.NewInt(int64(model.MaxConfidence)-int64(g)))
	}
	// P / 1000000^(n-1), rounded half to even.
	d := new(big.Int).Exp(million, big.NewInt(int64(len(groups)-1)), nil)
	q, r := new(big.Int).QuoRem(p, d, new(big.Int))
	switch c := new(big.Int).Lsh(r, 1).Cmp(d); {
	case c > 0, c == 0 && q.Bit(0) == 1:
		q.Add(q, big.NewInt(1))
	}
	return uint32(model.MaxConfidence - q.Int64()) //nolint:gosec // G115: q is in [0, MaxConfidence]
}

// support is one source's live support for a fact over [from, to), as the
// status rules read it.
type support struct {
	// group is the source's confidence group: its source system, or the
	// group of systems configured as copies of each other.
	group         string
	authoritative bool
	from, to      int64
	conf          uint32
}

// candidateFact is a fact of one (subject, predicate), with its supports
// from every source.
type candidateFact struct {
	// object is the fact's object: a subject ID, or a token for a value.
	object   string
	supports []support
}

// span is a fact's status over [from, to).
type span struct {
	from, to int64
	status   modelv1alpha1.FactStatus
	reason   modelv1alpha1.StatusReason
	conf     uint32
}

// predicateRules is what the status rules need to know of a predicate.
type predicateRules struct {
	// conflict is the predicate's conflict policy.
	conflict modelv1alpha1.ConflictPolicy
	// relation says the predicate's objects are subjects, which step 5
	// requires to be observed if the predicate can conflict.
	relation bool
	// threshold is the assertion threshold.
	threshold uint32
}

// statuses computes the status timelines of one subject's predicate: for
// each fact the spans of valid time it has a live support in, with the
// status of docs/spec/data-model.md, "Status", steps 1, 3, 5 and 7, and the
// conflicts of "Conflicts" that authority or nothing decides. Overrides,
// precedence and same_as are not applied yet. observed(object) lists the
// valid-time intervals the relation's object has a live exists support.
func statuses(rules predicateRules, facts []candidateFact, observed func(object string) []interval) [][]span {
	out, _ := statusesAndConflicts(rules, facts, observed)
	return out
}

// position is one source system's side of a disagreement: the facts (by
// index) it supports at or above the threshold, and whether it holds an
// authoritative support for any of them.
type position struct {
	group         string
	authoritative bool
	facts         []int
}

// conflictSpan is a disagreement over [from, to): each system's side, and the
// rule that decided it, or none while it stands.
type conflictSpan struct {
	from, to   int64
	positions  []position
	resolution modelv1alpha1.ConflictResolution
}

func sameConflict(a, b conflictSpan) bool {
	return a.resolution == b.resolution && slices.EqualFunc(a.positions, b.positions, func(x, y position) bool {
		return x.group == y.group && x.authoritative == y.authoritative && slices.Equal(x.facts, y.facts)
	})
}

// statusesAndConflicts is statuses, and also returns the disagreements the
// predicate's conflict policy finds, whether they stand or authority decided
// them (docs/spec/data-model.md, "Conflicts").
func statusesAndConflicts(rules predicateRules, facts []candidateFact, observed func(object string) []interval) ([][]span, []conflictSpan) {
	cuts := breakpoints(facts, observed, rules)
	out := make([][]span, len(facts))
	var conflicts []conflictSpan
	for i := 0; i <= len(cuts); i++ {
		from, to := int64(negInf), int64(posInf)
		if i > 0 {
			from = cuts[i-1]
		}
		if i < len(cuts) {
			to = cuts[i]
		}
		sts, cf := rules.at(from, facts, observed)
		if cf != nil {
			cf.from, cf.to = from, to
			if n := len(conflicts); n > 0 && conflicts[n-1].to == from && sameConflict(conflicts[n-1], *cf) {
				conflicts[n-1].to = to
			} else {
				conflicts = append(conflicts, *cf)
			}
		}
		for j, st := range sts {
			if st.status == modelv1alpha1.FactStatus_FACT_STATUS_NONE {
				continue
			}
			st.from, st.to = from, to
			if n := len(out[j]); n > 0 && out[j][n-1].to == from && sameSpan(out[j][n-1], st) {
				out[j][n-1].to = to
				continue
			}
			out[j] = append(out[j], st)
		}
	}
	return out, conflicts
}

func sameSpan(a, b span) bool {
	return a.status == b.status && a.reason == b.reason && a.conf == b.conf
}

// interval is a stretch of valid time.
type interval struct{ from, to int64 }

// breakpoints returns, sorted, every valid time at which a support or an
// object's existence starts or ends.
func breakpoints(facts []candidateFact, observed func(string) []interval, rules predicateRules) []int64 {
	var cuts []int64
	add := func(t int64) {
		if t != negInf && t != posInf {
			cuts = append(cuts, t)
		}
	}
	for _, f := range facts {
		for _, s := range f.supports {
			add(s.from)
			add(s.to)
		}
		if rules.needsObserved() {
			for _, iv := range observed(f.object) {
				add(iv.from)
				add(iv.to)
			}
		}
	}
	slices.Sort(cuts)
	return slices.Compact(cuts)
}

// needsObserved reports whether step 5 applies to the predicate.
func (r predicateRules) needsObserved() bool {
	return r.relation && r.conflict != modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE
}

// perFact is what the live supports say about one fact at one valid time.
type perFact struct {
	groups map[string]uint32 // group -> maximum confidence
	auth   map[string]bool   // group -> has an authoritative live support
}

// at computes every fact's status at one valid time; none for a fact with no
// live support. It also returns the disagreement at that time, if any.
func (r predicateRules) at(v int64, facts []candidateFact, observed func(string) []interval) ([]span, *conflictSpan) {
	state := make([]perFact, len(facts))
	out := make([]span, len(facts))
	for i, f := range facts {
		pf := perFact{groups: map[string]uint32{}, auth: map[string]bool{}}
		for _, s := range f.supports {
			if s.from <= v && v < s.to {
				pf.groups[s.group] = max(pf.groups[s.group], s.conf)
				if s.authoritative {
					pf.auth[s.group] = true
				}
			}
		}
		state[i] = pf
		if len(pf.groups) == 0 {
			out[i] = span{status: modelv1alpha1.FactStatus_FACT_STATUS_NONE, reason: modelv1alpha1.StatusReason_STATUS_REASON_NO_SUPPORT}
			continue
		}
		confs := make([]uint32, 0, len(pf.groups))
		for _, c := range pf.groups {
			confs = append(confs, c)
		}
		c := noisyOR(confs)
		switch {
		case c < r.threshold:
			out[i] = span{status: modelv1alpha1.FactStatus_FACT_STATUS_CANDIDATE, reason: modelv1alpha1.StatusReason_STATUS_REASON_BELOW_THRESHOLD, conf: c}
		case r.needsObserved() && !slices.ContainsFunc(observed(f.object), func(iv interval) bool { return iv.from <= v && v < iv.to }):
			out[i] = span{status: modelv1alpha1.FactStatus_FACT_STATUS_CANDIDATE, reason: modelv1alpha1.StatusReason_STATUS_REASON_UNOBSERVED_OBJECT, conf: c}
		default:
			out[i] = span{status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, reason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, conf: c}
		}
	}
	// Conflicts: among the objects that passed steps 1 to 5.
	var passing []int
	for i, o := range out {
		if o.status == modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED {
			passing = append(passing, i)
		}
	}
	// agreeing[g] lists the passing facts that group g supports at or above
	// the threshold.
	agreeing := map[string][]int{}
	for _, i := range passing {
		for g, c := range state[i].groups {
			if c >= r.threshold {
				agreeing[g] = append(agreeing[g], i)
			}
		}
	}
	var conflicted []int
	switch r.conflict {
	case modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_ONE:
		if len(passing) > 1 {
			conflicted = passing
		}
	case modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET:
		conflicted = setConflict(passing, agreeing)
	}
	if len(conflicted) == 0 {
		return out, nil
	}
	// Authority: when the systems that hold an authoritative support agree
	// with each other, they decide. Otherwise the conflict stands.
	var deciding []int
	decided := false
	for _, g := range slices.Sorted(maps.Keys(agreeing)) {
		if !slices.ContainsFunc(agreeing[g], func(i int) bool { return state[i].auth[g] }) {
			continue
		}
		if !decided {
			deciding, decided = agreeing[g], true
		} else if !slices.Equal(deciding, agreeing[g]) {
			decided = false
			break
		}
	}
	// A system that holds several objects of a `one` predicate can't decide
	// it: its own sources disagree.
	if r.conflict == modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_ONE && len(deciding) != 1 {
		decided = false
	}
	cf := &conflictSpan{}
	for _, g := range slices.Sorted(maps.Keys(agreeing)) {
		if len(agreeing[g]) == 0 {
			continue
		}
		auth := slices.ContainsFunc(agreeing[g], func(i int) bool { return state[i].auth[g] })
		cf.positions = append(cf.positions, position{group: g, authoritative: auth, facts: agreeing[g]})
	}
	if len(cf.positions) == 0 {
		// Each object passes on the combined confidence of several systems,
		// none of which meets the threshold alone: a position is then what a
		// system supports of the conflicted objects at any confidence, so a
		// standing conflict always has one (the store refuses one without).
		for _, g := range groupsOf(state, conflicted) {
			var held []int
			auth := false
			for _, i := range conflicted {
				if _, ok := state[i].groups[g]; ok {
					held = append(held, i)
					auth = auth || state[i].auth[g]
				}
			}
			cf.positions = append(cf.positions, position{group: g, authoritative: auth, facts: held})
		}
	}
	if decided {
		cf.resolution = modelv1alpha1.ConflictResolution_CONFLICT_RESOLUTION_AUTHORITY
	}
	for _, i := range conflicted {
		switch {
		case decided && slices.Contains(deciding, i):
			out[i].status, out[i].reason = modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, modelv1alpha1.StatusReason_STATUS_REASON_AUTHORITY
		case decided:
			out[i].status, out[i].reason = modelv1alpha1.FactStatus_FACT_STATUS_CANDIDATE, modelv1alpha1.StatusReason_STATUS_REASON_AUTHORITY
		default:
			out[i].status, out[i].reason = modelv1alpha1.FactStatus_FACT_STATUS_CONFLICTED, modelv1alpha1.StatusReason_STATUS_REASON_CONFLICT
		}
	}
	return out, cf
}

// setConflict returns, for a `set` predicate, the passing facts that are not
// in every non-empty set of agreeing facts when at least two systems have
// different non-empty sets.
func setConflict(passing []int, agreeing map[string][]int) []int {
	var sets [][]int
	for _, g := range slices.Sorted(maps.Keys(agreeing)) {
		if len(agreeing[g]) > 0 {
			sets = append(sets, agreeing[g])
		}
	}
	differ := false
	for _, s := range sets[min(1, len(sets)):] {
		if !slices.Equal(s, sets[0]) {
			differ = true
		}
	}
	if !differ {
		return nil
	}
	var out []int
	for _, i := range passing {
		if !slices.ContainsFunc(sets, func(s []int) bool { return !slices.Contains(s, i) }) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// groupsOf returns, sorted, the groups that support any of the facts.
func groupsOf(state []perFact, facts []int) []string {
	seen := map[string]bool{}
	for _, i := range facts {
		for g := range state[i].groups {
			seen[g] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
