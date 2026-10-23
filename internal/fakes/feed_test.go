package fakes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"bearing.example/adapters/authentik"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

var update = flag.Bool("update", false, "rewrite the recorded directory feed")

// feedPath is the recorded directory feed, shared by tests and demos.
const feedPath = "../../testdata/acme/directory.ndjson"

// directorySource is the CloudEvents source of the directory feed.
const directorySource = "adapter/authentik"

// recordFeed runs the Authentik adapter against the directory fake, as the
// server would, and returns its observations. The adapter's UI links point
// at the admin UI the fake's evidence URLs name.
func recordFeed(t *testing.T, d *Directory, at time.Time) []*eventv1alpha1.Observation {
	t.Helper()
	cfg, err := json.Marshal(authentik.Config{APIURL: d.URL, UIURL: directoryUI, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	a := &authentik.Adapter{HTTP: d.Client(), Now: func() time.Time { return at }, Getenv: func(string) string { return testToken }}
	var obs []*eventv1alpha1.Observation
	cur := ""
	for range 100 {
		res, err := a.Sync(t.Context(), adapter.SyncParams{Config: cfg, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		obs = append(obs, res.Observations...)
		if res.Done {
			return obs
		}
		cur = res.NextCursor
	}
	t.Fatal("sync did not finish")
	return nil
}

// readFeed decodes and validates every line of the recorded feed.
func readFeed(t *testing.T) []*eventv1alpha1.Observation {
	t.Helper()
	b, err := os.ReadFile(feedPath)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/fakes -update)", err)
	}
	var obs []*eventv1alpha1.Observation
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		o, err := model.DecodeObservation(sc.Bytes())
		if err != nil {
			t.Fatalf("%s:%d: %v", feedPath, n, err)
		}
		obs = append(obs, o)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return obs
}

func TestDirectoryFeedMatchesTheFake(t *testing.T) {
	d, _, _ := newDirectory(t)
	obs := recordFeed(t, d, DirectorySyncAt)
	var buf bytes.Buffer
	for _, o := range obs {
		b, err := model.EncodeJSON(o)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if *update {
		if err := os.WriteFile(feedPath, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(feedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Fatalf("%s differs from the adapter's output (run go test ./internal/fakes -update and review the diff)", feedPath)
	}
}

func TestDirectoryFeedFollowsTheAuthentikDeclaration(t *testing.T) {
	b, err := os.ReadFile("../../testdata/declarations/authentik.json")
	if err != nil {
		t.Fatal(err)
	}
	decl := &modelv1alpha1.AdapterDeclaration{}
	if err := model.DecodeJSON(b, decl); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]*modelv1alpha1.KindDeclaration{}
	keyKind := map[string]string{} // "authentik:user" -> "Person"
	for _, k := range decl.GetKinds() {
		kinds[k.GetKind()] = k
		for _, kt := range k.GetKeys() {
			keyKind[cmpOrStr(kt.GetIssuerType(), decl.GetIssuerType())+":"+kt.GetKeyType()] = k.GetKind()
		}
	}
	keyType := func(key string) string {
		ns, kt, _, err := model.Key(key).Parse()
		if err != nil {
			t.Fatal(err)
		}
		return ns + ":" + kt
	}
	for _, o := range readFeed(t) {
		e := o.GetData().GetEntity()
		k := kinds[e.GetKind()]
		if k == nil || o.GetSource() != directorySource || !o.GetTime().AsTime().Equal(DirectorySyncAt) {
			t.Fatalf("%s: kind %s, source %s, time %v", o.GetId(), e.GetKind(), o.GetSource(), o.GetTime().AsTime())
		}
		for _, key := range append([]string{e.GetKey()}, e.GetAliases()...) {
			if keyKind[keyType(key)] != e.GetKind() {
				t.Errorf("%s: key %s isn't a declared %s key", o.GetId(), key, e.GetKind())
			}
		}
		for _, l := range e.GetLinkedIds() {
			if !slices.ContainsFunc(k.GetLinks(), func(d *modelv1alpha1.LinkDeclaration) bool {
				return d.GetIssuerType()+":"+d.GetKeyType() == keyType(l)
			}) {
				t.Errorf("%s: link %s isn't declared", o.GetId(), l)
			}
		}
		for name := range e.GetAttributes() {
			if !declared(k, name, modelv1alpha1.Direction_DIRECTION_OUT) {
				t.Errorf("%s: attribute %s isn't declared", o.GetId(), name)
			}
		}
		for _, r := range o.GetData().GetRelations() {
			if r.GetFrom() == "" || !declared(k, r.GetType(), modelv1alpha1.Direction_DIRECTION_IN) {
				t.Errorf("%s: relation %v isn't a declared incoming field", o.GetId(), r)
			}
			if keyKind[keyType(r.GetFrom())] == "" {
				t.Errorf("%s: %s isn't a declared key type", o.GetId(), r.GetFrom())
			}
		}
	}
}

func declared(k *modelv1alpha1.KindDeclaration, predicate string, dir modelv1alpha1.Direction) bool {
	return slices.ContainsFunc(k.GetFields(), func(f *modelv1alpha1.FieldDeclaration) bool {
		d := f.GetDirection()
		if d == modelv1alpha1.Direction_DIRECTION_UNSPECIFIED {
			d = modelv1alpha1.Direction_DIRECTION_OUT
		}
		return f.GetPredicate() == predicate && d == dir
	})
}

func cmpOrStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// TestDirectoryFeedLinksMatchGitHub checks the feed lines up with the
// GitHub side: every linked node ID and login is the account the GitHub
// fake serves for the same person, and lfischer's engineering membership
// carries its known end.
func TestDirectoryFeedLinksMatchGitHub(t *testing.T) {
	o := NewOrg(testkit.NewClock(DirectorySyncAt))
	g := NewGitHub(t, o, GitHubOptions{Token: testToken})
	// Everyone with a GitHub account is in a child team of engineering.
	_, body := do(t, http.MethodGet, g.URL+"/orgs/acme/teams/engineering/members", nil, "X-Github-Next-Global-ID", "1")
	served := map[string]string{} // login -> node ID
	for _, u := range decode[[]map[string]any](t, body) {
		served[str(u["login"])] = str(u["node_id"])
	}
	links := 0
	var lfischerEnd *timestamppb.Timestamp
	for _, ob := range readFeed(t) {
		e := ob.GetData().GetEntity()
		if e.GetKind() == string(model.KindPerson) {
			username := strings.TrimPrefix(e.GetAliases()[0], "authentik:username/")
			p, ok := o.Person(username)
			if !ok || p.Directory == nil || e.GetKey() != "authentik:user/"+p.Directory.UUID {
				t.Fatalf("%s: no person %s with that UUID in the seed", ob.GetId(), username)
			}
			if !e.GetLinkedIdsComplete() {
				t.Fatalf("%s: the fake reads every link, so the feed must say the list is complete", ob.GetId())
			}
			for _, l := range e.GetLinkedIds() {
				if p.GitHub == nil || served[p.GitHub.Login] == "" ||
					(l != "github:user_node/"+served[p.GitHub.Login] && l != "github:user/"+p.GitHub.Login) {
					t.Fatalf("%s: link %s isn't %s's GitHub account as served (%v)", ob.GetId(), l, p.ID, served)
				}
				links++
			}
		}
		for _, r := range ob.GetData().GetRelations() {
			if e.GetAliases()[0] == "authentik:group_name/engineering" && r.GetFrom() == "authentik:user/9e8d7c6b-5a49-4382-b716-05f4e3d2c1b0" {
				lfischerEnd = r.GetValidTo()
			}
		}
	}
	// jdoe, rpatel and mchen by node ID and login, tbecker by login.
	if links != 7 {
		t.Fatalf("got %d links, want 7", links)
	}
	if lfischerEnd == nil || !lfischerEnd.AsTime().Equal(MembershipEndsAt) {
		t.Fatalf("lfischer's engineering membership ends at %v, want %v", lfischerEnd, MembershipEndsAt)
	}
}
