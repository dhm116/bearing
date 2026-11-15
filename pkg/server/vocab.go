package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
	"bearing.example/pkg/telemetry"
	"bearing.example/pkg/vocab"
)

// vocabPartition holds the decisions people make on proposed mappings (ADR 13,
// spike). One partition means one worker applies them in the order logged.
const vocabPartition store.Partition = "core/vocabulary"

// typeMappingDecided is the CloudEvents type of a mapping decision.
var typeMappingDecided = mustType(&eventv1alpha1.MappingDecided{})

// mappingDecisions maps the API's words to the event's enum.
var mappingDecisions = map[string]eventv1alpha1.MappingDecided_Decision{
	"accept": eventv1alpha1.MappingDecided_DECISION_ACCEPT,
	"reject": eventv1alpha1.MappingDecided_DECISION_REJECT,
}

// ErrNoVocabulary means the server was not given a Vocabulary.
var ErrNoVocabulary = errors.New("server: vocabulary decisions are not configured")

// ErrMappingRefused means accepting a mapping would stop Bearing booting; the
// error text names what is needed first.
var ErrMappingRefused = errors.New("mapping refused")

// MappingStore is the mappings a person decides on; duckstore.Store is one.
type MappingStore interface {
	Mappings(ctx context.Context) ([]vocab.Mapping, error)
	DecideMapping(ctx context.Context, id, status, author, why string, now time.Time) error
}

// Vocabulary is what the server needs to take mapping decisions: the store,
// and what the next boot builds its registry from, so that a decision is
// checked the way that boot will check it (ADR 13: "a mapping decision can't
// stop Bearing booting").
type Vocabulary struct {
	Store MappingStore
	// Base is the registry modules extend; nil means model.Core().
	Base *model.Registry
	// Modules are every module loaded at boot besides the core.
	Modules []vocab.Module
}

// MappingDecision is a request to accept or reject a proposed mapping.
type MappingDecision struct {
	// Decision is "accept" or "reject".
	Decision string
	ID       string
	Why      string
}

// Validate reports, as an error wrapping graph.ErrInvalidDecision, why d is
// not a decision. A decision is kept with its why, so one is required.
func (d MappingDecision) Validate() error {
	switch {
	case d.Decision != "accept" && d.Decision != "reject":
		return fmt.Errorf("%w: decision must be accept or reject", graph.ErrInvalidDecision)
	case d.ID == "":
		return fmt.Errorf("%w: %s takes a mapping ID", graph.ErrInvalidDecision, d.Decision)
	case strings.TrimSpace(d.Why) == "":
		return fmt.Errorf("%w: %s needs a why, kept with the decision", graph.ErrInvalidDecision, d.Decision)
	}
	return nil
}

// openProposal returns mapping id's open proposal, newest version first, and
// every mapping row.
func (s *Server) openProposal(ctx context.Context, id string) (vocab.Mapping, []vocab.Mapping, error) {
	all, err := s.cfg.Vocab.Store.Mappings(ctx)
	if err != nil {
		return vocab.Mapping{}, nil, err
	}
	var best *vocab.Mapping
	for i, m := range all {
		if m.ID == id && m.Status == vocab.Proposed && m.ValidTo == nil && (best == nil || m.Version > best.Version) {
			best = &all[i]
		}
	}
	if best == nil {
		return vocab.Mapping{}, nil, fmt.Errorf("mapping %s: %w: no open proposal", model.Clip(id), store.ErrNotFound)
	}
	return *best, all, nil
}

// checkMapping is the boot check run before a decision: the registry and the
// rules as the next boot would build them with proposal id asserted now. It
// fails with ErrMappingRefused, naming the proposed mappings it needs first.
func (s *Server) checkMapping(ctx context.Context, id string) error {
	p, all, err := s.openProposal(ctx, id)
	if err != nil {
		return err
	}
	now := s.clk.Now().UTC()
	a := p
	a.Status, a.ValidFrom, a.ValidTo = vocab.Asserted, now, nil
	ms := append(slices.DeleteFunc(slices.Clone(all), func(m vocab.Mapping) bool {
		return m.ID == id && m.Status == vocab.Proposed && m.ValidTo == nil
	}), a)
	base := s.cfg.Vocab.Base
	if base == nil {
		base = model.Core()
	}
	_, err = vocab.Registry(base, s.cfg.Vocab.Modules, ms, now)
	if err == nil {
		_, err = vocab.Compile(ms)
	}
	if err == nil {
		return nil
	}
	if needs := s.needsFirst(p, all, now); len(needs) > 0 {
		return fmt.Errorf("%w: cannot accept %s yet: accept %s first", ErrMappingRefused, id, strings.Join(needs, ", "))
	}
	return fmt.Errorf("%w: cannot accept %s: it would stop Bearing booting: %w", ErrMappingRefused, id, err)
}

