package memstore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// refPrefix starts a ref: a ChangeSet's name for a subject it creates.
const refPrefix = "new:"

// uuidText is the canonical lowercase text form of a UUID.
func uuidText(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !strings.ContainsRune("0123456789abcdef", c):
			return false
		}
	}
	return true
}

// successor returns the UUID after id, counting in its last 48 bits. The
// store keeps minting monotonic with it when NewID falls behind, as a fresh
// generator does after a restore.
func successor(id string) string {
	n, _ := strconv.ParseUint(id[24:], 16, 64)
	return fmt.Sprintf("%s%012x", id[:24], n+1)
}

// Apply implements contracts.GraphStore.
func (s *Store) Apply(_ context.Context, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(cs, false)
}

// Head implements contracts.GraphStore.
func (s *Store) Head(context.Context) (time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.head, nil
}

// apply runs one ChangeSet. On restore it keeps the ChangeSet's record time
// and subject IDs instead of the clock's and NewID's. Nothing it changes
// survives an error.
func (s *Store) apply(in *modelv1alpha1.ChangeSet, restore bool) (res contracts.ApplyResult, err error) {
	if in.GetEventId() == "" {
		return res, errors.New("change set: event_id is required")
	}
	if at, ok := s.events[in.GetEventId()]; ok {
		return contracts.ApplyResult{RecordedAt: at, Duplicate: true}, nil
	}
	var r time.Time
	if restore {
		r = in.GetRecordedAt().AsTime()
		if !r.After(s.head) {
			return res, fmt.Errorf("event %s: recorded_at %s is not after %s", in.GetEventId(), r, s.head)
		}
	} else {
		base := time.Time{}
		if in.GetBaseRecordedAt() != nil {
			base = in.GetBaseRecordedAt().AsTime()
		}
		if !base.Equal(s.head) {
			return res, fmt.Errorf("event %s: base %s, head %s: %w", in.GetEventId(), base, s.head, contracts.ErrStale)
		}
		r = s.Now().UTC().Truncate(time.Microsecond)
		if !r.After(s.head) {
			r = s.head.Add(time.Microsecond)
		}
	}
	cs := proto.CloneOf(in)
	cs.RecordedAt = timestamppb.New(r)
	var undo []func()
	defer func() {
		if err != nil {
			for _, u := range slices.Backward(undo) {
				u()
			}
		}
	}()
	res = contracts.ApplyResult{RecordedAt: r, Subjects: map[string]contracts.SubjectID{}}
	mint := func(ref, kind, id string, rule modelv1alpha1.MintRule) (string, error) {
		if !strings.HasPrefix(ref, refPrefix) || res.Subjects[ref] != "" {
			return "", fmt.Errorf("ref %q: want a unique %q ref", ref, refPrefix)
		}
		if !model.Kind(kind).Valid() {
			return "", fmt.Errorf("ref %s: kind %q is not registered", ref, kind)
		}
		if !restore {
			if id = s.NewID(); id <= s.lastID {
				id = successor(s.lastID)
			}
		}
		if !uuidText(id) || id <= s.lastID {
			return "", fmt.Errorf("ref %s: minted ID %q is not a UUID after %q", ref, id, s.lastID)
		}
		prev := s.lastID
		s.lastID = id
		s.subjects[id] = &modelv1alpha1.Subject{
			SubjectId: id, Kind: kind, Status: modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, MintedAt: cs.RecordedAt,
			MintedBy: &modelv1alpha1.MintedBy{EventId: cs.GetEventId(), Rule: rule},
		}
		undo = append(undo, func() { delete(s.subjects, id); s.lastID = prev })
		res.Minted = append(res.Minted, proto.CloneOf(s.subjects[id]))
		res.Subjects[ref] = contracts.SubjectID(id)
		return id, nil
	}
	for _, m := range cs.GetMints() {
		if m.GetRule() != modelv1alpha1.MintRule_MINT_RULE_OBSERVATION && m.GetRule() != modelv1alpha1.MintRule_MINT_RULE_REFERENCE {
			return res, fmt.Errorf("mint %s: rule %s is not observation or reference", m.GetRef(), m.GetRule())
		}
		if m.SubjectId, err = mint(m.GetRef(), m.GetKind(), m.GetSubjectId(), m.GetRule()); err != nil {
			return res, err
		}
	}
	// Un-merge targets are found before anything moves, so the rest of the
	// ChangeSet can name them.
	revived := map[*modelv1alpha1.Unmerge]*modelv1alpha1.MergeRecord{}
	for _, u := range cs.GetUnmerges() {
		target, rec, err := s.unmergeTarget(u, r, mint)
		if err != nil {
			return res, err
		}
		u.TargetId, revived[u] = target, rec
		res.Subjects[u.GetRef()] = contracts.SubjectID(target)
	}
	if err := s.resolveRefs(cs.ProtoReflect(), res.Subjects); err != nil {
		return res, err
	}
	for _, b := range cs.GetBindings() {
		if _, _, _, err := model.Key(b.GetAlias()).Parse(); err != nil {
			return res, err
		}
		rows := make([]proto.Message, len(b.GetBindings()))
		for i, row := range b.GetBindings() {
			if row.GetAlias() != b.GetAlias() {
				return res, fmt.Errorf("binding of %s in the timeline of %s", row.GetAlias(), b.GetAlias())
			}
			rows[i] = row
		}
		if err := s.write(&undo, s.bindings, b.GetAlias(), &modelv1alpha1.BindingTimeline{Alias: b.GetAlias()}, rows, r); err != nil {
			return res, err
		}
	}
	for _, m := range cs.GetMerges() {
		rec, err := s.merge(m, cs.GetEventId(), r)
		if err != nil {
			return res, err
		}
		s.merges = append(s.merges, rec)
		undo = append(undo, func() { s.merges = s.merges[:len(s.merges)-1] })
		res.Merges = append(res.Merges, proto.CloneOf(rec))
	}
	for _, u := range cs.GetUnmerges() {
		if old := revived[u]; old != nil {
			i := slices.Index(s.merges, old)
			next := proto.CloneOf(old)
			next.UnmergedAt, next.UnmergeEventId = cs.RecordedAt, cs.GetEventId()
			s.merges[i] = next
			undo = append(undo, func() { s.merges[i] = old })
		}
	}
	if err := s.writeClaims(&undo, cs, r); err != nil {
		return res, err
	}
	prevHead := s.head
	s.head, s.events[cs.GetEventId()] = r, r
	s.journal = append(s.journal, cs)
	undo = append(undo, func() {
		s.head = prevHead
		delete(s.events, cs.GetEventId())
		s.journal = s.journal[:len(s.journal)-1]
	})
	return res, nil
}

