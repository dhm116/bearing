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
)

// writeConflicts writes conflict and data-quality issue timelines.
func (s *Store) writeConflicts(undo *[]func(), cs *modelv1alpha1.ChangeSet, r time.Time) error {
	for _, ct := range cs.GetConflicts() {
		for _, c := range ct.GetConflicts() {
			if c.GetSubjectId() != ct.GetSubjectId() || c.GetPredicate() != ct.GetPredicate() {
				return fmt.Errorf("conflict on %s %s in the timeline of %s %s", c.GetSubjectId(), c.GetPredicate(), ct.GetSubjectId(), ct.GetPredicate())
			}
		}
		head := &modelv1alpha1.ConflictTimeline{SubjectId: ct.GetSubjectId(), Predicate: ct.GetPredicate()}
		if err := s.write(undo, s.conflict, ct.GetSubjectId()+"\x00"+ct.GetPredicate(), head, messages(ct.GetConflicts()), r); err != nil {
			return err
		}
	}
	for _, it := range cs.GetIssues() {
		if it.GetKey() == "" {
			return errors.New("issue timeline: key is required")
		}
		if err := s.write(undo, s.issues, it.GetKey(), &modelv1alpha1.IssueTimeline{Key: it.GetKey()}, messages(it.GetSpans()), r); err != nil {
			return err
		}
	}
	return nil
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
	var out []*modelv1alpha1.Conflict
	for _, ser := range s.conflict {
		c, _ := covering(ser.at(r), v).(*modelv1alpha1.Conflict)
		if c == nil || predicate != "" && c.GetPredicate() != predicate {
			continue
		}
		c = proto.CloneOf(c)
		c.SubjectId = s.canonical(c.GetSubjectId(), r)
		if want != "" && c.GetSubjectId() != want {
			continue
		}
		for _, p := range c.GetPositions() {
			for _, o := range p.GetObjects() {
				if o.GetSubjectId() != "" {
					o.SubjectId = s.canonical(o.GetSubjectId(), r)
				}
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetSubjectId()+"\x00"+out[i].GetPredicate() < out[j].GetSubjectId()+"\x00"+out[j].GetPredicate()
	})
	return out, nil
}

// DataQuality implements contracts.GraphStore.
func (s *Store) DataQuality(_ context.Context, f contracts.IssueFilter, validAt, recordedAt time.Time) ([]*modelv1alpha1.DataQualityIssue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, r := s.times(validAt, recordedAt)
	var keys []string
	for k := range s.issues {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []*modelv1alpha1.DataQualityIssue
	for _, k := range keys {
		sp, _ := covering(s.issues[k].at(r), v).(*modelv1alpha1.IssueSpan)
		if sp == nil || len(f.Issues) > 0 && !slices.Contains(f.Issues, sp.GetIssue().GetIssue()) {
			continue
		}
		issue := proto.CloneOf(sp.GetIssue())
		kindMatch, sourceMatch := len(f.Kinds) == 0, len(f.Sources) == 0
		for i, id := range issue.GetSubjectIds() {
			issue.SubjectIds[i] = s.canonical(id, r)
			kindMatch = kindMatch || slices.Contains(f.Kinds, s.subjects[id].GetKind())
		}
		for _, sup := range issue.GetSupports() {
			sourceMatch = sourceMatch || slices.Contains(f.Sources, sup.GetSource())
		}
		if kindMatch && sourceMatch {
			out = append(out, issue)
		}
	}
	return out, nil
}
