package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
	"bearing.example/pkg/telemetry"
)

// identityPartition holds the decisions people make on identity links. One
// partition means one worker applies them in the order they were logged.
const identityPartition store.Partition = "core/identity"

// maxReasonBytes bounds the free text that comes with a decision.
const maxReasonBytes = 1024

// LinkDecisions says which callers may decide identity links (the setting
// api.link_decisions).
type LinkDecisions string

// The values of LinkDecisions.
const (
	// LinkDecisionsOff refuses every decision.
	LinkDecisionsOff LinkDecisions = "off"
	// LinkDecisionsSocket takes decisions only from callers on the Unix
	// socket: a person on the server's host. It is the default.
	LinkDecisionsSocket LinkDecisions = "socket"
	// LinkDecisionsToken also takes them from holders of the API token.
	LinkDecisionsToken LinkDecisions = "token"
)

// Valid reports whether m is one of the values above or empty (the default).
func (m LinkDecisions) Valid() bool {
	switch m {
	case "", LinkDecisionsOff, LinkDecisionsSocket, LinkDecisionsToken:
		return true
	}
	return false
}

// allows reports whether a caller may decide under m. An agent never may.
func (m LinkDecisions) allows(c Caller) bool {
	switch {
	case c.Agent:
		return false
	case m == LinkDecisionsToken:
		return true
	case m == "" || m == LinkDecisionsSocket:
		return c.Local
	}
	return false
}

// permanentError marks a failure that trying again cannot fix, so the worker
// dead-letters the event at once.
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// ErrNoLinkDecider means the server was not given a LinkDecider.
var ErrNoLinkDecider = errors.New("server: link decisions are not configured")

// LinkDecider carries out a person's decision on a link; graph.Graph is one.
type LinkDecider interface {
	Decide(ctx context.Context, d graph.Decision) (graph.LinkResult, error)
}

// LinkDecision is a request to decide a link, as DecideLink takes it.
type LinkDecision struct {
	Kind graph.DecisionKind
	// LinkID is for Accept, Reject and UndoDistinct; A and B for Join and
	// Split.
	LinkID, A, B string
	Reason       string
}

// Validate reports, as an error wrapping graph.ErrInvalidDecision, why d is
// not a decision: an unknown kind, or the wrong fields for its kind.
func (d LinkDecision) Validate() error {
	if _, ok := decisionEnums[d.Kind]; !ok {
		return fmt.Errorf("%w: decision must be accept, reject, join, split or undo_distinct", graph.ErrInvalidDecision)
	}
	if d.byLink() && (d.LinkID == "" || d.A != "" || d.B != "") {
		return fmt.Errorf("%w: %s takes a link ID alone", graph.ErrInvalidDecision, d.Kind)
	}
	if !d.byLink() && (d.LinkID != "" || d.A == "" || d.B == "") {
		return fmt.Errorf("%w: %s takes two records and no link ID", graph.ErrInvalidDecision, d.Kind)
	}
	return nil
}

// byLink says whether the decision acts on a link, as against two records.
func (d LinkDecision) byLink() bool {
	return d.Kind == graph.Accept || d.Kind == graph.Reject || d.Kind == graph.UndoDistinct
}

// Logged names an event that is on the log.
type Logged struct {
	ID     string
	Offset store.Offset
}

var decisionEnums = map[graph.DecisionKind]eventv1alpha1.LinkDecided_Decision{
	graph.Accept:       eventv1alpha1.LinkDecided_DECISION_ACCEPT,
	graph.Reject:       eventv1alpha1.LinkDecided_DECISION_REJECT,
	graph.Join:         eventv1alpha1.LinkDecided_DECISION_JOIN,
	graph.Split:        eventv1alpha1.LinkDecided_DECISION_SPLIT,
	graph.UndoDistinct: eventv1alpha1.LinkDecided_DECISION_UNDO_DISTINCT,
}

