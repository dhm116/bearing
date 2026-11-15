// Package systemone is a client for the System One API (POST /v1/systemone)
// that TypeSafe's Jev and the open Kev models both serve (ADR 7). It asks
// yes/no questions and returns the model's calibrated p(true); how that p
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

// Answer is a yes/no question's reply.
type Answer struct {
	// P is p(true), between 0 and 1.
	P float64
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

const noulID = "q"

// Noul asks one yes/no question about state (any JSON value) and returns
// p(true).
func (c *Client) Noul(ctx context.Context, state any, question string) (Answer, error) {
	body, err := json.Marshal(map[string]any{
		"state": state, "model": c.cfg.Model,
		"questions": map[string]any{noulID: map[string]any{"type": "noul", "instructions": question}},
	})
	if err != nil {
		return Answer{}, fmt.Errorf("systemone: noul: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Answer{}, fmt.Errorf("systemone: noul: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKeyEnv != "" {
		if k := c.cfg.Getenv(c.cfg.APIKeyEnv); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return Answer{}, fmt.Errorf("systemone: noul: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Answer{}, fmt.Errorf("systemone: noul: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Answer{}, fmt.Errorf("systemone: noul: server answered HTTP %d", resp.StatusCode)
	}
	var out struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		} `json:"answers"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Answer{}, fmt.Errorf("systemone: noul: decode: %w", err)
	}
	a := out.Answers[noulID]
	if a.Type != "noul" || a.Noul == nil || *a.Noul < 0 || *a.Noul > 1 {
		return Answer{}, errors.New("systemone: noul: response has no noul probability")
	}
	return Answer{P: *a.Noul, Model: out.Model, Question: question, InputTokens: out.Usage.InputTokens}, nil
}
