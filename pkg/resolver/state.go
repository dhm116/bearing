package resolver

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// State keys. A key names a subject by a segment of its own, which the store
// replaces with the ID it mints when the segment is the ref of a mint in the
// same ChangeSet (docs/spec/contracts.md, "State keys"), so a value never
// holds a subject ID:
//
//	bind/<alias>/<subject>   the alias's writes to one subject (BindingWrites)
//	del/<namespace>/<subject>  the subject's deletions in a namespace (ScopeWatermarks)
//
// Source-supplied text in a key is percent-encoded.
const (
	bindPrefix = "bind/"
	delPrefix  = "del/"
)

func bindKey(alias model.Key, subject string) string {
	return bindPrefix + escape(string(alias)) + "/" + subject
}

func delKey(ns, subject string) string {
	return delPrefix + escape(ns) + "/" + subject
}

// packWrites encodes writes for one state entry. The subject is the key's,
// so BindingWrite.subject_id stays empty.
func packWrites(ws []write) (*anypb.Any, error) {
	sort.SliceStable(ws, func(i, j int) bool { return writeLess(ws[i], ws[j]) })
	msg := &resolverv1alpha1.BindingWrites{}
	for _, w := range ws {
		bw := &resolverv1alpha1.BindingWrite{Key: w.key, Tentative: w.tentative}
		if !w.tentative {
			bw.ValidFrom = timestamppb.New(w.from)
		}
		msg.Writes = append(msg.Writes, bw)
	}
	return anypb.New(msg)
}

// writeLess orders writes for a deterministic entry: observed before
// tentative, then by start and key.
func writeLess(a, b write) bool {
	if a.tentative != b.tentative {
		return !a.tentative
	}
	if !a.from.Equal(b.from) {
		return a.from.Before(b.from)
	}
	return model.CompareOrderingKeys(a.key, b.key) < 0
}

// unpackWrites decodes an entry whose key named subject.
func unpackWrites(a *anypb.Any, subject string) ([]write, error) {
	msg := &resolverv1alpha1.BindingWrites{}
	if err := a.UnmarshalTo(msg); err != nil {
		return nil, fmt.Errorf("%w: binding writes: %w", errCorrupt, err)
	}
	out := make([]write, 0, len(msg.GetWrites()))
	for _, bw := range msg.GetWrites() {
		w := write{key: bw.GetKey(), tentative: bw.GetTentative(), subject: subject}
		switch {
		case bw.GetReleased():
			return nil, fmt.Errorf("%w: a binding write is a release", errCorrupt)
		case !w.tentative && bw.GetValidFrom() == nil:
			return nil, fmt.Errorf("%w: observed binding write with no start", errCorrupt)
		case !w.tentative:
			w.from = bw.GetValidFrom().AsTime()
		}
		out = append(out, w)
	}
	return out, nil
}

// entryFor returns the state entry that holds ws, with all the writes of one
// (alias, subject), or one that deletes it if ws is empty.
func entryFor(alias model.Key, subject string, ws []write) (*modelv1alpha1.StateEntry, error) {
	e := &modelv1alpha1.StateEntry{Key: bindKey(alias, subject)}
	if len(ws) == 0 {
		return e, nil
	}
	v, err := packWrites(ws)
	if err != nil {
		return nil, err
	}
	e.Value = v
	return e, nil
}

// packMarks encodes a subject's deletion marks for one state entry.
func packMarks(ms []mark) (*anypb.Any, error) {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].at.Before(ms[j].at) })
	msg := &resolverv1alpha1.ScopeWatermarks{}
	for _, m := range ms {
		msg.Watermarks = append(msg.Watermarks, &resolverv1alpha1.Watermark{
			At: timestamppb.New(m.at), Key: m.key, Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_DELETED,
		})
	}
	return anypb.New(msg)
}

// unpackMarks decodes a subject's deletion marks.
func unpackMarks(a *anypb.Any) ([]mark, error) {
	msg := &resolverv1alpha1.ScopeWatermarks{}
	if err := a.UnmarshalTo(msg); err != nil {
		return nil, fmt.Errorf("%w: deletion marks: %w", errCorrupt, err)
	}
	out := make([]mark, 0, len(msg.GetWatermarks()))
	for _, w := range msg.GetWatermarks() {
		if w.GetAt() == nil || w.GetKey() == nil || w.GetReason() != modelv1alpha1.SupportReason_SUPPORT_REASON_DELETED {
			return nil, fmt.Errorf("%w: deletion mark without a time, key or the deleted reason", errCorrupt)
		}
		out = append(out, mark{at: w.GetAt().AsTime(), key: w.GetKey()})
	}
	return out, nil
}
