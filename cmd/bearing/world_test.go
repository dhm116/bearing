package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"bearing.example/adapters/github"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
)

const (
	githubSource    = "github-acme"
	directorySource = "authentik-acme"
	testToken       = "test-token"
	testSecret      = "test-secret"
)

// world is the fictional org read into an in-memory store the way the core
// will: the GitHub adapter's syncs and webhooks and the recorded directory
// feed go through the resolver. One fake clock drives the org, the adapter
// and the store, so record times are the story's times.
type world struct {
	t         testing.TB
	clock     *testkit.FakeClock
	org       *fakes.Org
	gh        *fakes.GitHub
	adapter   *github.Adapter
	cfg       json.RawMessage
	store     *memstore.Store
	resolver  *resolver.Resolver
	delivered int
	syncs     int
	next      int  // the next story step
	directory bool // the directory feed has been applied
}

func newWorld(t testing.TB) *world {
	t.Helper()
	clk := testkit.NewClock(fakes.Start)
	org := fakes.NewOrg(clk)
	srv := fakes.NewGitHub(t, org, fakes.GitHubOptions{Token: testToken, WebhookSecret: testSecret})
	cfg, err := json.Marshal(map[string]any{"org": "acme", "api_url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GITHUB_TOKEN": testToken, "GITHUB_WEBHOOK_SECRET": testSecret}
	a := &github.Adapter{HTTP: srv.Client(), Now: clk.Now, Getenv: func(k string) string { return env[k] }}
	s := memstore.New()
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	r, err := resolver.New(resolver.Config{
		Declarations: []*modelv1alpha1.AdapterDeclaration{declaration(t, "github"), declaration(t, "authentik")},
		Sources: map[string]*resolver.Source{
			githubSource: {Name: githubSource, Adapter: "github"},
			directorySource: {
				Name: directorySource, Adapter: "authentik",
				Issues: []resolver.Namespace{{Name: "authentik-saml", IssuerType: "saml"}},
				Links:  []resolver.Namespace{{Name: "github", IssuerType: "github"}},
			},
		},
	}, s)
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, clock: clk, org: org, gh: srv, adapter: a, cfg: cfg, store: s, resolver: r}
}

func declaration(t testing.TB, name string) *modelv1alpha1.AdapterDeclaration {
	t.Helper()
	b, err := os.ReadFile("../../testdata/declarations/" + name + ".json") //nolint:gosec // G304: reference declarations under testdata
	if err != nil {
		t.Fatal(err)
	}
	d := &modelv1alpha1.AdapterDeclaration{}
	if err := model.DecodeJSON(b, d); err != nil {
		t.Fatal(err)
	}
	return d
}

// apply feeds observations to the resolver as events of source, numbered
// within their batch (a sync, a webhook delivery or the directory feed).
func (w *world) apply(source, batch string, obs ...*eventv1alpha1.Observation) {
	w.t.Helper()
	for i, o := range obs {
		ev := resolver.Event{ID: fmt.Sprintf("%s/%s/%d", source, batch, i+1), Source: source, Observation: o}
		if _, err := w.resolver.Apply(context.Background(), ev); err != nil {
			w.t.Fatalf("apply %s: %v", ev.ID, err)
		}
		w.clock.Advance(time.Millisecond)
	}
}

// syncGitHub runs a full GitHub sync and applies it.
func (w *world) syncGitHub() {
	w.t.Helper()
	var all adapter.Observations
	cur := ""
	for range 50 {
		res, err := w.adapter.Sync(context.Background(), adapter.SyncParams{Config: w.cfg, Cursor: cur})
		if err != nil {
			w.t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if res.Done {
			break
		}
		cur = res.NextCursor
	}
	w.syncs++
	w.apply(githubSource, fmt.Sprintf("sync-%d", w.syncs), all...)
}

// deliver applies the webhook deliveries made since the last call.
func (w *world) deliver() {
	w.t.Helper()
	ds := w.gh.Deliveries()
	for _, d := range ds[w.delivered:] {
		res, err := w.adapter.Handle(context.Background(), adapter.HandleParams{Config: w.cfg, Headers: d.Header(), Body: d.Body})
		if err != nil {
			w.t.Fatalf("%s delivery: %v", d.Event, err)
		}
		w.apply(githubSource, "delivery-"+d.ID[len(d.ID)-4:], res.Observations...)
	}
	w.delivered = len(ds)
}

// syncDirectory applies the recorded directory feed.
func (w *world) syncDirectory() {
	w.t.Helper()
	f, err := os.Open("../../testdata/acme/directory.ndjson")
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var obs []*eventv1alpha1.Observation
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		o, err := model.DecodeObservation(sc.Bytes())
		if err != nil {
			w.t.Fatal(err)
		}
		obs = append(obs, o)
	}
	if err := sc.Err(); err != nil {
		w.t.Fatal(err)
	}
	w.apply(directorySource, "feed-1", obs...)
}

// play does the first GitHub sync and then advance(until).
func (w *world) play(until time.Time) {
	w.t.Helper()
	w.syncGitHub()
	w.advance(until)
}

// advance runs what the story has next at or before until, in time order:
// the directory feed at fakes.DirectorySyncAt and each change with the
// webhooks it sends.
func (w *world) advance(until time.Time) {
	w.t.Helper()
	steps := fakes.Story()
	for ; w.next < len(steps); w.next++ {
		step := steps[w.next]
		if !w.directory && step.At.After(fakes.DirectorySyncAt) && !fakes.DirectorySyncAt.After(until) {
			w.directory = true
			w.at(fakes.DirectorySyncAt)
			w.syncDirectory()
		}
		if step.At.After(until) {
			return
		}
		w.at(step.At)
		if step.Apply != nil {
			if err := step.Apply(w.org); err != nil {
				w.t.Fatal(err)
			}
			w.deliver()
		}
	}
}

func (w *world) at(t time.Time) {
	if t.After(w.clock.Now()) {
		w.clock.Set(t)
	}
}

// cli runs a query command against the world's store and returns its output.
func (w *world) cli(args ...string) (string, error) {
	w.t.Helper()
	env := queryEnv{
		Open: func(_ context.Context, url string) (contracts.GraphStore, func(context.Context) error, error) {
			if url != "mem://" {
				return nil, nil, fmt.Errorf("test store: unexpected URL %q", url)
			}
			return w.store, func(context.Context) error { return nil }, nil
		},
		Getenv: func(string) string { return "" },
		Now:    w.clock.Now,
	}
	var out bytes.Buffer
	err := runWith(context.Background(), env, append([]string{args[0], "--store", "mem://"}, args[1:]...), &out)
	return out.String(), err
}
