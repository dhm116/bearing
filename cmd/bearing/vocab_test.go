package main

import (
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"bearing.example/adapters/kubernetes"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
	"bearing.example/pkg/server"
	"bearing.example/pkg/store"
	"bearing.example/pkg/vocab"
)

const k8sSource = "kubernetes-acme"

// vocabServer is the Acme world's server with the Kubernetes module loaded and
// its suggestions stored as proposed, as a boot leaves them. It applies
// decisions when run is true, or once start is called; otherwise they stay on
// the log.
func (w *world) vocabServer(run bool) (addr string, start func()) {
	w.t.Helper()
	ctx := w.t.Context()
	mod := kubernetes.Module()
	for _, m := range []vocab.Module{vocab.Core(), mod} {
		source := ""
		if m.Owner == vocab.OwnerAdapter {
			source = k8sSource
		}
		if err := w.store.SyncModule(ctx, m, source, w.clock.Now()); err != nil {
			w.t.Fatal(err)
		}
	}
	for _, s := range mod.Suggests {
		s.Status, s.Author, s.ValidFrom = vocab.Proposed, k8sSource, w.clock.Now()
		if err := w.store.PutMapping(ctx, s); err != nil {
			w.t.Fatal(err)
		}
	}
	s, err := server.New(server.Config{
		Log: w.store, Applier: nopApplier{}, Links: w.store, Clock: w.clock, PollInterval: 5 * time.Millisecond,
		Vocab: &server.Vocabulary{Store: w.store, Modules: []vocab.Module{mod}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	api, err := server.NewAPI(server.APIConfig{
		Server: s, Query: &query.Querier{Graph: w.store, Now: w.clock.Now},
		CallerRate: server.RateLimit{PerMinute: -1}, DecideRate: server.RateLimit{PerMinute: -1},
		LinkDecisions: server.LinkDecisionsToken,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	start = sync.OnceFunc(func() { go func() { _ = s.Run(ctx) }() })
	if run {
		start()
	}
	srv := httptest.NewServer(api.Handler(testCallers))
	w.t.Cleanup(srv.Close)
	return srv.URL, start
}

// vocab runs `bearing vocab <args>` against addr as the token's caller.
func (w *world) vocab(addr string, args ...string) (string, error) {
	w.t.Helper()
	return w.cliWith(map[string]string{tokenEnv: apiToken}, append(append([]string{"vocab"}, args...), "--server", addr)...)
}

// mapping is the latest row of mapping id.
func (w *world) mapping(id string) vocab.Mapping {
	w.t.Helper()
	ms, err := w.store.Mappings(w.t.Context())
	if err != nil {
		w.t.Fatal(err)
	}
	var last vocab.Mapping
	for _, m := range ms {
		if m.ID == id {
			last = m
		}
	}
	return last
}

// waitStatus waits for the server to apply a decision on id.
func (w *world) waitStatus(id, status string) vocab.Mapping {
	w.t.Helper()
	for range 400 {
		if m := w.mapping(id); m.Status == status && m.ValidTo == nil {
			return m
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.t.Fatalf("mapping %s never became %s: %+v", id, status, w.mapping(id))
	return vocab.Mapping{}
}

// vocabEvents is the number of mapping decisions on the world's log.
func (w *world) vocabEvents() int {
	w.t.Helper()
	es, err := w.store.Read(w.t.Context(), "core/vocabulary", 0, store.MaxReadEntries)
	if err != nil {
		w.t.Fatal(err)
	}
	return len(es)
}

func TestVocabReviewListsTheProposedMappings(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	addr, _ := w.vocabServer(false)
	out, err := w.vocab(addr, "review")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"7 proposed mappings", "k8s-depends-on-is-depends-on", "io.k8s:depends_on exact_match com.dougmccall.bearing.core:depends_on", "proposed by kubernetes-acme"} {
		if !strings.Contains(out, want) {
			t.Errorf("got %q, want it to contain %q", out, want)
		}
	}
	if js, err := w.vocab(addr, "review", "--json"); err != nil || !strings.Contains(js, `"mappings"`) || !strings.Contains(js, `"k8s-deployment-is-component"`) {
		t.Errorf("got %q, %v, want the server's answer as JSON", js, err)
	}
}

func TestVocabAcceptRefusesWhatWouldStopBootingAndNamesWhatComesFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	addr, _ := w.vocabServer(true)
	const dep = "k8s-depends-on-is-depends-on"
	_, err := w.vocab(addr, "accept", dep, "--why", "env and DNS references")
	if err == nil {
		t.Fatal("accepted depends_on before the workload kinds are Components")
	}
	for _, want := range []string{"cannot accept " + dep, "k8s-daemonset-is-component", "k8s-deployment-is-component", "k8s-statefulset-is-component", "first", "mapping_refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("got %q, want it to contain %q", err, want)
		}
	}
	if n := w.vocabEvents(); n != 0 || w.mapping(dep).Status != vocab.Proposed {
		t.Fatalf("%d decisions logged and the mapping is %s, want nothing written", n, w.mapping(dep).Status)
	}
	// Accept what it needs, in order, and then it goes through.
	for _, id := range []string{"k8s-daemonset-is-component", "k8s-deployment-is-component", "k8s-statefulset-is-component"} {
		if out, err := w.vocab(addr, "accept", id, "--why", "each runs one service"); err != nil || !strings.Contains(out, "logged") {
			t.Fatalf("accept %s: %q, %v", id, out, err)
		}
		w.waitStatus(id, vocab.Asserted)
	}
	if _, err := w.vocab(addr, "accept", dep, "--why", "env and DNS references"); err != nil {
		t.Fatalf("accept %s after its broader mappings: %v", dep, err)
	}
	got := w.waitStatus(dep, vocab.Asserted)
	if got.Author != "token" || got.Justification != "env and DNS references" {
		t.Errorf("got author %q and why %q, want the caller and the why", got.Author, got.Justification)
	}
	// The next boot builds from what is stored now.
	ms, err := w.store.Mappings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vocab.Registry(model.Core(), []vocab.Module{kubernetes.Module()}, ms, w.clock.Now()); err != nil {
		t.Errorf("the registry the next boot builds fails: %v", err)
	}
	if out, _ := w.vocab(addr, "review"); strings.Contains(out, dep) {
		t.Errorf("review still lists %s after it was accepted:\n%s", dep, out)
	}
}

func TestVocabRejectAndAReplayedAcceptApplyOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	addr, start := w.vocabServer(false) // nothing is applied until the server runs
	const env = "k8s-namespace-is-environment"
	if _, err := w.vocab(addr, "reject", env, "--why", "many teams use the cluster"); err != nil {
		t.Fatal(err)
	}
	// Both accepts pass the check, since the first is not applied yet.
	for range 2 {
		if _, err := w.vocab(addr, "accept", "k8s-deployment-is-component", "--why", "one service"); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.vocabEvents(); n != 3 {
		t.Fatalf("%d decisions on the log, want 3", n)
	}
	start()
	w.waitStatus(env, vocab.Rejected)
	m := w.waitStatus("k8s-deployment-is-component", vocab.Asserted)
	time.Sleep(50 * time.Millisecond) // let the replayed accept be handled
	rows := 0
	ms, _ := w.store.Mappings(t.Context())
	for _, r := range ms {
		if r.ID == m.ID && r.Status == vocab.Asserted {
			rows++
		}
	}
	if rows != 1 || m.Author != "token" {
		t.Errorf("%d asserted rows by %q, want one by the caller", rows, m.Author)
	}
	// Decided mappings are no longer proposals.
	_, err := w.vocab(addr, "accept", env, "--why", "changed my mind")
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Errorf("got %v, want a not-found for a mapping already decided", err)
	}
}

func TestVocabDecisionsAreRefusedBeforeTheyAreSent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	addr, _ := w.vocabServer(false)
	for name, tt := range map[string]struct {
		vars map[string]string
		args []string
		want string
	}{
		"no subcommand":     {nil, []string{"vocab"}, ""},
		"an unknown one":    {nil, []string{"vocab", "promote"}, ""},
		"accept with none":  {nil, []string{"vocab", "accept", "--why", "x", "--server", addr}, "a mapping ID"},
		"review with an ID": {nil, []string{"vocab", "review", "x", "--server", addr}, "no arguments"},
		"no why":            {nil, []string{"vocab", "accept", "x", "--server", addr}, "--why"},
		"a store":           {nil, []string{"vocab", "accept", "x", "--why", "y", "--store", "mem://"}, "running server"},
		"no server":         {nil, []string{"vocab", "reject", "x", "--why", "y"}, "running server"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := w.cliWith(tt.vars, tt.args...)
			if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want a usage error containing %q", err, tt.want)
			}
		})
	}
	if _, err := w.vocab(addr, "reject", "nope", "--why", "y"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Errorf("got %v, want a not-found for an unknown mapping", err)
	}
	if n := w.vocabEvents(); n != 0 {
		t.Errorf("%d decisions were logged, want none", n)
	}
	if got := slices.Contains(strings.Fields(usage), "vocab"); !got {
		t.Error("the usage text does not mention vocab")
	}
}
