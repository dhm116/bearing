package github

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

const (
	testToken  = "test-token"
	testSecret = "s3cret"
)

// rig is the adapter pointed at the fictional org's GitHub fake.
type rig struct {
	t         *testing.T
	clock     *testkit.FakeClock
	org       *fakes.Org
	srv       *fakes.GitHub
	a         *Adapter
	cfg       json.RawMessage
	delivered int // deliveries already handled
}

func newRig(t *testing.T, mod ...func(*Config)) *rig {
	t.Helper()
	c := testkit.NewClock(fakes.Start)
	org := fakes.NewOrg(c)
	srv := fakes.NewGitHub(t, org, fakes.GitHubOptions{Token: testToken, WebhookSecret: testSecret})
	cfg := Config{Org: "acme", APIURL: srv.URL, PerPage: 2}
	for _, m := range mod {
		m(&cfg)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GITHUB_TOKEN": testToken, "GITHUB_WEBHOOK_SECRET": testSecret}
	a := &Adapter{HTTP: srv.Client(), Now: c.Now, Getenv: func(k string) string { return env[k] }}
	return &rig{t: t, clock: c, org: org, srv: srv, a: a, cfg: raw}
}

// sync runs a full sync and returns every observation and the last page.
func (r *rig) sync() (adapter.Observations, adapter.SyncResult) {
	r.t.Helper()
	obs, last, err := r.trySync()
	if err != nil {
		r.t.Fatal(err)
	}
	return obs, last
}

func (r *rig) trySync() (adapter.Observations, adapter.SyncResult, error) {
	var all adapter.Observations
	cur := ""
	for range 50 {
		res, err := r.a.Sync(context.Background(), adapter.SyncParams{Config: r.cfg, Cursor: cur})
		if err != nil {
			return all, res, err
		}
		all = append(all, res.Observations...)
		if res.Done {
			return all, res, nil
		}
		if res.NextCursor == "" || res.NextCursor == cur {
			r.t.Fatalf("not done but cursor %q after %q", res.NextCursor, cur)
		}
		cur = res.NextCursor
	}
	r.t.Fatal("sync never finished")
	return nil, adapter.SyncResult{}, nil
}

// handleNew handles the deliveries made since the last call and returns
// the observations of each.
func (r *rig) handleNew() []adapter.Observations {
	r.t.Helper()
	ds := r.srv.Deliveries()
	var out []adapter.Observations
	for _, d := range ds[r.delivered:] {
		res, err := r.a.Handle(context.Background(), adapter.HandleParams{Config: r.cfg, Headers: d.Header(), Body: d.Body})
		if err != nil {
			r.t.Fatalf("%s delivery: %v", d.Event, err)
		}
		out = append(out, res.Observations)
	}
	r.delivered = len(ds)
	return out
}

func byKey(obs adapter.Observations) map[string]*eventv1alpha1.Observation {
	m := map[string]*eventv1alpha1.Observation{}
	for _, o := range obs {
		m[o.GetData().GetEntity().GetKey()] = o
	}
	return m
}

// get returns the observation whose entity has alias, or fails.
func get(t *testing.T, obs adapter.Observations, alias string) *modelv1alpha1.ObservationData {
	t.Helper()
	for _, o := range obs {
		if slices.Contains(o.GetData().GetEntity().GetAliases(), alias) {
			return o.GetData()
		}
	}
	t.Fatalf("no observation with alias %s", alias)
	return nil
}

// links renders relations as "type to|from key file:line pattern".
func links(d *modelv1alpha1.ObservationData) []string {
	var out []string
	for _, r := range d.GetRelations() {
		end := "to " + r.GetTo()
		if r.GetFrom() != "" {
			end = "from " + r.GetFrom()
		}
		s := r.GetType() + " " + end
		if a := r.GetAttributes(); a != nil {
			s += fmt.Sprintf(" %s:%.0f %s", a["file"].GetStringValue(), a["line"].GetNumberValue(), a["pattern"].GetStringValue())
		}
		out = append(out, s)
	}
	return out
}

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// checkGolden compares obs with a golden file of ProtoJSON observations,
// one per line as the CLI prints them, and checks the file round-trips:
// every observation validates as an adapter's and decoding gives back
// exactly obs.
func checkGolden(t *testing.T, name string, obs adapter.Observations) {
	t.Helper()
	var buf bytes.Buffer
	for _, o := range obs {
		b, err := model.EncodeJSON(o)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata
	if err != nil {
		t.Fatalf("%v (run go test ./adapters/github -update)", err)
	}
	if !bytes.Equal(golden, buf.Bytes()) {
		t.Fatalf("output differs from %s (run go test ./adapters/github -update and review the diff):\n%s", path, buf.String())
	}
	lines := bytes.Split(bytes.TrimSuffix(golden, []byte("\n")), []byte("\n"))
	if len(lines) != len(obs) {
		t.Fatalf("got %d observations from %s, want %d", len(lines), path, len(obs))
	}
	for i, l := range lines {
		o := &eventv1alpha1.Observation{}
		if err := model.DecodeJSON(l, o); err != nil {
			t.Fatalf("%s:%d: %v", path, i+1, err)
		}
		if err := model.ValidateAdapterObservation(o); err != nil {
			t.Errorf("%s:%d: %v", path, i+1, err)
		}
		if !proto.Equal(o, obs[i]) {
			t.Errorf("%s:%d decodes to %v, want %v", path, i+1, o, obs[i])
		}
	}
}

// checkDeclared checks obs against the reference declaration
// testdata/declarations/github.json: declared kinds, key types, attributes,
// relations (with their direction) and snapshot scopes, and that node-ID
// keys are in the next format.
func checkDeclared(t *testing.T, obs adapter.Observations) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "declarations", "github.json"))
	if err != nil {
		t.Fatal(err)
	}
	decl := &modelv1alpha1.AdapterDeclaration{}
	if err := model.DecodeJSON(raw, decl); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]*modelv1alpha1.KindDeclaration{}
	keyTypes := map[string]bool{}
	for _, k := range decl.GetKinds() {
		kinds[k.GetKind()] = k
		for _, kt := range k.GetKeys() {
			keyTypes[kt.GetKeyType()] = true
		}
	}
	legacy := regexp.MustCompile(`^[A-Z]{1,4}_[A-Za-z0-9_-]+$`)
	field := func(k *modelv1alpha1.KindDeclaration, predicate string, dir modelv1alpha1.Direction) bool {
		return slices.ContainsFunc(k.GetFields(), func(f *modelv1alpha1.FieldDeclaration) bool {
			return f.GetPredicate() == predicate && (f.GetDirection() == dir || (dir == modelv1alpha1.Direction_DIRECTION_OUT && f.GetDirection() == modelv1alpha1.Direction_DIRECTION_UNSPECIFIED))
		})
	}
	checkKey := func(who, key string) {
		ns, kt, id, err := model.Key(key).Parse()
		if err != nil || ns != "github" || !keyTypes[kt] {
			t.Errorf("%s: key %q is not a declared github key (%v)", who, key, err)
		}
		if strings.HasSuffix(kt, "_node") && !legacy.MatchString(id) {
			t.Errorf("%s: key %q is not a next-format node ID", who, key)
		}
	}
	for _, o := range obs {
		d := o.GetData()
		e := d.GetEntity()
		k := kinds[e.GetKind()]
		if k == nil {
			t.Errorf("%s: kind %s is not declared", e.GetKey(), e.GetKind())
			continue
		}
		checkKey(e.GetKey(), e.GetKey())
		for _, a := range e.GetAliases() {
			checkKey(e.GetKey(), a)
		}
		for name := range e.GetAttributes() {
			if !field(k, name, modelv1alpha1.Direction_DIRECTION_OUT) {
				t.Errorf("%s: attribute %s is not declared", e.GetKey(), name)
			}
		}
		for _, r := range d.GetRelations() {
			dir, other := modelv1alpha1.Direction_DIRECTION_OUT, r.GetTo()
			if r.GetFrom() != "" {
				dir, other = modelv1alpha1.Direction_DIRECTION_IN, r.GetFrom()
			}
			if !field(k, r.GetType(), dir) {
				t.Errorf("%s: relation %s (%s) is not declared", e.GetKey(), r.GetType(), dir)
			}
			checkKey(e.GetKey(), other)
		}
		for _, s := range d.GetSnapshots() {
			for _, p := range s.GetPredicates() {
				if !field(k, p, s.GetDirection()) {
					t.Errorf("%s: snapshot of %s (%s) is not declared", e.GetKey(), p, s.GetDirection())
				}
			}
		}
	}
}

