package vocab

import (
	adapterv1alpha1 "bearing.example/gen/go/bearing/adapter/v1alpha1"
)

// ToProto is the module as an adapter publishes it in DescribeResponse. The
// owner and how the module was verified stay behind: they are the core's to
// say, not the adapter's.
func ToProto(m Module) *adapterv1alpha1.VocabularyModule {
	out := &adapterv1alpha1.VocabularyModule{
		Namespace: m.Namespace, Version: m.Version,
		Source: &adapterv1alpha1.VocabularySource{Repo: m.Source.Repo, Path: m.Source.Path, Ref: m.Source.Ref},
	}
	for _, t := range m.Terms {
		pt := &adapterv1alpha1.VocabularyTerm{
			Name: t.Name, Is: t.Is, Type: t.Type, Cardinality: t.Cardinality, Conflict: t.Conflict,
			Domain: t.Domain, Range: t.Range,
		}
		if d := t.Deprecated; d != nil {
			pt.Deprecated = &adapterv1alpha1.VocabularyDeprecation{Since: d.Since, ReplacedBy: d.ReplacedBy, Consider: d.Consider, Reason: d.Reason}
		}
		out.Terms = append(out.Terms, pt)
	}
	for _, s := range m.Suggests {
		out.Suggests = append(out.Suggests, &adapterv1alpha1.VocabularyMapping{
			Id: s.ID, Subject: s.Subject, Predicate: s.Predicate, Object: s.Object, Justification: s.Justification,
		})
	}
	return out
}

// FromProto is a module an adapter published, as the core takes it: the owner
// is always the adapter and every suggestion is proposed, whatever was sent.
// It does not check the module; Check and the registry do.
func FromProto(p *adapterv1alpha1.VocabularyModule) *Module {
	if p == nil {
		return nil
	}
	m := &Module{
		Owner: OwnerAdapter, Namespace: p.GetNamespace(), Version: p.GetVersion(),
		Source: Source{Repo: p.GetSource().GetRepo(), Path: p.GetSource().GetPath(), Ref: p.GetSource().GetRef()},
	}
	for _, t := range p.GetTerms() {
		mt := Term{
			Name: t.GetName(), Is: t.GetIs(), Type: t.GetType(), Cardinality: t.GetCardinality(), Conflict: t.GetConflict(),
			Domain: t.GetDomain(), Range: t.GetRange(),
		}
		if d := t.GetDeprecated(); d != nil {
			mt.Deprecated = &Deprecation{Since: d.GetSince(), ReplacedBy: d.GetReplacedBy(), Consider: d.GetConsider(), Reason: d.GetReason()}
		}
		m.Terms = append(m.Terms, mt)
	}
	for _, s := range p.GetSuggests() {
		m.Suggests = append(m.Suggests, Mapping{
			ID: s.GetId(), Subject: s.GetSubject(), Predicate: s.GetPredicate(), Object: s.GetObject(),
			Justification: s.GetJustification(), Status: Proposed,
		})
	}
	return m
}
