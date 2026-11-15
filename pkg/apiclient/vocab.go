package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Mapping is a mapping as the server lists it (ADR 13, spike).
type Mapping struct {
	ID            string         `json:"id"`
	Version       int            `json:"version"`
	Subject       string         `json:"subject"`
	Predicate     string         `json:"predicate"`
	Object        string         `json:"object"`
	Target        *MappingTarget `json:"target,omitempty"`
	Guess         bool           `json:"guess,omitempty"`
	Status        string         `json:"status"`
	Author        string         `json:"author,omitempty"`
	Justification string         `json:"justification,omitempty"`
	ValidFrom     time.Time      `json:"valid_from"`
}

// MappingTarget is what a derives rule makes.
type MappingTarget struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

// Mappings lists the open mappings with status (proposed when empty).
func (c *Client) Mappings(ctx context.Context, status string) ([]Mapping, error) {
	v := url.Values{}
	set(v, "status", status)
	var a struct {
		Mappings []Mapping `json:"mappings"`
	}
	if err := c.get(ctx, "/v1/mappings", v, &a); err != nil {
		return nil, err
	}
	return a.Mappings, nil
}

// MappingDecision is a person's decision on a proposed mapping: Decision is
// accept or reject.
type MappingDecision struct {
	Decision string `json:"decision"`
	ID       string `json:"id"`
	Why      string `json:"why"`
}

// DecideMapping sends a decision, which the server checks against what the
// next boot needs before it logs it. It answers once the decision is on the
// event log; a worker applies it later.
func (c *Client) DecideMapping(ctx context.Context, d MappingDecision) (*DecisionAccepted, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("apiclient: %w", err)
	}
	var a DecisionAccepted
	if err := c.do(ctx, http.MethodPost, "/v1/mappings/decisions", bytes.NewReader(b), &a); err != nil {
		return nil, err
	}
	a.EventID = cleanText(a.EventID, 64)
	return &a, nil
}
