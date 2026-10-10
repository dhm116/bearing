package query_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/query"
)

// A question that names no window is about the last day, and says so.
func TestChangesDefaultToTheLastDay(t *testing.T) {
	r := newRig(t)
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.clock.Advance(30 * time.Hour)
	r.observe("catalog-acme", "Repository", "catalog:repo/payments", nil, "payments")
	now := r.clock.Now()

	got, err := r.q.Changes(context.Background(), query.ChangesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.DefaultWindow || !got.Until.Equal(now) || !got.Since.Equal(now.Add(-query.DefaultChangesWindow)) {
		t.Errorf("window %v to %v (default %v), want the last 24 hours up to %v", got.Since, got.Until, got.DefaultWindow, now)
	}
	if len(got.Changes) == 0 {
		t.Fatal("no changes, want the repository's")
	}
	for _, c := range got.Changes {
		if c.Subject.Name != "payments" || !c.ChangedAt.After(got.Since) {
			t.Errorf("change %+v: want only what changed in the last day, about payments", c)
		}
	}
	if got.NextPageToken != "" || got.MostRecentChange != nil {
		t.Errorf("token %q and most recent change %v on a page with changes, want neither", got.NextPageToken, got.MostRecentChange)
	}

	// Naming either end is a window of the caller's own.
	own, err := r.q.Changes(context.Background(), query.ChangesRequest{Since: now.Add(-48 * time.Hour)})
	if err != nil || own.DefaultWindow || len(own.Changes) <= len(got.Changes) {
		t.Errorf("a named start: got %d changes (default %v), %v; want more than the default's %d", len(own.Changes), own.DefaultWindow, err, len(got.Changes))
	}
}

// When nothing changed in the window, the answer says when the newest change
// before it was; with no change ever it says nothing of the kind.
func TestChangesSayWhenTheLastOneWasWhenNothingChanged(t *testing.T) {
	r := newRig(t)
	got, err := r.q.Changes(context.Background(), query.ChangesRequest{})
	if err != nil || len(got.Changes) != 0 || got.MostRecentChange != nil || got.NextPageToken != "" {
		t.Fatalf("an empty graph: got %+v, %v, want nothing at all", got, err)
	}
	seen := r.clock.Now()
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.clock.Advance(72 * time.Hour)

	got, err = r.q.Changes(context.Background(), query.ChangesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 0 || got.MostRecentChange == nil || !got.MostRecentChange.Equal(seen) {
		t.Errorf("got %d changes and most recent %v, want none and %v", len(got.Changes), got.MostRecentChange, seen)
	}
	// A window of the caller's own gets the same help.
	got, err = r.q.Changes(context.Background(), query.ChangesRequest{Since: seen.Add(time.Minute), Until: seen.Add(time.Hour)})
	if err != nil || len(got.Changes) != 0 || got.MostRecentChange == nil || !got.MostRecentChange.Equal(seen) {
		t.Errorf("a window after the change: got %d changes and %v, %v, want none and %v", len(got.Changes), got.MostRecentChange, err, seen)
	}
}

// Pages are read on the window of the first, a subject keeps its scope, and
// together they are the whole answer in the order a single page would give.
func TestChangesPageThroughAWindow(t *testing.T) {
	r := newRig(t)
	start := r.clock.Now().Add(-time.Hour)
	for i := range 7 {
		r.observe("catalog-acme", "Repository", fmt.Sprintf("catalog:repo/svc-%d", i), nil, fmt.Sprintf("svc-%d", i))
	}
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	whole, err := r.q.Changes(context.Background(), query.ChangesRequest{Since: start, Limit: 1000})
	if err != nil || len(whole.Changes) < 8 || whole.NextPageToken != "" {
		t.Fatalf("the whole answer: got %d changes, token %q, %v", len(whole.Changes), whole.NextPageToken, err)
	}
	want := factIDs(whole.Changes)
	if !slices.IsSortedFunc(whole.Changes, func(a, b query.Change) int { return b.ChangedAt.Compare(a.ChangedAt) }) {
		t.Error("changes are not newest first")
	}

	var (
		got   []string
		token string
		first *query.Changes
	)
	for pages := 0; ; pages++ {
		req := query.ChangesRequest{Since: start, Limit: 3}
		if token != "" {
			req = query.ChangesRequest{Limit: 3, PageToken: token}
		}
		page, err := r.q.Changes(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = page
		} else if !page.Since.Equal(first.Since) || !page.Until.Equal(first.Until) || page.Axis != first.Axis {
			t.Errorf("page %d is on %v to %v, want the first page's %v to %v", pages, page.Since, page.Until, first.Since, first.Until)
		}
		if len(page.Changes) > 3 {
			t.Fatalf("page %d: %d changes, want at most 3", pages, len(page.Changes))
		}
		got = append(got, factIDs(page.Changes)...)
		// Whatever happens in the meantime does not move the window.
		r.clock.Advance(time.Hour)
		r.observe("catalog-acme", "Team", fmt.Sprintf("catalog:team/late-%d", pages), nil, "Late")
		if token = page.NextPageToken; token == "" {
			break
		}
		if pages > len(want) {
			t.Fatal("paging does not end")
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("pages hold %v, want %v", got, want)
	}

	// A subject's pages stay about that subject.
	var perSubject []string
	req := query.ChangesRequest{Ref: "catalog:repo/svc-2", Since: start, Limit: 1}
	for {
		page, err := r.q.Changes(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Changes {
			if c.Subject.Name != "svc-2" {
				t.Errorf("change %+v on a page about svc-2", c)
			}
			perSubject = append(perSubject, c.FactID)
		}
		if page.NextPageToken == "" {
			break
		}
		req = query.ChangesRequest{Limit: 1, PageToken: page.NextPageToken}
	}
	if len(perSubject) == 0 {
		t.Error("no changes about svc-2")
	}
}

func factIDs(cs []query.Change) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.FactID
	}
	return out
}

// The record axis pages the same way.
func TestChangesPageOnTheRecordAxis(t *testing.T) {
	r := newRig(t)
	start := r.clock.Now().Add(-time.Hour)
	for i := range 4 {
		r.observe("catalog-acme", "Repository", fmt.Sprintf("catalog:repo/svc-%d", i), nil, fmt.Sprintf("svc-%d", i))
	}
	page, err := r.q.Changes(context.Background(), query.ChangesRequest{Since: start, Axis: query.AxisRecord, Limit: 2})
	if err != nil || len(page.Changes) != 2 || page.NextPageToken == "" || page.Axis != query.AxisRecord {
		t.Fatalf("got %d changes, token %q, axis %q, %v; want 2, a token, record", len(page.Changes), page.NextPageToken, page.Axis, err)
	}
	next, err := r.q.Changes(context.Background(), query.ChangesRequest{PageToken: page.NextPageToken})
	if err != nil || next.Axis != query.AxisRecord || len(next.Changes) == 0 {
		t.Fatalf("next page: got %+v, %v, want more on the record axis", next, err)
	}
}

func TestChangesRefuseBadTokensAndLimits(t *testing.T) {
	r := newRig(t)
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.observe("catalog-acme", "Team", "catalog:team/infra", nil, "Infra")
	page, err := r.q.Changes(context.Background(), query.ChangesRequest{Since: r.clock.Now().Add(-3 * time.Hour), Limit: 1})
	if err != nil || page.NextPageToken == "" {
		t.Fatalf("got %+v, %v, want a first page with a token", page, err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	valid, err := base64.RawURLEncoding.DecodeString(page.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"not base64": "not a token!",
		"not JSON":   enc("hello"),
		// A valid token padded with JSON whitespace: only its length is wrong.
		"too long":         enc(string(valid) + strings.Repeat(" ", 512)),
		"trailing data":    enc(string(valid) + "{}"),
		"no start":         enc(regexp.MustCompile(`"since":"[^"]*"`).ReplaceAllString(string(valid), `"since":"0001-01-01T00:00:00Z"`)),
		"another version":  enc(strings.Replace(string(valid), `"v":1`, `"v":2`, 1)),
		"an unknown field": enc(strings.Replace(string(valid), `"v":1`, `"v":1,"extra":true`, 1)),
		"no cursor":        enc(`{"v":1,"axis":"valid","since":"2026-01-01T00:00:00Z","until":"2026-01-02T00:00:00Z"}`),
		"unknown axis":     enc(strings.Replace(string(valid), `"valid"`, `"sideways"`, 1)),
		"a long subject":   enc(strings.Replace(string(valid), `"changed_at"`, `"subject":"`+strings.Repeat("x", 130)+`","changed_at"`, 1)),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := r.q.Changes(context.Background(), query.ChangesRequest{PageToken: token}); !errors.Is(err, query.ErrBadPageToken) {
				t.Errorf("got %v, want a bad page token", err)
			}
		})
	}
	// A token carries its question.
	for name, req := range map[string]query.ChangesRequest{
		"a subject": {PageToken: page.NextPageToken, Ref: "catalog:team/infra"},
		"a start":   {PageToken: page.NextPageToken, Since: r.clock.Now()},
		"an axis":   {PageToken: page.NextPageToken, Axis: query.AxisRecord},
	} {
		if _, err := r.q.Changes(context.Background(), req); !errors.Is(err, query.ErrBadPageToken) {
			t.Errorf("a token with %s: got %v, want it refused", name, err)
		}
	}
	// The querier refuses a limit itself, before it asks the store.
	asked := &countingGraph{GraphStore: r.store}
	q := &query.Querier{Graph: asked, Now: r.clock.Now}
	for _, limit := range []int{-1, 1001} {
		if _, err := q.Changes(context.Background(), query.ChangesRequest{Limit: limit}); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Errorf("limit %d: got %v, want an error", limit, err)
		}
	}
	if asked.pages != 0 {
		t.Errorf("the store was asked for %d pages, want none for a bad limit", asked.pages)
	}
}

// countingGraph counts the pages asked of the store.
type countingGraph struct {
	contracts.GraphStore
	pages int
}

func (g *countingGraph) ChangesPage(ctx context.Context, req contracts.ChangesRequest) (contracts.ChangesPage, error) {
	g.pages++
	return g.GraphStore.ChangesPage(ctx, req)
}

// A page reached by a token that comes back empty keeps the question's
// default window and does not claim to name the newest change before it.
func TestChangesEmptyContinuationKeepsTheQuestion(t *testing.T) {
	r := newRig(t)
	r.observe("catalog-acme", "Team", "catalog:team/old", nil, "Old")
	r.clock.Advance(30 * time.Hour)
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.observe("catalog-acme", "Team", "catalog:team/infra", nil, "Infra")
	first, err := r.q.Changes(context.Background(), query.ChangesRequest{Limit: 1})
	if err != nil || first.NextPageToken == "" || !first.DefaultWindow {
		t.Fatalf("got %+v, %v, want a default-window first page with a token", first, err)
	}
	// A token whose cursor is at the window's start has nothing after it.
	raw, err := base64.RawURLEncoding.DecodeString(first.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	var tok map[string]any
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatal(err)
	}
	tok["changed_at"] = first.Since.Format(time.RFC3339Nano)
	raw, err = json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	next, err := r.q.Changes(context.Background(), query.ChangesRequest{PageToken: base64.RawURLEncoding.EncodeToString(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Changes) != 0 || !next.DefaultWindow || next.MostRecentChange != nil || next.NextPageToken != "" {
		t.Errorf("got %d changes, default %v, most recent %v, token %q; want an empty page that keeps the default window and names no most recent change",
			len(next.Changes), next.DefaultWindow, next.MostRecentChange, next.NextPageToken)
	}
}

// The largest token Changes issues stays well inside the bound it enforces:
// a subject ID, the record axis, the default flag and nanosecond times.
func TestChangesTheLargestTokenIsAccepted(t *testing.T) {
	r := newRig(t)
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.observe("catalog-acme", "Team", "catalog:team/infra", nil, "Infra")
	r.clock.Advance(123456789 * time.Nanosecond)
	first, err := r.q.Changes(context.Background(), query.ChangesRequest{Ref: "catalog:team/platform", Axis: query.AxisRecord, Limit: 1})
	if err != nil || first.NextPageToken == "" {
		t.Fatalf("got %+v, %v, want a token", first, err)
	}
	if n := len(first.NextPageToken); n > 400 {
		t.Errorf("a token is %d bytes; the bound is 512, so a legitimate token should leave room to grow", n)
	}
	if _, err := r.q.Changes(context.Background(), query.ChangesRequest{PageToken: first.NextPageToken}); err != nil {
		t.Errorf("the token Changes issued was refused: %v", err)
	}
}