func TestSyncFirstFollowsTheFictionalOrg(t *testing.T) {
	r := newRig(t)
	obs, last := r.sync()
	checkGolden(t, "sync-first.golden.jsonl", obs)
	checkDeclared(t, obs)

	if got, want := last.CompleteSync.GetKinds(), []string{"Repository", "Team"}; !slices.Equal(got, want) {
		t.Errorf("complete_sync kinds = %v, want %v", got, want)
	}
	if b, _ := json.Marshal(last); !strings.Contains(string(b), `"complete_sync":{"kinds":["Repository","Team"]}`) {
		t.Errorf("last page JSON lacks complete_sync: %s", b)
	}
	for i, req := range r.srv.Requests() {
		if req.Method != http.MethodPost || req.Path != "/graphql" || req.Header.Get("X-Github-Next-Global-Id") != "1" ||
			req.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("request %d: %s %s, next-ID header %q: want an authenticated GraphQL POST asking for next-format IDs",
				i, req.Method, req.Path, req.Header.Get("X-Github-Next-Global-Id"))
		}
	}

	repos := map[string][]string{
		// Line 2: line 1 is a comment. A name key is an alias, the node ID the key.
		"github:repo/acme/payments-api": {"approves_changes to github:team/acme/payments .github/CODEOWNERS:2 *"},
		"github:repo/acme/web": {
			"approves_changes to github:team/acme/platform CODEOWNERS:1 *",
			"approves_changes to github:user/jdoe CODEOWNERS:2 /docs/",
		},
		// The stale root file is ignored: .github/ wins.
		"github:repo/acme/ops-scripts": {"approves_changes to github:team/acme/legacy-ops .github/CODEOWNERS:1 *"},
		"github:repo/acme/handbook": {
			"approves_changes to github:team/acme/platform docs/CODEOWNERS:1 /guides/",
			"approves_changes to github:team/acme/sre docs/CODEOWNERS:2 /runbooks/",
		},
		"github:repo/acme/sandbox": nil,
	}
	for alias, want := range repos {
		d := get(t, obs, alias)
		if got := links(d); !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", alias, got, want)
		}
		if s := d.GetSnapshots(); len(s) != 1 || s[0].GetDirection() != modelv1alpha1.Direction_DIRECTION_OUT || !slices.Equal(s[0].GetPredicates(), []string{"approves_changes"}) {
			t.Errorf("%s: snapshots = %v, want the approves_changes scope even with no CODEOWNERS", alias, s)
		}
	}
	sandbox := get(t, obs, "github:repo/acme/sandbox").GetEntity().GetAttributes()
	if !isNull(sandbox["codeowners_rules"]) || !isNull(sandbox["description"]) || !isNull(sandbox["language"]) ||
		sandbox["default_branch"].GetStringValue() != "trunk" || !isEmptyList(sandbox["topics"]) {
		t.Errorf("sandbox attributes = %v: want codeowners_rules, description and language null, topics [] and branch trunk", sandbox)
	}
	if n := get(t, obs, "github:repo/acme/web").GetEntity().GetAttributes()["codeowners_rules"].GetNumberValue(); n != 2 {
		t.Errorf("web codeowners_rules = %v, want 2", n)
	}
	nodeOf := func(alias string) string { return get(t, obs, alias).GetEntity().GetKey() }
	engineering := links(get(t, obs, "github:team/acme/engineering"))
	for _, child := range []string{"payments", "platform", "sre"} {
		if want := "member_of from " + nodeOf("github:team/acme/"+child); !slices.Contains(engineering, want) {
			t.Errorf("engineering: %q missing from %q", want, engineering)
		}
	}
	if len(engineering) != 3 {
		t.Errorf("engineering lists %q: want its three child teams and no people (jdoe is in payments only)", engineering)
	}
	payments := links(get(t, obs, "github:team/acme/payments"))
	want := []string{"member_of from " + nodeOf("github:user/jdoe"), "member_of from " + nodeOf("github:user/rpatel")}
	if !slices.Equal(payments, want) {
		t.Errorf("payments members = %q, want %q", payments, want)
	}
	if m := get(t, obs, "github:user/meichen").GetEntity().GetAttributes(); m["login"].GetStringValue() != "meichen" || m["name"].GetStringValue() != "Mei Chen" ||
		!slices.Equal(listOf(m["verified_email"]), []string{"mchen@acme.example"}) {
		t.Errorf("meichen = %v", m)
	}
	if m := get(t, obs, "github:user/sokafor-ext").GetEntity().GetAttributes(); !isEmptyList(m["verified_email"]) {
		t.Errorf("sokafor-ext verified_email = %v, want []", m["verified_email"])
	}
	for _, o := range obs {
		if strings.Contains(o.GetData().GetEntity().GetKey(), "lfischer") || slices.Contains(o.GetData().GetEntity().GetAliases(), "github:user/lfischer") {
			t.Errorf("lfischer has no GitHub account but %v was observed", o.GetData().GetEntity())
		}
	}
}

func isNull(v *structpb.Value) bool {
	_, ok := v.GetKind().(*structpb.Value_NullValue)
	return ok
}

func isEmptyList(v *structpb.Value) bool {
	return v.GetListValue() != nil && len(v.GetListValue().GetValues()) == 0
}

func listOf(v *structpb.Value) []string {
	var out []string
	for _, e := range v.GetListValue().GetValues() {
		out = append(out, e.GetStringValue())
	}
	return out
}
