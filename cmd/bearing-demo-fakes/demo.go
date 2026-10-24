package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"bearing.example/internal/fakes"
	"bearing.example/pkg/clock"
)

// Config says where the fakes listen and what they accept.
type Config struct {
	// GitHub, Directory and Kubernetes are the listeners of the three fakes;
	// nil picks a loopback port, as a test does.
	GitHub, Directory, Kubernetes net.Listener
	// TLS, if set, makes the three fakes serve HTTPS.
	TLS *tls.Config
	// The tokens the fakes accept and the secret that signs webhook
	// deliveries: the values the Bearing sources use.
	GitHubToken, DirectoryToken, KubernetesToken, WebhookSecret string
	// WebhookURL, if set, receives a signed delivery for each GitHub change
	// the story makes. WebhookClient sends it (nil means the default client).
	WebhookURL    string
	WebhookClient *http.Client
}

// demoClock is the one clock the fakes read: it stands still at the story's
// start and moves only when a step is played.
type demoClock struct {
	clock.Real
	mu sync.Mutex
	t  time.Time
}

func (c *demoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *demoClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.t) {
		c.t = t
	}
}

// step is one change of either story, by the name the control endpoint takes.
type step struct {
	id    string
	name  string
	at    time.Time
	apply func() error
}

// githubStepIDs and clusterStepIDs name fakes.Story() and
// fakes.KubernetesStory() in order. The cluster's last step edits the
// adapter's settings, not the cluster, so the demo leaves it out.
var (
	githubStepIDs  = []string{"repo-rename", "codeowners-move", "team-rename", "team-delete", "membership-end"}
	clusterStepIDs = []string{"replicas", "image", "team-label", "annotation", "dependency", "workload-delete"}
)

// demo is the three fakes of the Acme org, sharing one clock.
type demo struct {
	cfg   Config
	clk   *demoClock
	gh    *fakes.GitHub
	dir   *fakes.Directory
	kube  *fakes.Kubernetes
	steps []step

	mu   sync.Mutex // guards the fields below, and serializes advance
	done int        // steps played
	sent int        // webhook deliveries sent
}

// newDemo starts the fakes. They close when tb's cleanups run.
func newDemo(tb fakes.TB, cfg Config) *demo {
	tb.Helper()
	d := &demo{cfg: cfg, clk: &demoClock{t: fakes.Start}}
	org := fakes.NewOrg(d.clk)
	cluster := fakes.NewCluster(d.clk)
	listen := func(l net.Listener) fakes.Listen { return fakes.Listen{Listener: l, TLS: cfg.TLS} }
	d.gh = fakes.NewGitHub(tb, org, fakes.GitHubOptions{Token: cfg.GitHubToken, WebhookSecret: cfg.WebhookSecret, Listen: listen(cfg.GitHub)})
	d.dir = fakes.NewDirectory(tb, org, fakes.DirectoryOptions{Token: cfg.DirectoryToken, Listen: listen(cfg.Directory)})
	d.kube = fakes.NewKubernetes(tb, cluster, fakes.KubernetesOptions{Token: cfg.KubernetesToken, Listen: listen(cfg.Kubernetes)})

	for i, s := range fakes.Story() {
		d.steps = append(d.steps, step{id: githubStepIDs[i], name: s.Name, at: s.At, apply: apply(s.Apply, org)})
	}
	ids := clusterStepIDs
	for _, s := range fakes.KubernetesStory() {
		if s.Apply == nil {
			continue
		}
		d.steps = append(d.steps, step{id: ids[0], name: s.Name, at: s.At, apply: apply(s.Apply, cluster)})
		ids = ids[1:]
	}
	slices.SortStableFunc(d.steps, func(a, b step) int { return a.at.Compare(b.at) })
	return d
}

func apply[T any](f func(*T) error, v *T) func() error {
	if f == nil {
		return func() error { return nil }
	}
	return func() error { return f(v) }
}

// stepStatus is a step as GET /demo/steps lists it.
type stepStatus struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	At      time.Time `json:"at"`
	Applied bool      `json:"applied"`
}

type status struct {
	Now   time.Time    `json:"now"`
	Steps []stepStatus `json:"steps"`
}

// advanceResult is what POST /demo/advance answers.
type advanceResult struct {
	Now      time.Time `json:"now"`
	Played   []string  `json:"played"`
	Webhooks int       `json:"webhooks_sent"`
}

var (
	errUnknownTarget = errors.New("unknown target: use a step id (see GET /demo/steps) or an RFC 3339 time")
	errBackwards     = errors.New("the fakes' clock does not run backwards: restart them to start over")
)

// status lists the steps and the fakes' time.
func (d *demo) status() status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := status{Now: d.clk.Now()}
	for i, s := range d.steps {
		st.Steps = append(st.Steps, stepStatus{ID: s.id, Name: s.name, At: s.at, Applied: i < d.done})
	}
	return st
}

// advance plays every step not yet played up to to, a step id (inclusive) or
// a time, then sends the webhook deliveries those steps made.
func (d *demo) advance(ctx context.Context, to string) (advanceResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var end int
	var until time.Time
	if i := slices.IndexFunc(d.steps, func(s step) bool { return s.id == to }); i >= 0 {
		end = i + 1
	} else if t, err := time.Parse(time.RFC3339, to); err == nil {
		if t.Before(d.clk.Now()) {
			return advanceResult{}, errBackwards
		}
		until = t
		end, _ = slices.BinarySearchFunc(d.steps, t, func(s step, t time.Time) int {
			if s.at.After(t) {
				return 1
			}
			return -1 // a step at t itself is played
		})
	} else {
		return advanceResult{}, errUnknownTarget
	}
	res := advanceResult{Played: []string{}}
	for ; d.done < end; d.done++ {
		s := d.steps[d.done]
		d.clk.set(s.at)
		if err := s.apply(); err != nil {
			return res, fmt.Errorf("%s: %w", s.id, err)
		}
		res.Played = append(res.Played, s.id)
	}
	if !until.IsZero() {
		d.clk.set(until)
	}
	res.Now = d.clk.Now()
	n, err := d.deliver(ctx)
	res.Webhooks = n
	return res, err
}

// deliver sends the signed deliveries of the changes made since the last
// call to the webhook URL, if there is one. The caller holds d.mu.
func (d *demo) deliver(ctx context.Context) (int, error) {
	if d.cfg.WebhookURL == "" {
		return 0, nil
	}
	client := d.cfg.WebhookClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	all := d.gh.Deliveries()
	n := 0
	for _, dl := range all[min(d.sent, len(all)):] {
		req, err := dl.Request(ctx, d.cfg.WebhookURL)
		if err != nil {
			return n, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return n, fmt.Errorf("webhook delivery %s: %w", dl.ID, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return n, fmt.Errorf("webhook delivery %s: the server answered %s", dl.ID, resp.Status)
		}
		d.sent++
		n++
	}
	return n, nil
}

// control serves the endpoints that play the story:
//
//	GET  /demo/steps               the steps and which are played
//	POST /demo/advance?to=<step|time>
func (d *demo) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /demo/steps", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, d.status()) })
	mux.HandleFunc("POST /demo/advance", func(w http.ResponseWriter, r *http.Request) {
		res, err := d.advance(r.Context(), r.URL.Query().Get("to"))
		switch {
		case errors.Is(err, errUnknownTarget):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		case errors.Is(err, errBackwards):
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "result": res})
		default:
			writeJSON(w, http.StatusOK, res)
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // the client went away; nothing to do
}
