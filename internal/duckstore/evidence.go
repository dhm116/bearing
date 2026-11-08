package duckstore

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// maxEvidenceDepth is the most facts one Evidence path crosses.
const maxEvidenceDepth = 10

// evState is where a path stands after zero or more facts: at record rec.
type evState struct {
	rec, cluster string
	hops         int
	clusters     []string // clusters on the path, so a path never returns to one (as Walk)
	steps        []graph.PathStep
	stepCl       []string // parallel to steps: a hop's cluster, empty for a fact
	ids          []string // record IDs along the path, to break ties
}

// evFact is a candidate fact seen in the direction of travel: from is the
// record matched against where a path stands, to the record it leads to.
type evFact struct {
	f            *graph.Fact
	from, to     string
	fromCl, toCl string // toCl is empty for an attribute
	toKind       string
}

// evClaim is a claim before its facts have their sources.
type evClaim struct {
	fact  *graph.Fact
	st    *evState // the path, ending with the fact
	end   string
	endCl string
}

// Evidence implements graph.Graph. Facts are read a level at a time, only
// those leaving the clusters the previous level reached, so no path is
// enumerated (Walk's cluster walk, level by level). Each fact keeps its best
// path: the best arrival among the previous level's claims whose end is in
// the fact's cluster, by same_as hops then record IDs. Relations come from
// one read of the open links among the end clusters' records, in the same
// transaction.
func (s *Store) Evidence(ctx context.Context, r graph.EvidenceRequest) (graph.Evidence, error) {
	ev, err := read(ctx, s, func(db querier) (graph.Evidence, error) { return s.claims(ctx, db, r) })
	if err != nil {
		return graph.Evidence{}, fmt.Errorf("evidence: %w", err)
	}
	return ev, nil
}

func (s *Store) claims(ctx context.Context, db querier, r graph.EvidenceRequest) (graph.Evidence, error) {
	depth := min(max(r.MaxDepth, 1), maxEvidenceDepth)
	starts, err := evStarts(ctx, db, r.From)
	if err != nil {
		return graph.Evidence{}, err
	}
	var claims []evClaim
	byID := map[string]*graph.Fact{}
	prev := starts
	for level := 1; level <= depth && len(prev) > 0; level++ {
		at := map[string][]*evState{}
		for _, st := range prev {
			at[st.cluster] = append(at[st.cluster], st)
		}
		facts, err := s.evFacts(ctx, db, r, slices.Sorted(maps.Keys(at)))
		if err != nil {
			return graph.Evidence{}, err
		}
		prev = nil
		for _, ef := range facts {
			if byID[ef.f.ID] != nil {
				continue // reached at a shallower level
			}
			st := bestArrival(at[ef.fromCl], ef)
			if st == nil {
				continue
			}
			byID[ef.f.ID] = ef.f
			end := evClaim{fact: ef.f, st: st}
			if ef.toCl != "" {
				end.end, end.endCl = ef.to, ef.toCl
				prev = append(prev, st)
			}
			if ef.toCl == "" || s.answers(r, ef) {
				claims = append(claims, end)
			}
		}
	}
	return s.assemble(ctx, db, claims, byID)
}

// evStarts are the From records that exist, each a path of no facts.
func evStarts(ctx context.Context, db querier, from []graph.EntityID) ([]*evState, error) {
	if len(from) == 0 {
		return nil, nil
	}
	ids := make([]string, len(from))
	for i, id := range from {
		ids[i] = string(id)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, cluster_id FROM entity WHERE id IN (`+placeholders(len(ids))+`) ORDER BY id`, anySlice(ids)...)
	if err != nil {
		return nil, fmt.Errorf("start records: %w", err)
	}
	return collect(rows, func(sc scanner) (*evState, error) {
		st := &evState{}
		err := sc.Scan(&st.rec, &st.cluster)
		st.clusters, st.ids = []string{st.cluster}, []string{st.rec}
		return st, err
	})
}

// evFacts reads the asserted facts leaving the given clusters in the
// direction of travel. An attribute has no object cluster, so it never
// matches in reverse.
func (s *Store) evFacts(ctx context.Context, db querier, r graph.EvidenceRequest, clusters []string) ([]evFact, error) {
	from, to := "se", "oe"
	if r.Reverse {
		from, to = to, from
	}
	where, args := holdsAt(r.At, true)
	where += ` AND ` + from + `.cluster_id IN (` + placeholders(len(clusters)) + `)`
	args = append(args, anySlice(clusters)...)
	if len(r.Predicates) > 0 {
		where += ` AND f.predicate IN (` + placeholders(len(r.Predicates)) + `)`
		args = append(args, anySlice(r.Predicates)...)
	}
	rows, err := db.QueryContext(ctx, `SELECT `+from+`.cluster_id, `+to+`.cluster_id, coalesce(`+to+`.kind, ''),
	f.id, f.subject_id, f.predicate, f.object_id, CAST(f.value AS VARCHAR), f.status, f.confidence, f.valid_from, f.valid_to,
	CAST(f.qualifiers AS VARCHAR) FROM `+factFrom+` WHERE `+where+` ORDER BY f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("facts: %w", err)
	}
	return collect(rows, func(sc scanner) (evFact, error) {
		var ef evFact
		var toCl sql.NullString
		f, err := scanFact(sc, &ef.fromCl, &toCl, &ef.toKind)
		ef.f, ef.toCl = &f, toCl.String
		ef.from, ef.to = string(f.Subject), string(f.Object)
		if r.Reverse {
			ef.from, ef.to = ef.to, ef.from
		}
		return ef, err
	})
}