// needsFirst names the proposed broader mappings that p's shape check waits
// on: an exact_match needs every kind in its term's domain and range to be
// broader than a core kind first.
func (s *Server) needsFirst(p vocab.Mapping, all []vocab.Mapping, now time.Time) []string {
	if p.Predicate != vocab.ExactMatch {
		return nil
	}
	ns, _, _ := vocab.Split(p.Subject)
	var kinds []string
	for _, m := range s.cfg.Vocab.Modules {
		for _, t := range m.Terms {
			if vocab.Full(m.Namespace, t.Name) != p.Subject {
				continue
			}
			for _, n := range append(slices.Clone(t.Domain), t.Range...) {
				switch tns, term, ok := vocab.Split(n); {
				case !ok:
					n = vocab.Full(ns, n)
				case tns == "core":
					n = vocab.Full(vocab.CoreNamespace, term)
				}
				kinds = append(kinds, n)
			}
		}
	}
	var needs []string
	for _, m := range all {
		if m.Predicate == vocab.Broader && m.Status == vocab.Proposed && m.ValidTo == nil && slices.Contains(kinds, m.Subject) && !slices.Contains(needs, m.ID) &&
			!slices.ContainsFunc(all, func(o vocab.Mapping) bool {
				return o.Predicate == vocab.Broader && o.Subject == m.Subject && o.InForce(now)
			}) {
			needs = append(needs, m.ID)
		}
	}
	slices.Sort(needs)
	return needs
}

// Mappings lists the open mappings with status, oldest first.
func (s *Server) Mappings(ctx context.Context, status string) ([]vocab.Mapping, error) {
	if s.cfg.Vocab == nil {
		return nil, ErrNoVocabulary
	}
	all, err := s.cfg.Vocab.Store.Mappings(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(m vocab.Mapping) bool { return m.ValidTo != nil || m.Status != status }), nil
}

// DecideMapping puts a person's decision on a proposed mapping on the log and
// returns once it is there; it is applied in order and, keyed by its event ID,
// once. An accept that would stop Bearing booting is refused before anything
// is logged, with ErrMappingRefused; a mapping with no open proposal fails
// with store.ErrNotFound; a request that cannot be carried out, with
// graph.ErrInvalidDecision.
func (s *Server) DecideMapping(ctx context.Context, d MappingDecision, actor *eventv1alpha1.Actor) (Logged, error) {
	if s.cfg.Vocab == nil {
		return Logged{}, ErrNoVocabulary
	}
	if actor.GetSubject() == "" || actor.GetAgent() {
		return Logged{}, fmt.Errorf("%w: only a person decides", graph.ErrInvalidDecision)
	}
	if err := d.Validate(); err != nil {
		return Logged{}, err
	}
	if d.Decision == "accept" {
		if err := s.checkMapping(ctx, d.ID); err != nil {
			return Logged{}, err
		}
	} else if _, _, err := s.openProposal(ctx, d.ID); err != nil {
		return Logged{}, err
	}
	data, err := model.EncodeJSON(&eventv1alpha1.MappingDecided{
		MappingId: d.ID, Decision: mappingDecisions[d.Decision], Reason: clipReason(d.Why), Actor: actor,
	})
	if err != nil {
		return Logged{}, fmt.Errorf("encode mapping decision: %w", err)
	}
	ev := store.Event{
		ID: string(vocabPartition) + "/" + s.cfg.NewID(), Partition: vocabPartition,
		Type: typeMappingDecided, Time: s.clk.Now().UTC(), Data: data,
	}
	stamp(ctx, &ev)
	got, err := s.cfg.Log.Append(ctx, []store.Event{ev})
	if err != nil {
		return Logged{}, fmt.Errorf("append mapping decision: %w", err)
	}
	if len(got) != 1 {
		return Logged{}, errors.New("append mapping decision: the event log returned no position")
	}
	s.wakeUp()
	return Logged{ID: ev.ID, Offset: got[0].Offset}, nil
}

// handleMappingDecided applies one decision. The boot check runs again, since
// the mappings may have changed since the request; a decision that no longer
// passes is dead-lettered, never applied. A mapping already decided, as when
// the event is applied twice, changes nothing.
func (s *Server) handleMappingDecided(ctx context.Context, e store.Entry) error {
	msg := &eventv1alpha1.MappingDecided{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		return permanentError{fmt.Errorf("event %s: %w", e.ID, err)}
	}
	if e.Partition != vocabPartition {
		return permanentError{fmt.Errorf("event %s is in partition %q, not %q", e.ID, e.Partition, vocabPartition)}
	}
	if s.cfg.Vocab == nil {
		return fmt.Errorf("event %s: %w", e.ID, ErrNoVocabulary)
	}
	if msg.GetActor().GetSubject() == "" || msg.GetActor().GetAgent() {
		return permanentError{fmt.Errorf("event %s: not decided by a person", e.ID)}
	}
	status := map[eventv1alpha1.MappingDecided_Decision]string{
		eventv1alpha1.MappingDecided_DECISION_ACCEPT: vocab.Asserted, eventv1alpha1.MappingDecided_DECISION_REJECT: vocab.Rejected,
	}[msg.GetDecision()]
	if status == "" {
		return permanentError{fmt.Errorf("event %s: decision %s is not accept or reject", e.ID, msg.GetDecision())}
	}
	id := msg.GetMappingId()
	if status == vocab.Asserted {
		if err := s.checkMapping(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			if errors.Is(err, ErrMappingRefused) {
				return permanentError{fmt.Errorf("event %s: %w", e.ID, err)}
			}
			return fmt.Errorf("event %s: %w", e.ID, err)
		}
	}
	err := s.cfg.Vocab.Store.DecideMapping(ctx, id, status, msg.GetActor().GetSubject(), clipReason(msg.GetReason()), e.Time)
	if errors.Is(err, store.ErrNotFound) {
		telemetry.Logger(pkgName).WarnContext(ctx, "a mapping decision changed nothing: the mapping was already decided, or the event was applied before",
			"event", e.ID, "decision", msg.GetDecision().String(), "mapping", model.Clip(id))
		return nil
	}
	if err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	return nil
}