// DecideLink puts a person's decision on the log, and returns once it is
// there. The decision is applied in order with the other identity decisions
// and, being keyed by its event ID, once. Only a person decides: an actor
// marked as an agent is refused. It fails with graph.ErrInvalidDecision for a
// request that cannot be carried out as given, ErrNoLinkDecider when the
// server could not apply it.
func (s *Server) DecideLink(ctx context.Context, d LinkDecision, actor *eventv1alpha1.Actor) (Logged, error) {
	if s.cfg.Links == nil {
		return Logged{}, ErrNoLinkDecider
	}
	if actor.GetSubject() == "" || actor.GetAgent() {
		return Logged{}, fmt.Errorf("%w: only a person decides", graph.ErrInvalidDecision)
	}
	if err := d.Validate(); err != nil {
		return Logged{}, err
	}
	enum := decisionEnums[d.Kind]
	msg := &eventv1alpha1.LinkDecided{A: d.A, B: d.B, Decision: enum, LinkId: d.LinkID, Reason: clipReason(d.Reason), Actor: actor}
	typ, _ := model.EventType(msg)
	data, err := model.EncodeJSON(msg)
	if err != nil {
		return Logged{}, fmt.Errorf("encode decision: %w", err)
	}
	ev := store.Event{
		ID: string(identityPartition) + "/" + s.cfg.NewID(), Partition: identityPartition,
		Type: typ, Time: s.clk.Now().UTC(), Data: data,
	}
	got, err := s.cfg.Log.Append(ctx, []store.Event{ev})
	if err != nil {
		return Logged{}, fmt.Errorf("append decision: %w", err)
	}
	if len(got) != 1 {
		return Logged{}, errors.New("append decision: the event log returned no position")
	}
	s.wakeUp()
	return Logged{ID: ev.ID, Offset: got[0].Offset}, nil
}

// clipReason cuts a reason to maxReasonBytes of valid UTF-8, without control
// characters other than newline and tab.
func clipReason(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if badReasonRune(r) {
			return -1
		}
		return r
	}, s)
	if len(s) <= maxReasonBytes {
		return s
	}
	cut := maxReasonBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// badReasonRune reports a control character a reason may not hold: people
// and agents read reasons, so nothing in one may move a cursor or hide text.
func badReasonRune(r rune) bool {
	return r != '\n' && r != '\t' && (unicode.IsControl(r) || r == '\u2028' || r == '\u2029')
}

// handleLinkDecided applies one decision. It holds identityMu, so a matching
// run never reads the links half way through a decision, nor writes proposals
// from a view that a decision has since changed.
func (s *Server) handleLinkDecided(ctx context.Context, e store.Entry) error {
	msg := &eventv1alpha1.LinkDecided{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		return permanentError{fmt.Errorf("event %s: %w", e.ID, err)}
	}
	if e.Partition != identityPartition {
		return permanentError{fmt.Errorf("event %s is in partition %q, not %q", e.ID, e.Partition, identityPartition)}
	}
	if s.cfg.Links == nil {
		return fmt.Errorf("event %s: %w", e.ID, ErrNoLinkDecider)
	}
	if msg.GetActor().GetSubject() == "" || msg.GetActor().GetAgent() {
		return permanentError{fmt.Errorf("event %s: not decided by a person", e.ID)}
	}
	var kind graph.DecisionKind
	for k, v := range decisionEnums {
		if v == msg.GetDecision() {
			kind = k
		}
	}
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	res, err := s.cfg.Links.Decide(ctx, graph.Decision{
		ID: e.ID, Kind: kind, LinkID: msg.GetLinkId(), A: msg.GetA(), B: msg.GetB(),
		Actor: msg.GetActor().GetSubject(), Reason: clipReason(msg.GetReason()), At: e.Time,
	})
	if errors.Is(err, graph.ErrInvalidDecision) || errors.Is(err, graph.ErrNotFound) {
		return permanentError{fmt.Errorf("event %s: %w", e.ID, err)}
	}
	if err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	if res == (graph.LinkResult{}) {
		telemetry.Logger(pkgName).WarnContext(ctx, "a link decision changed nothing: the link was already in that state, or the event was applied before",
			"event", e.ID, "decision", string(kind), "link", model.Clip(msg.GetLinkId()))
	}
	return nil
}