// bestArrival picks the path to extend with ef among those standing in its
// cluster: fewest same_as hops, then smallest record IDs. A path never
// returns to a cluster it has been in.
func bestArrival(states []*evState, ef evFact) *evState {
	var best *evState
	var bestHops int
	var bestIDs []string
	for _, st := range states {
		if ef.toCl != "" && slices.Contains(st.clusters, ef.toCl) {
			continue
		}
		hops, ids := st.hops, slices.Clone(st.ids)
		if st.rec != ef.from {
			hops++
			ids = append(ids, ef.from)
		}
		if ef.toCl != "" {
			ids = append(ids, ef.to)
		}
		if best == nil || cmp.Or(cmp.Compare(hops, bestHops), slices.Compare(ids, bestIDs)) < 0 {
			best, bestHops, bestIDs = st, hops, ids
		}
	}
	if best == nil {
		return nil
	}
	next := &evState{rec: ef.to, cluster: ef.toCl, hops: bestHops, ids: bestIDs}
	next.clusters = append(slices.Clone(best.clusters), ef.toCl)
	next.steps, next.stepCl = slices.Clone(best.steps), slices.Clone(best.stepCl)
	if best.rec != ef.from {
		next.steps = append(next.steps, graph.PathStep{From: graph.EntityID(best.rec), To: graph.EntityID(ef.from)})
		next.stepCl = append(next.stepCl, best.cluster)
	}
	next.steps = append(next.steps, graph.PathStep{From: graph.EntityID(ef.from), To: graph.EntityID(ef.to), Fact: ef.f})
	next.stepCl = append(next.stepCl, "")
	return next
}

// answers reports whether the fact's end is the answer's kind or a
// descendant, by the end record's kind or, for a kindless record, by the
// predicate's range (domain, travelling backwards).
func (s *Store) answers(r graph.EvidenceRequest, ef evFact) bool {
	if r.AnswerKind == "" {
		return true
	}
	if ef.toKind != "" {
		return s.reg.IsA(model.Kind(ef.toKind), r.AnswerKind)
	}
	p, ok := s.reg.Predicate(ef.f.Predicate)
	allowed := p.Range
	if r.Reverse {
		allowed = p.Domain
	}
	return ok && len(allowed) > 0 && !slices.ContainsFunc(allowed, func(k model.Kind) bool { return !s.reg.IsA(k, r.AnswerKind) })
}

