// Package contracts defines the interfaces between Bearing's components.
// Each interface has one default implementation and can be backed by any
// other technology that passes its conformance suite. One backend may serve
// several interfaces; by default SurrealDB serves both GraphStore and
// VectorIndex (see docs/adr/0005-one-store-to-start.md and pkg/store).
//
//	Interface      Default         Alternatives
//	GraphStore     SurrealDB       PostgreSQL, Neo4j, Apache AGE, Memgraph
//	VectorIndex    SurrealDB       Qdrant, pgvector, OpenSearch, Weaviate
//	EventBus       NATS JetStream  Kafka, SQS/SNS, Postgres queue
//	Extractor      any LLM API     hosted or local models
//	Judge          Kev 4B          Jev hosted API
//	PolicyDecider  OPA             Cedar
//	Executor       Temporal        Postgres job runner
package contracts

import (
	"context"
	"errors"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("not found")

// VectorPoint is one embedded item in the semantic index. Every point refers
// back to a graph subject; the index is never the source of truth.
type VectorPoint struct {
	ID        string         `json:"id"`
	SubjectID SubjectID      `json:"subject_id"`
	Vector    []float32      `json:"vector"`
	Text      string         `json:"text,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// VectorQuery searches the index.
type VectorQuery struct {
	Vector []float32
	Limit  int
	// Kinds filters on each point's "kind" payload field.
	Kinds []model.Kind
}

// VectorHit is one search result.
type VectorHit struct {
	Point VectorPoint `json:"point"`
	Score float32     `json:"score"`
}

// VectorIndex is the semantic index over subjects and documents.
type VectorIndex interface {
	Upsert(ctx context.Context, points []VectorPoint) error
	Search(ctx context.Context, q VectorQuery) ([]VectorHit, error)
	DeleteBySubject(ctx context.Context, id SubjectID) error
	// Repoint moves every point of subject from to subject to, after a
	// merge (ApplyResult.Merges).
	Repoint(ctx context.Context, from, to SubjectID) error
}

// EventBus carries observations and change events between components as
// CloudEvents. Delivery is at least once; handlers must be idempotent.
type EventBus interface {
	Publish(ctx context.Context, topic string, event []byte) error
	// Subscribe calls handle for each event until ctx is cancelled. Returning
	// an error from handle asks for redelivery.
	Subscribe(ctx context.Context, topic, group string, handle func(context.Context, []byte) error) error
}

// Document is unstructured text for candidate extraction.
type Document struct {
	Source model.Key `json:"source"`
	Title  string    `json:"title,omitempty"`
	Text   string    `json:"text"`
	URL    string    `json:"url,omitempty"`
}

// Candidate is an entity or relation proposed by an extractor. Candidates
// are never written to the graph until a Judge scores them.
type Candidate struct {
	Entity    *modelv1alpha1.Entity     `json:"entity"`
	Relations []*modelv1alpha1.Relation `json:"relations,omitempty"`
	// Span quotes the text that supports the candidate.
	Span string `json:"span"`
}

// Extractor proposes candidates from unstructured text, usually with an LLM.
type Extractor interface {
	Extract(ctx context.Context, doc Document) ([]Candidate, error)
}

// QuestionType is the shape of a judgment question.
type QuestionType string

// The question types a Judge answers.
const (
	QuestionChoice QuestionType = "choice"
	QuestionYesNo  QuestionType = "yes_no"
	QuestionScore  QuestionType = "score"
)

// Question is one typed judgment. All questions in a request share its state.
type Question struct {
	ID      string       `json:"id"`
	Type    QuestionType `json:"type"`
	Prompt  string       `json:"prompt"`
	Options []string     `json:"options,omitempty"` // choice and score levels
}

// JudgeRequest asks several questions about one state object.
type JudgeRequest struct {
	State     map[string]any `json:"state"`
	Questions []Question     `json:"questions"`
}

// Answer holds calibrated probabilities for one question.
type Answer struct {
	// Probabilities per option for choice and score questions.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// P is the probability of "yes" for yes/no questions.
	P float64 `json:"p,omitempty"`
	// Expected is the expected level for score questions.
	Expected float64 `json:"expected,omitempty"`
}

// Judge returns calibrated typed judgments, for example "are these two keys
// the same person?" or "does this contradict the current owner?".
type Judge interface {
	Judge(ctx context.Context, req JudgeRequest) (map[string]Answer, error)
}

// Actor is who asked for something: a person or an agent acting for one.
type Actor struct {
	Subject string    `json:"subject"` // OIDC subject or agent token id
	Person  SubjectID `json:"person,omitempty"`
	Agent   bool      `json:"agent"`
}

// ActionRequest asks to run one named action against a subject.
type ActionRequest struct {
	Action string         `json:"action"` // e.g. "rollback"
	Target SubjectID      `json:"target"`
	Params map[string]any `json:"params,omitempty"`
	Actor  Actor          `json:"actor"`
}

// Decision is a policy outcome. A denial always says why and how to fix it.
type Decision struct {
	Allow            bool   `json:"allow"`
	RequiresApproval bool   `json:"requires_approval"`
	Reason           string `json:"reason"`
	Fix              string `json:"fix,omitempty"`
}

// PolicyDecider evaluates a request against policy with graph facts as input.
type PolicyDecider interface {
	Decide(ctx context.Context, req ActionRequest, facts []*modelv1alpha1.FactState) (Decision, error)
}

// Plan describes what an action will do before it runs.
type Plan struct {
	ID      string   `json:"id"`
	Summary string   `json:"summary"`
	Steps   []string `json:"steps"`
	// Notifies lists subjects (teams, channels) told when the action runs.
	Notifies []SubjectID `json:"notifies,omitempty"`
}

// Outcome is the result of applying or verifying a plan.
type Outcome struct {
	PlanID  string `json:"plan_id"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail"`
}

// Executor runs actions durably: plan, apply, verify and roll back.
type Executor interface {
	Plan(ctx context.Context, req ActionRequest) (Plan, error)
	Apply(ctx context.Context, plan Plan) (Outcome, error)
	Verify(ctx context.Context, plan Plan) (Outcome, error)
	Rollback(ctx context.Context, plan Plan) (Outcome, error)
}
