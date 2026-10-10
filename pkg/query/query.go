// Package query answers the CLI's questions from a [contracts.GraphStore]:
// what is known about a subject, who owns it, what it relates to and what
// changed. Every answer carries its provenance (each fact's supports give
// the source, event ID, confidence and observed time) and is computed at a
// [Point] in valid and record time.
//
// It is the layer between the commands and the store. The commands render
// what it returns; in M3 a server will answer the same questions and the
// CLI will ask it, so nothing here prints or reads flags.
package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// Querier reads one graph store.
type Querier struct {
	Graph contracts.GraphStore
	// Now is the time a question with no end is asked at; nil means time.Now.
	Now func() time.Time
}

// Point is the pair of times a question is asked at. A zero time means now,
// from the store's clock (docs/spec/data-model.md, "Queries").
type Point struct {
	// Valid is the time in the world the answer describes.
	Valid time.Time `json:"valid_at,omitzero"`
	// Recorded is the time the answer is as Bearing knew it.
	Recorded time.Time `json:"recorded_at,omitzero"`
}

// ErrNotFound is returned when a subject reference names nothing at the
// point asked about.
var ErrNotFound = errors.New("query: not found")

// maxMergeHops bounds how far a subject ID is followed through merges.
const maxMergeHops = 16

// Resolve returns the canonical subject ref names at p. A ref is a subject
// ID or a key such as "github:repo/acme/payments"; a key resolves through
// the bindings valid at p.Valid, so a renamed repository's old name finds
// the same subject (and, before the rename, the new name finds nothing).
func (q *Querier) Resolve(ctx context.Context, ref string, p Point) (contracts.SubjectID, error) {
	if ref == "" {
		return "", errors.New("a subject is required")
	}
	if strings.Contains(ref, ":") {
		s, err := q.Graph.ResolveKey(ctx, model.Key(ref), p.Valid, p.Recorded)
		if errors.Is(err, contracts.ErrNotFound) {
			return "", fmt.Errorf("no subject has key %s at %s: %w", ref, describe(p), ErrNotFound)
		}
		if err != nil {
			return "", fmt.Errorf("resolve key %s: %w", ref, err)
		}
		return contracts.SubjectID(s.GetSubjectId()), nil
	}
	id := ref
	for range maxMergeHops {
		s, err := q.Graph.Subject(ctx, contracts.SubjectID(id), p.Recorded)
		if errors.Is(err, contracts.ErrNotFound) {
			return "", fmt.Errorf("no subject %s as recorded %s: %w", ref, recordedText(p), ErrNotFound)
		}
		if err != nil {
			return "", fmt.Errorf("subject %s: %w", ref, err)
		}
		if s.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED || s.GetMergedInto() == "" {
			return contracts.SubjectID(s.GetSubjectId()), nil
		}
		id = s.GetMergedInto()
	}
	return "", fmt.Errorf("subject %s: merges go deeper than %d", ref, maxMergeHops)
}

// describe says when p is, for error messages.
func describe(p Point) string {
	v := "now"
	if !p.Valid.IsZero() {
		v = p.Valid.UTC().Format(time.RFC3339)
	}
	return v + ", " + recordedText(p)
}

func recordedText(p Point) string {
	if p.Recorded.IsZero() {
		return "as known now"
	}
	return "as known " + p.Recorded.UTC().Format(time.RFC3339)
}

// Ref names a subject in an answer: enough to read it and to ask about it
// next.
type Ref struct {
	ID   string `json:"subject_id"`
	Kind string `json:"kind,omitempty"`
	// Name is the subject's name attribute at the point asked, or failing
	// that the first key bound to it. Empty if neither is known.
	Name string `json:"name,omitempty"`
}

// String renders r as `Platform [Team 01a0…]`.
func (r Ref) String() string {
	switch {
	case r.Name != "" && r.Kind != "":
		return fmt.Sprintf("%s [%s %s]", r.Name, r.Kind, r.ID)
	case r.Kind != "":
		return fmt.Sprintf("[%s %s]", r.Kind, r.ID)
	}
	return r.ID
}

// labeler builds Refs, caching them for one question.
type labeler struct {
	q     *Querier
	p     Point
	cache map[string]Ref
}

func (q *Querier) labeler(p Point) *labeler {
	return &labeler{q: q, p: p, cache: map[string]Ref{}}
}

// ref returns the Ref of a subject ID as written in a fact: IDs in results
// are canonical already, so a failed lookup still gives a usable Ref.
func (l *labeler) ref(ctx context.Context, id string) (Ref, error) {
	if r, ok := l.cache[id]; ok {
		return r, nil
	}
	r := Ref{ID: id}
	s, err := l.q.Graph.Subject(ctx, contracts.SubjectID(id), l.p.Recorded)
	switch {
	case errors.Is(err, contracts.ErrNotFound):
		l.cache[id] = r
		return r, nil
	case err != nil:
		return Ref{}, fmt.Errorf("subject %s: %w", id, err)
	}
	r.Kind = s.GetKind()
	names, err := l.q.Graph.AsOf(ctx, contracts.FactFilter{SubjectID: contracts.SubjectID(id), Predicate: "name"}, l.p.Valid, l.p.Recorded)
	if err != nil {
		return Ref{}, fmt.Errorf("name of %s: %w", id, err)
	}
	r.Name = bestName(names)
	if r.Name == "" {
		// Ended or never named: the first key still says what it was.
		rows, err := l.q.Graph.Bindings(ctx, nil, []contracts.SubjectID{contracts.SubjectID(id)}, l.p.Recorded)
		if err != nil {
			return Ref{}, fmt.Errorf("keys of %s: %w", id, err)
		}
		for _, b := range rows {
			if r.Name == "" || b.GetAlias() < r.Name {
				r.Name = b.GetAlias()
			}
		}
	}
	l.cache[id] = r
	return r, nil
}

// bestName picks the name with the most confidence; ties go to the first,
// which is the smallest fact ID.
func bestName(names []*modelv1alpha1.FactState) string {
	best := ""
	var conf uint32
	for _, f := range names {
		if v := f.GetObject().GetValue().GetStringValue(); v != "" && (best == "" || f.GetConfidencePpm() > conf) {
			best, conf = v, f.GetConfidencePpm()
		}
	}
	return best
}

// optTime converts a timestamp, or returns nil for an absent one.
func optTime(t *timestamppb.Timestamp) *time.Time {
	if t == nil {
		return nil
	}
	v := t.AsTime()
	return &v
}