// assemble turns the chosen paths into claims: it attaches sources, names the
// hops' links, loads the records on the paths and in the end clusters, and
// relates the end clusters.
func (s *Store) assemble(ctx context.Context, db querier, claims []evClaim, byID map[string]*graph.Fact) (graph.Evidence, error) {
	facts := make([]*graph.Fact, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		facts = append(facts, byID[id])
	}
	if err := attachSources(ctx, db, facts); err != nil {
		return graph.Evidence{}, err
	}
	hopLinks, err := hopChains(ctx, db, claims)
	if err != nil {
		return graph.Evidence{}, err
	}
	var out graph.Evidence
	seen := map[string]bool{}
	var ids, endClusters []string
	for _, c := range claims {
		cl := graph.Claim{Fact: *c.fact, End: graph.EntityID(c.end), Path: slices.Clone(c.st.steps), SameAs: c.st.hops}
		for i, step := range cl.Path {
			if step.Fact != nil {
				cl.Facts++
			} else {
				cl.Path[i].Links = hopLinks[[2]string{string(step.From), string(step.To)}]
			}
			for _, id := range []graph.EntityID{step.From, step.To} {
				if id != "" && !seen[string(id)] {
					seen[string(id)] = true
					ids = append(ids, string(id))
				}
			}
		}
		cl.Start = cl.Path[0].From
		out.Claims = append(out.Claims, cl)
		if c.endCl != "" && !slices.Contains(endClusters, c.endCl) {
			endClusters = append(endClusters, c.endCl)
		}
	}
	slices.SortFunc(out.Claims, func(a, b graph.Claim) int {
		return cmp.Or(cmp.Compare(a.Facts, b.Facts), cmp.Compare(a.SameAs, b.SameAs), cmp.Compare(a.Fact.ID, b.Fact.ID))
	})
	byRec := map[graph.EntityID]graph.Record{}
	if len(ids) > 0 {
		recs, err := records(ctx, db, `e.id IN (`+placeholders(len(ids))+`)`, anySlice(ids)...)
		if err != nil {
			return graph.Evidence{}, err
		}
		for _, rec := range recs {
			byRec[rec.ID] = rec
		}
	}
	if len(endClusters) > 1 { // a single cluster has nothing to relate to
		recs, err := records(ctx, db, `e.cluster_id IN (`+placeholders(len(endClusters))+`)`, anySlice(endClusters)...)
		if err != nil {
			return graph.Evidence{}, err
		}
		for _, rec := range recs {
			byRec[rec.ID] = rec
		}
		if out.Relations, err = relations(ctx, db, recs); err != nil {
			return graph.Evidence{}, err
		}
	}
	out.Records = slices.SortedFunc(maps.Values(byRec), func(a, b graph.Record) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// hopChains returns, for each same_as hop on the claims' paths, the asserted
// links of the shortest chain between its records. Only links whose two
// records share a cluster count: a link the store refused joins different
// ones.
func hopChains(ctx context.Context, db querier, claims []evClaim) (map[[2]string][]string, error) {
	hops := map[[2]string]string{} // hop -> its cluster
	for _, c := range claims {
		for i, step := range c.st.steps {
			if step.Fact == nil {
				hops[[2]string{string(step.From), string(step.To)}] = c.st.stepCl[i]
			}
		}
	}
	out := map[[2]string][]string{}
	if len(hops) == 0 {
		return out, nil
	}
	clusters := slices.Sorted(maps.Values(hops))
	clusters = slices.Compact(clusters)
	rows, err := db.QueryContext(ctx, `SELECT l.id, l.a, l.b FROM identity_link l
		JOIN entity ea ON ea.id = l.a JOIN entity eb ON eb.id = l.b
		WHERE l.kind = 'same_as' AND l.status = 'asserted' AND l.valid_to IS NULL
		AND ea.cluster_id = eb.cluster_id AND ea.cluster_id IN (`+placeholders(len(clusters))+`)`, anySlice(clusters)...)
	if err != nil {
		return nil, fmt.Errorf("hop links: %w", err)
	}
	type edge struct{ to, link string }
	adj := map[string][]edge{}
	_, err = collect(rows, func(sc scanner) (struct{}, error) {
		var id, a, b string
		err := sc.Scan(&id, &a, &b)
		adj[a] = append(adj[a], edge{b, id})
		adj[b] = append(adj[b], edge{a, id})
		return struct{}{}, err
	})
	if err != nil {
		return nil, fmt.Errorf("hop links: %w", err)
	}
	for _, es := range adj {
		slices.SortFunc(es, func(x, y edge) int { return cmp.Or(cmp.Compare(x.to, y.to), cmp.Compare(x.link, y.link)) })
	}
	for hop := range hops {
		// Breadth first from hop[0]: the first chain found is the shortest,
		// and neighbours go in ID order so it is the same every time.
		type via struct{ from, link string }
		parent := map[string]via{hop[0]: {}}
		queue := []string{hop[0]}
		for len(queue) > 0 && parent[hop[1]] == (via{}) {
			cur := queue[0]
			queue = queue[1:]
			for _, e := range adj[cur] {
				if _, ok := parent[e.to]; !ok {
					parent[e.to] = via{cur, e.link}
					queue = append(queue, e.to)
				}
			}
		}
		var chain []string
		for at := hop[1]; at != hop[0] && parent[at] != (via{}); at = parent[at].from {
			chain = append(chain, parent[at].link)
		}
		slices.Reverse(chain)
		out[hop] = chain
	}
	return out, nil
}

// relations classifies each pair of distinct end clusters by the open links
// among their records (recs), read in one query.
func relations(ctx context.Context, db querier, recs []graph.Record) ([]graph.Relation, error) {
	clusterOf := make(map[graph.EntityID]graph.EntityID, len(recs))
	ids := make([]string, len(recs))
	for i, rec := range recs {
		clusterOf[rec.ID], ids[i] = rec.Cluster, string(rec.ID)
	}
	ls, err := loadLinks(ctx, db, graph.LinkFilter{}, ids)
	if err != nil {
		return nil, err
	}
	byPair := map[[2]graph.EntityID][]graph.Link{}
	for _, l := range ls {
		a, b := clusterOf[l.A], clusterOf[l.B]
		if a != b {
			pair := [2]graph.EntityID{min(a, b), max(a, b)}
			byPair[pair] = append(byPair[pair], l)
		}
	}
	var out []graph.Relation
	for _, pair := range slices.SortedFunc(maps.Keys(byPair), func(x, y [2]graph.EntityID) int { return slices.Compare(x[:], y[:]) }) {
		var distinct, notJoined, proposed []graph.Link
		for _, l := range byPair[pair] {
			switch {
			case l.Status == graph.Proposed:
				proposed = append(proposed, l)
			case l.Kind == graph.DistinctFrom:
				distinct = append(distinct, l)
			default:
				notJoined = append(notJoined, l)
			}
		}
		rel := graph.Relation{A: pair[0], B: pair[1], Links: slices.Concat(distinct, notJoined, proposed)}
		switch {
		case len(distinct) > 0:
			rel.Kind = graph.RelationDistinct
		case len(notJoined) > 0:
			rel.Kind = graph.RelationAssertedNotJoined
		default:
			rel.Kind = graph.RelationProposed
		}
		out = append(out, rel)
	}
	return out, nil
}
