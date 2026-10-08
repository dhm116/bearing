package memstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// writeConflicts writes conflict and data-quality issue timelines.
func (s *Store) writeConflicts(undo *[]func(), cs *modelv1alpha1.ChangeSet, r time.Time) error {
	seen := map[string]bool{}
	for _, ct := range cs.GetConflicts() {
		key := ct.GetSubjectId() + "\x00" + ct.GetPredicate()
		if ct.GetSubjectId() == "" || ct.GetPredicate() == "" {
			return errors.New("conflict timeline: subject_id and predicate are required")
		}
		if seen[key] {
			return fmt.Errorf("conflict timeline %s %s: twice in one change set", ct.GetSubjectId(), ct.GetPredicate())
		}
		seen[key] = true
		for _, c := range ct.GetConflicts() {
			if c == nil || c.GetSubjectId() != ct.GetSubjectId() || c.GetPredicate() != ct.GetPredicate() {
				return fmt.Errorf("conflict on %s %s in the timeline of %s %s", c.GetSubjectId(), c.GetPredicate(), ct.GetSubjectId(), ct.GetPredicate())
			}
			if len(c.GetPositions()) == 0 {
				return fmt.Errorf("conflict on %s %s: no positions", ct.GetSubjectId(), ct.GetPredicate())
			}
			for _, p := range c.GetPositions() {
				if p == nil || p.GetSourceSystem() == "" {
					return fmt.Errorf("conflict on %s %s: a position needs a source_system", ct.GetSubjectId(), ct.GetPredicate())
				}
				for _, o := range p.GetObjects() {
					if _, err := model.FactID(ct.GetSubjectId(), ct.GetPredicate(), o); err != nil {
						return fmt.Errorf("conflict on %s %s: %w", ct.GetSubjectId(), ct.GetPredicate(), err)
					}
				}
			}
		}
		head := &modelv1alpha1.ConflictTimeline{SubjectId: ct.GetSubjectId(), Predicate: ct.GetPredicate()}
		if err := s.write(undo, s.conflict, key, head, messages(ct.GetConflicts()), r); err != nil {
			return err
		}
	}
	keys := map[string]bool{}
	for _, it := range cs.GetIssues() {
		if it.GetKey() == "" {
			return errors.New("issue timeline: key is required")
		}
		if keys[it.GetKey()] {
			return fmt.Errorf("issue timeline %s: twice in one change set", it.GetKey())
		}
		keys[it.GetKey()] = true
		for _, sp := range it.GetSpans() {
			if sp.GetIssue().GetIssue() == modelv1alpha1.IssueType_ISSUE_TYPE_UNSPECIFIED || !validIssue(sp.GetIssue().GetIssue()) {
				return fmt.Errorf("issue timeline %s: want a span with a known issue type", it.GetKey())
			}
			for _, sup := range sp.GetIssue().GetSupports() {
				if sup.GetSource() == "" {
					return fmt.Errorf("issue timeline %s: a support needs a source", it.GetKey())
				}
			}
		}
		if err := s.write(undo, s.issues, it.GetKey(), &modelv1alpha1.IssueTimeline{Key: it.GetKey()}, messages(it.GetSpans()), r); err != nil {
			return err
		}
	}
	return nil
}

func validIssue(t modelv1alpha1.IssueType) bool {
	_, ok := modelv1alpha1.IssueType_name[int32(t)]
	return ok
}

// Conflicts implements contracts.GraphStore.
func (s *Store) Conflicts(_ context.Context, subject contracts.SubjectID, predicate string, validAt, recordedAt time.Time) ([]*modelv1alpha1.Conflict, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, r := s.times(validAt, recordedAt)
	want := ""
	if subject != "" {
		want = s.canonical(string(subject), r)
	}
	type entry struct {
		c   *modelv1alpha1.Conflict
		key string // the timeline's key: subject and predicate as written
	}
	var found []entry
	for _, key := range sortedKeys(s.conflict) {
		c, _ := covering(s.conflict[key].at(r), v).(*modelv1alpha1.Conflict)
		if c == nil || predicate != "" && c.GetPredicate() != predicate {
			continue
		}
		c = proto.CloneOf(c)
		c.SubjectId = s.canonical(c.GetSubjectId(), r)
		if want != "" && c.GetSubjectId() != want {
			continue
		}
		for _, p := range c.GetPositions() {
			p.Objects = s.canonicalObjects(p.GetObjects(), r)
		}
		found = append(found, entry{c, key})
	}
	// Timelines written under different subjects can canonicalize to one
	// (subject, predicate) after a merge; both answer, in the order of their
	// keys as written.
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i].c, found[j].c
		if a.GetSubjectId() != b.GetSubjectId() {
			return a.GetSubjectId() < b.GetSubjectId()
		}
		if a.GetPredicate() != b.GetPredicate() {
			return a.GetPredicate() < b.GetPredicate()
		}
		return found[i].key < found[j].key
	})
	var out []*modelv1alpha1.Conflict
	for _, e := range found {
		out = append(out, e.c)
	}
	return out, nil
}

// canonicalObjects canonicalizes subject objects as recorded at r and drops
// the repeats that merges create, keeping the first of each.
func (s *Store) canonicalObjects(objects []*modelv1alpha1.FactObject, r time.Time) []*modelv1alpha1.FactObject {
	var out []*modelv1alpha1.FactObject
	for _, o := range objects {
		if o.GetSubjectId() != "" {
			o.SubjectId = s.canonical(o.GetSubjectId(), r)
		}
		if !slices.ContainsFunc(out, func(x *modelv1alpha1.FactObject) bool { return proto.Equal(x, o) }) {
			out = append(out, o)
		}
	}
	return out
}

// DataQuality implements contracts.GraphStore.
func (s *Store) DataQuality(_ context.Context, f contracts.IssueFilter, validAt, recordedAt time.Time) ([]*modelv1alpha1.DataQualityIssue, error) {
	for _, t := range f.Types {
		if t == modelv1alpha1.IssueType_ISSUE_TYPE_UNSPECIFIED || !validIssue(t) {
			return nil, fmt.Errorf("filter: unknown issue type %d", t)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, r := s.times(validAt, recordedAt)
	var out []*modelv1alpha1.DataQualityIssue
	for _, k := range sortedKeys(s.issues) {
		sp, _ := covering(s.issues[k].at(r), v).(*modelv1alpha1.IssueSpan)
		if sp == nil || len(f.Types) > 0 && !slices.Contains(f.Types, sp.GetIssue().GetIssue()) {
			continue
		}
		issue := proto.CloneOf(sp.GetIssue())
		kindMatch, sourceMatch := len(f.Kinds) == 0, len(f.Sources) == 0
		var ids []string
		for _, id := range issue.GetSubjectIds() {
			kindMatch = kindMatch || slices.Contains(f.Kinds, s.subjects[id].GetKind())
			if c := s.canonical(id, r); !slices.Contains(ids, c) {
				ids = append(ids, c)
			}
		}
		issue.SubjectIds = ids
		for _, sup := range issue.GetSupports() {
			sourceMatch = sourceMatch || slices.Contains(f.Sources, sup.GetSource())
		}
		if kindMatch && sourceMatch {
			out = append(out, issue)
		}
	}
	return out, nil
}