// unmergeTarget finds where an un-merge's aliases go: the subject that
// merged into A with exactly those aliases, or a split mint.
func (s *Store) unmergeTarget(u *modelv1alpha1.Unmerge, r time.Time, mint func(ref, kind, id string, rule modelv1alpha1.MintRule) (string, error)) (string, *modelv1alpha1.MergeRecord, error) {
	a, ok := s.subjects[u.GetSubjectId()]
	if !ok || s.canonical(a.GetSubjectId(), r) != a.GetSubjectId() {
		return "", nil, fmt.Errorf("unmerge: subject %s is not active", u.GetSubjectId())
	}
	held := s.aliasesOf(a.GetSubjectId(), r)
	d := slices.Sorted(slices.Values(u.GetAliases()))
	d = slices.Compact(d)
	if len(d) == 0 || len(d) >= len(held) {
		return "", nil, fmt.Errorf("unmerge %s: want a non-empty proper subset of its %d aliases", a.GetSubjectId(), len(held))
	}
	for _, k := range d {
		if !slices.Contains(held, k) {
			return "", nil, fmt.Errorf("unmerge %s: alias %s is not one of its aliases", a.GetSubjectId(), k)
		}
	}
	for _, rec := range s.merges {
		if rec.GetSurvivorId() == a.GetSubjectId() && mergeLive(rec, r) && slices.Equal(rec.GetMergedAliases(), d) {
			if rec.GetRule() == modelv1alpha1.MergeRule_MERGE_RULE_PLACEHOLDER {
				return "", nil, fmt.Errorf("unmerge %s: a placeholder merge can't be un-merged", a.GetSubjectId())
			}
			return rec.GetMergedId(), rec, nil
		}
	}
	id, err := mint(u.GetRef(), a.GetKind(), u.GetTargetId(), modelv1alpha1.MintRule_MINT_RULE_SPLIT)
	return id, nil, err
}

// merge records one merge; the lower subject_id survives.
func (s *Store) merge(m *modelv1alpha1.Merge, eventID string, r time.Time) (*modelv1alpha1.MergeRecord, error) {
	ids := m.GetSubjectIds()
	if len(ids) != 2 || ids[0] == ids[1] {
		return nil, fmt.Errorf("merge: want two different subjects, got %v", ids)
	}
	for _, id := range ids {
		if s.canonical(id, r) != id {
			return nil, fmt.Errorf("merge: subject %s is not active", id)
		}
	}
	survivor, merged := min(ids[0], ids[1]), max(ids[0], ids[1])
	if a, b := s.subjects[survivor].GetKind(), s.subjects[merged].GetKind(); a != b {
		return nil, fmt.Errorf("merge: %s is a %s and %s a %s", survivor, a, merged, b)
	}
	return &modelv1alpha1.MergeRecord{
		SurvivorId: survivor, MergedId: merged, Rule: m.GetRule(), ConfidencePpm: m.GetConfidencePpm(),
		Evidence: m.GetEvidence(), SurvivorAliases: s.aliasesOf(survivor, r), MergedAliases: s.aliasesOf(merged, r),
		EventId: eventID, RecordedAt: timestamppb.New(r),
	}, nil
}

