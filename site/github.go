package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// GitHub reads milestones and issues from the REST API. It needs no token
// for a public repository; a token only raises the rate limit.
type GitHub struct {
	Repo  string
	Token string
	// API is the REST base URL; empty means https://api.github.com.
	API  string
	HTTP *http.Client
}

// Milestone is a GitHub milestone with its issue counts.
type Milestone struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	URL         string `json:"html_url"`
	Open        int    `json:"open_issues"`
	Closed      int    `json:"closed_issues"`
}

// Issue is a GitHub issue. Pull requests are dropped when listing.
type Issue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	URL       string `json:"html_url"`
	Milestone *struct {
		Number int `json:"number"`
	} `json:"milestone"`
	PullRequest *struct{} `json:"pull_request"`
}

// GitHubData is everything the roadmap reads from GitHub.
type GitHubData struct {
	Milestones []Milestone
	Issues     map[int]Issue
}

// maxIssuePages bounds the issue listing at 10,000 issues.
const maxIssuePages = 100

// Fetch reads every milestone and every issue, open and closed.
func (g *GitHub) Fetch(ctx context.Context) (*GitHubData, error) {
	var ms []Milestone
	if err := g.get(ctx, "/repos/"+g.Repo+"/milestones?state=all&per_page=100", &ms); err != nil {
		return nil, fmt.Errorf("milestones: %w", err)
	}
	issues := map[int]Issue{}
	for page := 1; page <= maxIssuePages; page++ {
		var batch []Issue
		path := "/repos/" + g.Repo + "/issues?state=all&per_page=100&page=" + strconv.Itoa(page)
		if err := g.get(ctx, path, &batch); err != nil {
			return nil, fmt.Errorf("issues page %d: %w", page, err)
		}
		for _, is := range batch {
			if is.PullRequest == nil {
				issues[is.Number] = is
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	return &GitHubData{Milestones: ms, Issues: issues}, nil
}

func (g *GitHub) get(ctx context.Context, path string, into any) error {
	base := g.API
	if base == "" {
		base = "https://api.github.com"
	}
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body can echo request details; keep only the status.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(into)
}
