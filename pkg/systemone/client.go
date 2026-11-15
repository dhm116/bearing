// Package systemone is a client for the System One API (POST /v1/systemone)
// that TypeSafe's Jev and the open Kev models both serve (ADR 7). It asks
// multiple-choice questions and returns the model's calibrated probabilities; how that p
// weighs against other evidence is the caller's matcher's business.
package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Config says where the model is. Defaults are applied by New.
type Config struct {
	// BaseURL is the server's root, such as http://127.0.0.1:8000.
	BaseURL string
	// Model is sent as the request's model; empty means "kev-latest".
	Model string
	// APIKeyEnv names the environment variable holding a bearer token; empty
	// or unset sends none (a local Kev needs none).
	APIKeyEnv string
	// HTTP is the client to use; nil means http.DefaultClient. Its transport
	// is wrapped with otelhttp.
	HTTP *http.Client
	// Getenv reads APIKeyEnv; nil means os.Getenv.
	Getenv func(string) string
}

// Client asks a System One server questions.
type Client struct{ cfg Config }

// ChoiceAnswer is a multiple-choice question's reply.
type ChoiceAnswer struct {
	// Choice is the name picked; Probabilities are by name.
	Choice        string
	Probabilities map[string]float64
	// Model is the model id the server reports it served; Question is the
	// instruction asked, so evidence can name both.
	Model, Question string
	// InputTokens is the server's usage figure, when it reports one.
	InputTokens int
}

// New applies Config defaults.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("systemone: new: base URL is required")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Model == "" {
		cfg.Model = "kev-latest"
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	hc := http.DefaultClient
	if cfg.HTTP != nil {
		hc = cfg.HTTP
	}
	wrapped := *hc
	wrapped.Transport = otelhttp.NewTransport(hc.Transport)
	cfg.HTTP = &wrapped
	return &Client{cfg: cfg}, nil
}

const choiceID = "q"

// Choice asks one multiple-choice question about state (any JSON value):
// which of the criteria (name -> description) fits, with the probability of
// each name.
func (c *Client) Choice(ctx context.Context, state any, instructions string, criteria map[string]string) (ChoiceAnswer, error) {
	body, err := json.Marshal(map[string]any{
		"state": state, "model": c.cfg.Model,
		"questions": map[string]any{choiceID: map[string]any{"type": "choice", "instructions": instructions, "criteria": criteria}},
	})
	if err != nil {
		return ChoiceAnswer{}, fmt.Errorf("systemone: choice: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return ChoiceAnswer{}, fmt.Errorf("systemone: choice: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKeyEnv != "" {
		if k := c.cfg.Getenv(c.cfg.APIKeyEnv); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return ChoiceAnswer{}, fmt.Errorf("systemone: choice: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ChoiceAnswer{}, fmt.Errorf("systemone: choice: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ChoiceAnswer{}, fmt.Errorf("systemone: choice: server answered HTTP %d", resp.StatusCode)
	}
	var out struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type          string             `json:"type"`
			Choice        string             `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ChoiceAnswer{}, fmt.Errorf("systemone: choice: decode: %w", err)
	}
	a, ok := out.Answers[choiceID]
	if !ok || a.Type != "choice" {
		return ChoiceAnswer{}, errors.New("systemone: choice: response has no choice answer")
	}
	if _, ok := criteria[a.Choice]; !ok {
		return ChoiceAnswer{}, fmt.Errorf("systemone: choice: server picked %q, which was not offered", a.Choice)
	}
	return ChoiceAnswer{Choice: a.Choice, Probabilities: a.Probabilities, Model: out.Model, Question: instructions, InputTokens: out.Usage.InputTokens}, nil
}