// resolveRefs replaces every ref in a subject_id or subject_ids field with
// its subject, and checks every subject named exists.
func (s *Store) resolveRefs(m protoreflect.Message, refs map[string]contracts.SubjectID) error {
	var err error
	fix := func(v string) string {
		if strings.HasPrefix(v, refPrefix) {
			if id, ok := refs[v]; ok {
				return string(id)
			}
			err = fmt.Errorf("ref %s names no mint or unmerge", v)
		} else if _, ok := s.subjects[v]; !ok && v != "" && err == nil {
			err = fmt.Errorf("subject %s: %w", v, contracts.ErrNotFound)
		}
		return v
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Name() == "subject_id" && fd.Kind() == protoreflect.StringKind:
			m.Set(fd, protoreflect.ValueOfString(fix(v.String())))
		case fd.Name() == "subject_ids" && fd.IsList():
			for i := range v.List().Len() {
				v.List().Set(i, protoreflect.ValueOfString(fix(v.List().Get(i).String())))
			}
		case fd.Message() == nil || fd.IsMap() || fd.Message().FullName().Parent() == "google.protobuf":
		case fd.IsList():
			for i := range v.List().Len() {
				if e := s.resolveRefs(v.List().Get(i).Message(), refs); e != nil && err == nil {
					err = e
				}
			}
		default:
			if e := s.resolveRefs(v.Message(), refs); e != nil && err == nil {
				err = e
			}
		}
		return err == nil
	})
	return err
}

// write checks a timeline's rows and replaces the series.
func (s *Store) write(undo *[]func(), t table, key string, head proto.Message, rows []proto.Message, r time.Time) error {
	type span struct{ from, to time.Time }
	var spans []span
	for _, m := range rows {
		iv, ok := m.(validity)
		if !ok {
			continue
		}
		sp := span{from: time.Unix(-1<<40, 0), to: time.Unix(1<<40, 0)}
		if iv.GetValidFrom() != nil {
			sp.from = iv.GetValidFrom().AsTime()
		}
		if iv.GetValidTo() != nil {
			sp.to = iv.GetValidTo().AsTime()
		}
		if !sp.to.After(sp.from) {
			return fmt.Errorf("%s: valid_to %s is not after valid_from %s", key, sp.to, sp.from)
		}
		spans = append(spans, sp)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from.Before(spans[j].from) })
	for i := 1; i < len(spans); i++ {
		if spans[i].from.Before(spans[i-1].to) {
			return fmt.Errorf("%s: rows overlap in valid time at %s", key, spans[i].from)
		}
	}
	*undo = append(*undo, t.replace(key, head, rows, r))
	return nil
}

// writeClaims writes the claim-store timelines and the resolver's state.
func (s *Store) writeClaims(undo *[]func(), cs *modelv1alpha1.ChangeSet, r time.Time) error {
	if err := s.writeFacts(undo, cs, r); err != nil {
		return err
	}
	if err := s.writeConflicts(undo, cs, r); err != nil {
		return err
	}
	for _, e := range cs.GetState() {
		if e.GetKey() == "" {
			return errors.New("state entry: key is required")
		}
		var rows []proto.Message
		if e.GetValue() != nil {
			rows = []proto.Message{e}
		}
		if err := s.write(undo, s.state, e.GetKey(), &modelv1alpha1.StateEntry{Key: e.GetKey()}, rows, r); err != nil {
			return err
		}
	}
	return nil
}

func messages[T proto.Message](in []T) []proto.Message {
	out := make([]proto.Message, len(in))
	for i, m := range in {
		out[i] = m
	}
	return out
}

// Backup implements contracts.GraphStore.
func (s *Store) Backup(_ context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, cs := range s.journal {
		if _, err := protodelim.MarshalTo(w, cs); err != nil {
			return fmt.Errorf("backup event %s: %w", cs.GetEventId(), err)
		}
	}
	return nil
}

// Restore implements contracts.GraphStore.
func (s *Store) Restore(_ context.Context, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.journal) > 0 {
		return errors.New("restore: the store is not empty")
	}
	br := bufio.NewReader(r)
	for {
		cs := &modelv1alpha1.ChangeSet{}
		err := protodelim.UnmarshalFrom(br, cs)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			n := len(s.journal) + 1
			s.reset()
			return fmt.Errorf("restore: read change set %d: %w", n, err)
		}
		if _, err := s.apply(cs, true); err != nil {
			s.reset()
			return fmt.Errorf("restore: %w", err)
		}
	}
}
