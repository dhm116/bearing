package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

func TestSyncPagesByCursorWhateverThePageSize(t *testing.T) {
	keys := func(perPage int) []string {
		r := newRig(t, func(c *Config) { c.PerPage = perPage })
		obs, _ := r.sync()
		var out []string
		for _, o := range obs {
			out = append(out, o.GetData().GetEntity().GetKey())
		}
		// A person on several teams is reported once per team page.
		slices.Sort(out)
		return slices.Compact(out)
	}
	one, all := keys(1), keys(100)
	if len(all) == 0 || !slices.Equal(one, all) {
		t.Fatalf("page size 1 gave %v, page size 100 gave %v: want the same", one, all)
	}
}

func TestSyncObservedAtIsWhenTheRequestWasSent(t *testing.T) {
	r := newRig(t)
	n := 0
	r.a.Now = func() time.Time { n++; return fakes.Start.Add(time.Duration(n) * time.Second) }
	obs, _ := r.sync()

	// Five repositories at two a page: requests 1, 2 and 3. The teams page
	// is request 4 and its first team's members request 5.
	var repos, teamAt, personAt []time.Duration
	for _, o := range obs {
		at := o.GetTime().AsTime().Sub(fakes.Start)
		switch o.GetData().GetEntity().GetKind() {
		case string(model.KindRepository):
			repos = append(repos, at)
		case string(model.KindTeam):
			teamAt = append(teamAt, at)
		case string(model.KindPerson):
			personAt = append(personAt, at)
		}
	}
	sec := time.Second
	if want := []time.Duration{sec, sec, 2 * sec, 2 * sec, 3 * sec}; !slices.Equal(repos, want) {
		t.Errorf("repositories observed at %v, want the sends of their pages %v", repos, want)
	}
	if teamAt[0] != 4*sec || personAt[0] != 5*sec {
		t.Errorf("first team at %v and first person at %v, want the teams page send (4s) before the members send (5s)", teamAt[0], personAt[0])
	}
}

// TestSyncFailuresAreErrorsNeverCompleteSyncs: a refused or rate-limited
// read must not look like an empty org, which would end every fact.
func TestSyncFailuresAreErrorsNeverCompleteSyncs(t *testing.T) {
	canary := testkit.Canary("env:GITHUB_TOKEN")
	tests := []struct {
		name  string
		setup func(r *rig)
		want  string
	}{
		{"rate limited", func(r *rig) { r.srv.RateLimitNext(1) }, "rate limit exceeded"},
		{"bad token", func(r *rig) { r.a.Getenv = func(string) string { return canary } }, "401"},
		{"anonymous", func(r *rig) { r.a.Getenv = func(string) string { return "" } }, "401"},
		{"unknown org", func(r *rig) {
			r.cfg = json.RawMessage(`{"org":"nope","api_url":"` + r.srv.URL + `"}`)
		}, "could not resolve to an organization"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			tt.setup(r)
			obs, last, err := r.trySync()
			var rpcErr *adapter.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != adapter.CodeUpstream || !strings.Contains(strings.ToLower(rpcErr.Message), tt.want) {
				t.Fatalf("got %v, want an upstream error containing %q", err, tt.want)
			}
			if len(obs) != 0 || last.Done || last.CompleteSync != nil {
				t.Fatalf("got %d observations, done %v, complete_sync %v: want nothing", len(obs), last.Done, last.CompleteSync)
			}
			testkit.AssertNoLeaks(t, err.Error(), canary)
		})
	}

	t.Run("anonymous requests carry no credentials", func(t *testing.T) {
		r := newRig(t)
		r.a.Getenv = func(string) string { return "" }
		_, _, _ = r.trySync()
		reqs := r.srv.Requests()
		if len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "" {
			t.Fatalf("got %d requests, first with Authorization %q: want one anonymous request and no retry", len(reqs), reqs[0].Header.Get("Authorization"))
		}
	})

	t.Run("rate limited after the first page", func(t *testing.T) {
		r := newRig(t)
		first, err := r.a.Sync(context.Background(), adapter.SyncParams{Config: r.cfg})
		if err != nil || first.Done || len(first.Observations) == 0 {
			t.Fatalf("first page: %v, done %v", err, first.Done)
		}
		r.srv.RateLimitNext(1)
		second, err := r.a.Sync(context.Background(), adapter.SyncParams{Config: r.cfg, Cursor: first.NextCursor})
		if err == nil || !strings.Contains(err.Error(), "rate limit") || second.Done || len(second.Observations) != 0 {
			t.Fatalf("second page: %v, done %v, %d observations: want a rate limit error and nothing", err, second.Done, len(second.Observations))
		}
	})
}

func TestSyncRefusesLegacyNodeIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A server that ignores X-Github-Next-Global-ID.
		_, _ = w.Write([]byte(`{"data":{"organization":{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":null},
			"nodes":[{"id":"MDEwOlJlcG9zaXRvcnk1MjU3NzY0OTU=","databaseId":525776495,"name":"x","nameWithOwner":"acme/x","url":"https://github.com/acme/x"}]}}}}`))
	}))
	t.Cleanup(srv.Close)
	a := &Adapter{HTTP: srv.Client(), Now: time.Now, Getenv: func(string) string { return testToken }}
	_, err := a.Sync(context.Background(), adapter.SyncParams{Config: json.RawMessage(`{"org":"acme","api_url":"` + srv.URL + `"}`)})
	if err == nil || !strings.Contains(err.Error(), "next format") {
		t.Fatalf("got %v, want a refusal to emit a legacy node ID", err)
	}
}

func TestSyncUsesTheConfiguredNamespace(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Namespace = "ghes-acme" })
	obs, _ := r.sync()
	var refs []string
	for _, o := range obs {
		e := o.GetData().GetEntity()
		refs = append(refs, append([]string{e.GetKey()}, e.GetAliases()...)...)
		for _, rel := range o.GetData().GetRelations() {
			refs = append(refs, rel.GetTo()+rel.GetFrom())
		}
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "ghes-acme:") {
			t.Errorf("key %q is not in the ghes-acme namespace", ref)
		}
	}
	if len(refs) < 20 {
		t.Errorf("checked only %d keys", len(refs))
	}
}

func TestSyncRejectsBadParams(t *testing.T) {
	for name, p := range map[string]adapter.SyncParams{
		"no org":         {Config: json.RawMessage(`{}`)},
		"bad namespace":  {Config: json.RawMessage(`{"org":"acme","namespace":"GitHub"}`)},
		"bad cursor":     {Config: json.RawMessage(`{"org":"acme"}`), Cursor: "{"},
		"unknown phase":  {Config: json.RawMessage(`{"org":"acme"}`), Cursor: `{"phase":"people"}`},
		"malformed json": {Config: json.RawMessage(`{`)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newRig(t).a.Sync(context.Background(), p)
			var rpcErr *adapter.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != adapter.CodeInvalidParams {
				t.Fatalf("got %v, want invalid params", err)
			}
		})
	}
}

func TestSyncAllAcceptsEverySyncObservation(t *testing.T) {
	r := newRig(t)
	var n int
	err := adapter.SyncAll(context.Background(), r.a, r.cfg, 50, func(*eventv1alpha1.Observation) error { n++; return nil })
	if err != nil || n == 0 {
		t.Fatalf("SyncAll: %d observations, %v", n, err)
	}
}
