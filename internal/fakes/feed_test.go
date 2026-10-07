package fakes

import (
	"bufio"
	"bytes"
	"flag"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/model"
)

var update = flag.Bool("update", false, "rewrite the recorded directory feed")

// feedPath is the recorded directory feed, shared by tests and demos.
const feedPath = "../../testdata/acme/directory.ndjson"

// directorySource is the CloudEvents source of the directory feed.
const directorySource = "adapter/authentik"

// recordFeed reads the directory fake as an Authentik adapter would and
// returns its observations under the reference declaration: one per user,
// then one per group with its members (users and child groups) as
// member_of claims and a snapshot scope over them.
func recordFeed(t *testing.T, d *Directory, at time.Time) []*eventv1alpha1.Observation {
	t.Helper()
	users := listAll[akUser](t, d.URL, "/api/v3/core/users/")
	groups := listAll[akGroup](t, d.URL, "/api/v3/core/groups/")
	type conn struct {
		User       int    `json:"user"`
		Identifier string `json:"identifier"`
	}
	githubID := map[int]int64{} // user pk -> numeric GitHub user ID
	for _, c := range listAll[conn](t, d.URL, "/api/v3/sources/user_connections/oauth/?source__slug=github") {
		id, err := strconv.ParseInt(c.Identifier, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		githubID[c.User] = id
	}
	var obs []*eventv1alpha1.Observation
	userKey := map[int]string{}
	for _, u := range users {
		key := string(model.NewKey("authentik", "user", u.UUID))
		userKey[u.PK] = key
		e := &modelv1alpha1.Entity{
			Kind: string(model.KindPerson), Key: key,
			Aliases: []string{string(model.NewKey("authentik", "username", u.Username))},
			Attributes: map[string]*structpb.Value{
				"name":  structpb.NewStringValue(u.Name),
				"email": structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{structpb.NewStringValue(u.Email)}}),
			},
		}
		// The OAuth connection is the authoritative link: the next-format
		// node ID derives from the numeric ID it records.
		if id, ok := githubID[u.PK]; ok {
			e.LinkedIds = append(e.LinkedIds, string(model.NewKey("github", "user_node", nextNodeID("U", id))))
		}
		if gh := u.Attributes.GitHub; gh != nil && gh.Login != "" {
			e.LinkedIds = append(e.LinkedIds, string(model.NewKey("github", "user", gh.Login)))
		}
		obs = append(obs, model.NewObservation(directorySource, at, &modelv1alpha1.ObservationData{
			Entity: e, Evidence: &modelv1alpha1.Evidence{Url: UserURL(u.PK)},
		}))
	}
	for _, g := range groups {
		periods := map[int]akPeriod{}
		for _, p := range g.Attributes.MembershipPeriods {
			periods[p.User] = p
		}
		var rels []*modelv1alpha1.Relation
		for _, pk := range g.Users {
			r := &modelv1alpha1.Relation{Type: string(model.RelMemberOf), End: &modelv1alpha1.Relation_From{From: userKey[pk]}}
			if p, ok := periods[pk]; ok {
				r.ValidFrom, r.ValidTo = timestamp(t, p.Start), timestamp(t, p.End)
			}
			rels = append(rels, r)
		}
		for _, child := range groups {
			if child.Parent != nil && *child.Parent == g.PK {
				rels = append(rels, &modelv1alpha1.Relation{
					Type: string(model.RelMemberOf),
					End:  &modelv1alpha1.Relation_From{From: string(model.NewKey("authentik", "group", child.PK))},
				})
			}
		}
		obs = append(obs, model.NewObservation(directorySource, at, &modelv1alpha1.ObservationData{
			Entity: &modelv1alpha1.Entity{
				Kind: string(model.KindTeam), Key: string(model.NewKey("authentik", "group", g.PK)),
				Aliases:    []string{string(model.NewKey("authentik", "group_name", g.Name))},
				Attributes: map[string]*structpb.Value{"name": structpb.NewStringValue(g.Name)},
			},
			Relations: rels,
			Snapshots: []*modelv1alpha1.SnapshotScope{{Direction: modelv1alpha1.Direction_DIRECTION_IN, Predicates: []string{string(model.RelMemberOf)}}},
			Evidence:  &modelv1alpha1.Evidence{Url: GroupURL(g.PK)},
		}))
	}
	return obs
}

func timestamp(t *testing.T, s *string) *timestamppb.Timestamp {
	t.Helper()
	if s == nil {
		return nil
	}
	ts, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		t.Fatal(err)
	}
	return timestamppb.New(ts)
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
	recorded := readFeed(t)
	if len(recorded) != len(obs) {
		t.Fatalf("got %d recorded observations, want %d (run go test ./internal/fakes -update and review the diff)", len(recorded), len(obs))
	}
	for i := range obs {
		if !proto.Equal(recorded[i], obs[i]) {
			t.Fatalf("%s line %d differs from the fake (run go test ./internal/fakes -update and review the diff):\n got %v\nwant %v",
				feedPath, i+1, recorded[i], obs[i])
		}
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
