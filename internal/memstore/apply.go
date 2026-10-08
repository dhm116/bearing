package memstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// refPrefix starts a ref: a ChangeSet's name for a subject it creates.
const refPrefix = "new:"

// Apply implements contracts.GraphStore.
func (s *Store) Apply(ctx context.Context, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	if err := ctx.Err(); err != nil {
		return contracts.ApplyResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(cs, nil)
}

// Head implements contracts.GraphStore.
func (s *Store) Head(context.Context) (time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.head, nil
}

// result is the ApplyResult of a journal entry.
func result(e *modelv1alpha1.JournalEntry, duplicate bool) contracts.ApplyResult {
	res := contracts.ApplyResult{RecordedAt: e.GetChangeSet().GetRecordedAt().AsTime(), Duplicate: duplicate, Subjects: map[string]contracts.SubjectID{}}
	for ref, id := range e.GetSubjects() {
		res.Subjects[ref] = contracts.SubjectID(id)
	}
	for _, m := range e.GetMinted() {
		res.Minted = append(res.Minted, proto.CloneOf(m))
	}
	for _, m := range e.GetMerges() {
		res.Merges = append(res.Merges, proto.CloneOf(m))
	}
	return res
}

// apply runs one ChangeSet. On replay, want is the journal entry being
// restored: the apply keeps its record time and subject IDs instead of the
// clock's and the ID source's, and fails if it decides anything
// differently. Nothing it changes survives an error or a panic.
func (s *Store) apply(in *modelv1alpha1.ChangeSet, want *modelv1alpha1.JournalEntry) (res contracts.ApplyResult, err error) {
	if in.GetEventId() == "" {
		return res, errors.New("change set: event_id is required")
	}
	if n := proto.Size(in); n > contracts.MaxChangeSetBytes {
		return res, fmt.Errorf("event %s: change set is %d bytes, over the %d-byte limit", in.GetEventId(), n, contracts.MaxChangeSetBytes)
	}
	if err := contracts.CheckChangeSetLimits(in); err != nil {
		return res, fmt.Errorf("event %s: %w", in.GetEventId(), err)
	}
	if i, ok := s.events[in.GetEventId()]; ok {
		if want != nil {
			return res, fmt.Errorf("event %s: applied twice", in.GetEventId())
		}
		return result(s.journal[i], true), nil
	}
	r, err := s.recordTime(in, want != nil)
	if err != nil {
		return res, err
	}
	cs := proto.CloneOf(in)
	cs.RecordedAt = timestamppb.New(r)
	var undo []func()
	defer func() {
		p := recover()
		if p != nil || err != nil {
			for _, u := range slices.Backward(undo) {
				u()
			}
		}
		if p != nil {
			panic(p)
		}
	}()
	entry := &modelv1alpha1.JournalEntry{ChangeSet: cs, Subjects: map[string]string{}}
	mint := func(ref, kind, id string, rule modelv1alpha1.MintRule) (string, error) {
		if !strings.HasPrefix(ref, refPrefix) || entry.GetSubjects()[ref] != "" {
			return "", fmt.Errorf("ref %q: want a unique %q ref", ref, refPrefix)
		}
		if !model.Kind(kind).Valid() {
			return "", fmt.Errorf("ref %s: kind %q is not registered", ref, kind)
		}
		if want == nil {
			id = s.IDs.NewIDAt(r)
		}
		if err := s.checkID(id, r); err != nil {
			return "", fmt.Errorf("ref %s: %w", ref, err)
		}
		prev := s.lastID
		s.lastID = id
		s.subjects[id] = &modelv1alpha1.Subject{
			SubjectId: id, Kind: kind, Status: modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, MintedAt: cs.RecordedAt,
			MintedBy: &modelv1alpha1.MintedBy{EventId: cs.GetEventId(), Rule: rule},
		}
		undo = append(undo, func() { delete(s.subjects, id); s.lastID = prev })
		entry.Minted = append(entry.Minted, proto.CloneOf(s.subjects[id]))
		entry.Subjects[ref] = id
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
	// Un-merge targets are found against the state before the ChangeSet,
	// before anything moves, so the rest of the ChangeSet can name them.
	revived := map[*modelv1alpha1.Unmerge]*modelv1alpha1.MergeRecord{}
	claimed := map[string]bool{}
	held := map[string][]string{} // each subject's aliases before the ChangeSet, found once
	s.direct = map[string][]string{}
	defer func() { s.direct = nil }()
	for _, u := range cs.GetUnmerges() {
		if ref := u.GetRef(); !strings.HasPrefix(ref, refPrefix) || entry.GetSubjects()[ref] != "" {
			return res, fmt.Errorf("unmerge of %s: ref %q: want a unique %q ref", u.GetSubjectId(), ref, refPrefix)
		}
		target, rec, err := s.unmergeTarget(u, r, claimed, held, mint)
		if err != nil {
			return res, err
		}
		u.TargetId, revived[u] = target, rec
		entry.Subjects[u.GetRef()] = target
		entry.Unmerges = append(entry.Unmerges, &modelv1alpha1.UnmergeOutcome{TargetId: target, Split: rec == nil})
	}
	if err := s.resolveRefs(cs.ProtoReflect(), entry.GetSubjects()); err != nil {
		return res, err
	}
	aliases := map[string]bool{}
	for _, b := range cs.GetBindings() {
		if _, _, _, err := model.Key(b.GetAlias()).Parse(); err != nil {
			return res, err
		}
		if aliases[b.GetAlias()] {
			return res, fmt.Errorf("alias %s: two timelines in one change set", b.GetAlias())
		}
		aliases[b.GetAlias()] = true
		rows := make([]proto.Message, len(b.GetBindings()))
		for i, row := range b.GetBindings() {
			if row.GetAlias() != b.GetAlias() {
				return res, fmt.Errorf("binding of %s in the timeline of %s", row.GetAlias(), b.GetAlias())
			}
			rows[i] = row
			if s.indexAlias(row.GetSubjectId(), b.GetAlias()) {
				subject, alias := row.GetSubjectId(), b.GetAlias()
				undo = append(undo, func() { s.unindexAlias(subject, alias) })
			}
		}
		old := s.bindings[b.GetAlias()]
		if err := s.write(&undo, s.bindings, b.GetAlias(), &modelv1alpha1.BindingTimeline{Alias: b.GetAlias()}, rows, r); err != nil {
			return res, err
		}
		s.relive(&undo, b.GetAlias(), old, s.bindings[b.GetAlias()])
	}
	recorded := 0                    // bytes of merge records so far: each carries two alias sets
	s.direct = map[string][]string{} // bindings may have changed
	for _, m := range cs.GetMerges() {
		rec, err := s.merge(m, cs.GetEventId(), r)
		if err != nil {
			return res, err
		}
		if recorded += proto.Size(rec); recorded > contracts.MaxChangeSetBytes {
			return res, fmt.Errorf("event %s: merge records are %d bytes, over the %d-byte limit", cs.GetEventId(), recorded, contracts.MaxChangeSetBytes)
		}
		undo = append(undo, s.addMerge(rec))
		entry.Merges = append(entry.Merges, proto.CloneOf(rec))
	}
	for _, u := range cs.GetUnmerges() {
		if old := revived[u]; old != nil {
			i := s.indexOfMerge(old)
			next := proto.CloneOf(old)
			next.UnmergedAt, next.UnmergeEventId = cs.RecordedAt, cs.GetEventId()
			s.merges[i] = next
			undo = append(undo, func() { s.merges[i] = old })
		}
	}
	if err := s.writeClaims(&undo, cs, r); err != nil {
		return res, err
	}
	if want != nil && !proto.Equal(entry, want) {
		return res, fmt.Errorf("event %s: replay decided differently from the journal", cs.GetEventId())
	}
	// The entry must marshal (proto3 strings must be UTF-8) and fit in one
	// backup frame, or the backup won't write or restore.
	b, err := proto.Marshal(entry)
	if err != nil {
		return res, fmt.Errorf("event %s: %w", cs.GetEventId(), err)
	}
	if n := protowire.SizeTag(2) + protowire.SizeBytes(len(b)); n > contracts.MaxChangeSetBytes {
		return res, fmt.Errorf("event %s: change set is %d bytes as recorded, over the %d-byte limit", cs.GetEventId(), n, contracts.MaxChangeSetBytes)
	}
	// Nothing after this can fail, so it needs no undo.
	s.head, s.events[cs.GetEventId()] = r, len(s.journal)
	s.journal = append(s.journal, entry)
	return result(entry, false), nil
}

// recordTime is the record time of an apply: max(now, head + 1µs), or on
// replay the journal's, which must be after the head and no later than the
// time the backup was taken. It doesn't compare with the clock, which may
// have stepped back since.
func (s *Store) recordTime(cs *modelv1alpha1.ChangeSet, replay bool) (time.Time, error) {
	if replay {
		if cs.GetRecordedAt() == nil {
			return time.Time{}, fmt.Errorf("event %s: no recorded_at", cs.GetEventId())
		}
		r := cs.GetRecordedAt().AsTime()
		if !r.After(s.head) {
			return time.Time{}, fmt.Errorf("event %s: recorded_at %s is not after %s", cs.GetEventId(), r, s.head)
		}
		if r.After(s.restoreUntil) {
			return time.Time{}, fmt.Errorf("event %s: recorded_at %s is after the backup was taken at %s", cs.GetEventId(), r, s.restoreUntil)
		}
		return r, nil
	}
	base := time.Time{}
	if cs.GetBaseRecordedAt() != nil {
		base = cs.GetBaseRecordedAt().AsTime()
	}
	if !base.Equal(s.head) {
		return time.Time{}, fmt.Errorf("event %s: base %s, head %s: %w", cs.GetEventId(), base, s.head, contracts.ErrStale)
	}
	r := s.Now().UTC().Truncate(time.Microsecond)
	if !r.After(s.head) {
		r = s.head.Add(time.Microsecond)
	}
	return r, nil
}

// checkID checks a minted subject ID: a canonical UUIDv7, after the last
// one minted, with a timestamp no later than the apply's record time.
func (s *Store) checkID(id string, r time.Time) error {
	at, err := model.UUIDv7Time(id)
	switch {
	case err != nil:
		return fmt.Errorf("minted ID: %w", err)
	case id <= s.lastID:
		return fmt.Errorf("minted ID %s is not after %s", id, s.lastID)
	case at.After(r):
		return fmt.Errorf("minted ID %s has a timestamp after the record time %s", id, r)
	}
	return nil
}

// unmergeTarget finds where an un-merge's aliases go: the subject that
// merged into A with exactly those aliases, or a split mint. claimed holds
// the targets of the ChangeSet's earlier un-merges; none may be reused.
func (s *Store) unmergeTarget(u *modelv1alpha1.Unmerge, r time.Time, claimed map[string]bool, heldBy map[string][]string, mint func(ref, kind, id string, rule modelv1alpha1.MintRule) (string, error)) (string, *modelv1alpha1.MergeRecord, error) {
	a, ok := s.subjects[u.GetSubjectId()]
	if !ok || s.canonical(a.GetSubjectId(), r) != a.GetSubjectId() {
		return "", nil, fmt.Errorf("unmerge: subject %s is not active", u.GetSubjectId())
	}
	held, ok := heldBy[a.GetSubjectId()]
	if !ok {
		held = s.aliasesOf(a.GetSubjectId(), r)
		heldBy[a.GetSubjectId()] = held
	}
	d := slices.Sorted(slices.Values(u.GetAliases()))
	d = slices.Compact(d)
	if len(d) == 0 || len(d) >= len(held) {
		return "", nil, fmt.Errorf("unmerge %s: want a non-empty proper subset of its %d aliases", a.GetSubjectId(), len(held))
	}
	for _, k := range d {
		if _, found := slices.BinarySearch(held, k); !found {
			return "", nil, fmt.Errorf("unmerge %s: alias %s is not one of its aliases", a.GetSubjectId(), k)
		}
	}
	for _, i := range s.survivorOf[a.GetSubjectId()] {
		if rec := s.merges[i]; mergeLive(rec, r) && slices.Equal(rec.GetMergedAliases(), d) {
			if rec.GetRule() == modelv1alpha1.MergeRule_MERGE_RULE_PLACEHOLDER {
				return "", nil, fmt.Errorf("unmerge %s: a placeholder merge can't be un-merged", a.GetSubjectId())
			}
			if claimed[rec.GetMergedId()] {
				return "", nil, fmt.Errorf("unmerge %s: an earlier un-merge in the change set already reactivates %s", a.GetSubjectId(), rec.GetMergedId())
			}
			claimed[rec.GetMergedId()] = true
			return rec.GetMergedId(), rec, nil
		}
	}
	// A split mint is new, so no other un-merge can have claimed it.
	id, err := mint(u.GetRef(), a.GetKind(), u.GetTargetId(), modelv1alpha1.MintRule_MINT_RULE_SPLIT)
	return id, nil, err
}

// merge records one merge; the lower subject_id survives.
func (s *Store) merge(m *modelv1alpha1.Merge, eventID string, r time.Time) (*modelv1alpha1.MergeRecord, error) {
	ids := m.GetSubjectIds()
	if len(ids) != 2 || ids[0] == ids[1] {
		return nil, fmt.Errorf("merge: want two different subjects, got %d IDs", len(ids))
	}
	if m.GetRule() == modelv1alpha1.MergeRule_MERGE_RULE_UNSPECIFIED {
		return nil, errors.New("merge: rule is required")
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
func (s *Store) resolveRefs(m protoreflect.Message, refs map[string]string) error {
	var err error
	fix := func(v string) string {
		if strings.HasPrefix(v, refPrefix) {
			if id, ok := refs[v]; ok {
				return id
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
	keys := map[string]bool{}
	for _, e := range cs.GetState() {
		if e.GetKey() == "" {
			return errors.New("state entry: key is required")
		}
		if keys[e.GetKey()] {
			return fmt.Errorf("state entry %s: twice in one change set", e.GetKey())
		}
		keys[e.GetKey()] = true
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
