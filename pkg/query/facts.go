package query

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// Object is a fact's object: a subject for a relation, or a value.
type Object struct {
	Subject *Ref   `json:"subject,omitempty"`
	Type    string `json:"type,omitempty"`
	Value   any    `json:"value,omitempty"`
}

// String renders the object for people: a relation as its Ref, a value as
// its text.
func (o Object) String() string {
	switch {
	case o.Subject != nil:
		return o.Subject.String()
	}
	switch o.Value.(type) {
	case map[string]any, []any:
		b, err := json.Marshal(o.Value)
		if err == nil {
			return string(b)
		}
	}
	return fmt.Sprint(o.Value)
}

// Support is one source's backing for a fact: the provenance of an answer.
type Support struct {
	Source        string           `json:"source"`
	Adapter       string           `json:"adapter,omitempty"`
	EventID       string           `json:"event_id"`
	ObservedAt    time.Time        `json:"observed_at"`
	ConfidencePPM uint32           `json:"confidence_ppm"`
	Reason        string           `json:"reason"`
	ValidFrom     *time.Time       `json:"valid_from,omitempty"`
	ValidTo       *time.Time       `json:"valid_to,omitempty"`
	RecordedAt    time.Time        `json:"recorded_at"`
	Qualifiers    []map[string]any `json:"qualifiers,omitempty"`
	EvidenceURL   string           `json:"evidence_url,omitempty"`
}

// Fact is a fact at a point, with the supports that give it.
type Fact struct {
	ID            string     `json:"fact_id"`
	Subject       Ref        `json:"subject"`
	Predicate     string     `json:"predicate"`
	Object        Object     `json:"object"`
	Status        string     `json:"status"`
	StatusReason  string     `json:"status_reason"`
	ConfidencePPM uint32     `json:"confidence_ppm"`
	ValidFrom     *time.Time `json:"valid_from,omitempty"`
	ValidTo       *time.Time `json:"valid_to,omitempty"`
	Supports      []Support  `json:"supports"`
}

// enumName turns FACT_STATUS_ASSERTED into "asserted", given the prefix.
func enumName(prefix, name string) string {
	return strings.ToLower(strings.TrimPrefix(name, prefix))
}

func (l *labeler) object(ctx context.Context, o *modelv1alpha1.FactObject) (Object, error) {
	if o.GetSubjectId() != "" {
		r, err := l.ref(ctx, o.GetSubjectId())
		if err != nil {
			return Object{}, err
		}
		return Object{Subject: &r}, nil
	}
	return Object{Type: enumName("VALUE_TYPE_", o.GetType().String()), Value: o.GetValue().AsInterface()}, nil
}

func supports(in []*modelv1alpha1.Support) []Support {
	out := make([]Support, 0, len(in))
	for _, s := range in {
		sup := Support{
			Source: s.GetSource(), Adapter: s.GetAdapter(), EventID: s.GetEventId(), ObservedAt: s.GetObservedAt().AsTime(),
			ConfidencePPM: s.GetConfidencePpm(), Reason: enumName("SUPPORT_REASON_", s.GetReason().String()),
			ValidFrom: optTime(s.GetValidFrom()), ValidTo: optTime(s.GetValidTo()), RecordedAt: s.GetRecordedAt().AsTime(),
			EvidenceURL: s.GetEvidence().GetUrl(),
		}
		for _, q := range s.GetQualifiers() {
			sup.Qualifiers = append(sup.Qualifiers, q.AsMap())
		}
		out = append(out, sup)
	}
	return out
}

func (l *labeler) fact(ctx context.Context, f *modelv1alpha1.FactState) (Fact, error) {
	subject, err := l.ref(ctx, f.GetSubjectId())
	if err != nil {
		return Fact{}, err
	}
	object, err := l.object(ctx, f.GetObject())
	if err != nil {
		return Fact{}, err
	}
	return Fact{
		ID: f.GetFactId(), Subject: subject, Predicate: f.GetPredicate(), Object: object,
		Status: enumName("FACT_STATUS_", f.GetStatus().String()), StatusReason: enumName("STATUS_REASON_", f.GetStatusReason().String()),
		ConfidencePPM: f.GetConfidencePpm(), ValidFrom: optTime(f.GetValidFrom()), ValidTo: optTime(f.GetValidTo()),
		Supports: supports(f.GetSupports()),
	}, nil
}

func (l *labeler) facts(ctx context.Context, in []*modelv1alpha1.FactState) ([]Fact, error) {
	out := make([]Fact, 0, len(in))
	for _, f := range in {
		fact, err := l.fact(ctx, f)
		if err != nil {
			return nil, err
		}
		out = append(out, fact)
	}
	return out, nil
}
