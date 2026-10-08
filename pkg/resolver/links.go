package resolver

import (
	"cmp"
	"context"
	"slices"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// linkSourcePrefix starts the source of an authoritative link's evidence
// (docs/spec/data-model.md, "Identity across systems").
const linkSourcePrefix = "core/identity/link/"

// linkTarget is an authoritative link and the subject that holds its key, or
// the placeholder minted for it.
type linkTarget struct {
	key     keyRef
	subject string
}

// mergeOnLinks merges the entity's subject with the subjects its
// authoritative links name: one system storing another's permanent ID is the
// strongest identity evidence there is (rule authoritative, confidence 1.0).
// The earlier mint survives. The guard keeps two things one system tells
// apart from becoming one: subjects that hold different `id` aliases of one
// key type in one namespace never merge. Targets that the guard excludes from
// each other are evidence for neither, so none of them merges; across applies
// the first merge stands. The conflict records that flag such a pair come
// with the next part.
//
// Evidence is judged when the observation that carries the link is applied.
// A merge is never undone by evidence ending, so a later complete linked_ids
// list that drops the link changes nothing here.
func (u *run) mergeOnLinks(ctx context.Context, targets []linkTarget) error {
	for i := range targets {
		targets[i].subject = u.g.mustCanon(ctx, targets[i].subject)
	}
	for _, t := range targets {
		clash := false
		for _, o := range targets {
			if o.subject == t.subject {
				continue
			}
			held, err := u.holdDifferentIDs(ctx, t.subject, o.subject)
			if err != nil {
				return err
			}
			clash = clash || held
		}
		chosen := u.g.mustCanon(ctx, u.chosen)
		if clash || chosen == u.g.mustCanon(ctx, t.subject) {
			continue
		}
		held, err := u.holdDifferentIDs(ctx, chosen, t.subject)
		if err != nil {
			return err
		}
		if held {
			continue
		}
		u.merge(chosen, t.subject, modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE)
		u.merges[len(u.merges)-1].Evidence = []*modelv1alpha1.Support{u.linkEvidence(t.key)}
	}
	return nil
}

// linkEvidence is the support of the identity evidence an authoritative link
// is: derived from the link claim, at full confidence.
func (u *run) linkEvidence(k keyRef) *modelv1alpha1.Support {
	conf := uint32(model.MaxConfidence)
	via := &modelv1alpha1.Via{Object: string(k.key)}
	for _, own := range u.p.keys {
		via.Subject = append(via.Subject, string(own.key))
	}
	return &modelv1alpha1.Support{
		Source: linkSourcePrefix + u.p.ev.Source, EventId: u.p.ev.ID, ObservationId: u.p.obs.GetId(),
		ObservedAt: u.p.obs.GetTime(), ConfidencePpm: &conf, Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_DERIVED,
		Via: via, Evidence: u.p.obs.GetData().GetEvidence(),
	}
}

// holdDifferentIDs reports whether two subjects hold different `id` aliases
// of one key type in one namespace: the merge guard.
func (u *run) holdDifferentIDs(ctx context.Context, a, b string) (bool, error) {
	ha, err := u.heldIDs(ctx, a)
	if err != nil {
		return false, err
	}
	hb, err := u.heldIDs(ctx, b)
	if err != nil {
		return false, err
	}
	for _, x := range ha {
		for _, y := range hb {
			if x.groupKey() == y.groupKey() && x.key != y.key {
				return true, nil
			}
		}
	}
	return false, nil
}

// heldIDs returns the `id` aliases a canonical subject holds, counting the
// ones this ChangeSet binds to it or to a subject it merges into it.
func (u *run) heldIDs(ctx context.Context, id string) ([]keyRef, error) {
	var out []keyRef
	owned, err := u.g.owned(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, a := range owned {
		if k, ok := u.ix.lookup(string(a)); ok && k.isID() {
			out = append(out, k)
		}
	}
	for subject, keys := range u.held {
		if u.g.mustCanon(ctx, subject) == id {
			out = append(out, keys...)
		}
	}
	slices.SortFunc(out, func(a, b keyRef) int { return cmp.Compare(a.key, b.key) })
	return out, nil
}
