// Package identityeval is the labelled evaluation set for identity
// resolution (ADR 7): observations for record pairs whose answer is known,
// a timeline in which evidence stops, and the metrics that score a matcher
// against them. Test helpers and data only; production code never imports
// it.
//
// Labels are complete: a pair of records that no [Case] names is different,
// so a proposal for it is a false positive. People who belong to teams are
// shared by key (both teams name the same person key as a member), so the
// members feature of a matcher is a plain overlap of member records.
package identityeval

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// Observation sources of the set.
const (
	SourceGitHub     = "adapter/github"
	SourceDirectory  = "adapter/authentik"
	SourceKubernetes = "adapter/kubernetes"
)

// Case is one labelled pair of records. A and B are the records' primary
// keys, from different sources.
type Case struct {
	// Group names the variation the case tests: "acme", "convention",
	// "rename", "near-miss", "unrelated" or "person-*".
	Group string
	Kind  model.Kind
	A, B  model.Key
	// Same is the truth: the two records describe one thing.
	Same bool
}

// Set is the observations and the labels for their pairs.
type Set struct {
	Observations []*eventv1alpha1.Observation
	Cases        []Case
}

// Pair is an unordered pair of record keys.
type Pair struct{ A, B model.Key }

// NewPair orders a and b so Pair{a, b} equals Pair{b, a}.
func NewPair(a, b model.Key) Pair {
	if b < a {
		a, b = b, a
	}
	return Pair{a, b}
}

// Pair returns the case's unordered pair.
func (c Case) Pair() Pair { return NewPair(c.A, c.B) }

// Generate builds the set at time at. The same seed gives the same set.
func Generate(seed uint64, at time.Time) Set {
	rng := rand.New(rand.NewPCG(seed, 0x1d)) //nolint:gosec // seeded test data, not security
	g := &gen{rng: rng, at: at, used: map[string]bool{}}
	g.acme()
	g.conventions()
	g.renames()
	g.nearMisses()
	g.unrelated()
	g.persons()
	return Set{Observations: g.obs, Cases: g.cases}
}

type gen struct {
	rng   *rand.Rand
	at    time.Time
	n     int
	used  map[string]bool
	obs   []*eventv1alpha1.Observation
	cases []Case
}

// overlaps are the shares of members two teams have in common.
var overlaps = []float64{0, 0.25, 0.5, 0.75, 1}

// renames are teams that changed names: the pair is one team.
var renames = [][2]string{
	{"sre", "reliability"},
	{"infra", "infrastructure"},
	{"devex", "developer-experience"},
	{"data", "analytics"},
	{"sec", "security"},
	{"growth", "acquisition"},
	{"mobile", "apps"},
	{"billing", "revenue"},
	{"search", "discovery"},
	{"qa", "quality"},
}

// nearMisses are different teams with similar names. No name here is in
// renames or the Acme story: a repeated name would make records of
// different cases look alike, and the labels are complete.
var nearMisses = [][2]string{
	{"ledger", "legend"},
	{"pager", "paper"},
	{"identity", "identify"},
	{"gateway", "gatekeeper"},
	{"routing", "rooting"},
	{"release", "relay"},
	{"core", "care"},
	{"storage", "stooge"},
	{"edge", "ledge"},
	{"ops", "opts"},
	{"site", "sight"},
	{"cloud", "clouds"},
}

// conventions turn a slug into how another system names the team.
var conventions = []func(string) string{
	func(s string) string { return s },
	func(s string) string { return title(s) + " Team" },
	func(s string) string { return s + "-team" },
	func(s string) string { return "team-" + s },
	func(s string) string { return strings.ReplaceAll(s, "-", "_") + "_team" },
}

func title(s string) string {
	r := []rune(s)
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

func slug(name string) string {
	s := strings.ToLower(strings.Join(strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '_' }), "-"))
	return s
}

// word returns a fresh pronounceable word, not used before.
func (g *gen) word() string {
	const cons, vow = "bdfgklmnprstvz", "aeiou"
	for {
		var b strings.Builder
		for range 3 {
			b.WriteByte(cons[g.rng.IntN(len(cons))])
			b.WriteByte(vow[g.rng.IntN(len(vow))])
		}
		if w := b.String(); !g.used[w] {
			g.used[w] = true
			return w
		}
	}
}

func (g *gen) id(prefix string) string {
	g.n++
	return fmt.Sprintf("%s%04d", prefix, g.n)
}

func str(s string) *structpb.Value { return structpb.NewStringValue(s) }

func strs(s ...string) *structpb.Value {
	vals := make([]*structpb.Value, len(s))
	for i, v := range s {
		vals[i] = str(v)
	}
	return structpb.NewListValue(&structpb.ListValue{Values: vals})
}

func (g *gen) emit(source string, d *modelv1alpha1.ObservationData) {
	g.obs = append(g.obs, model.NewObservation(source, g.at, d))
}

// team observes a team in a system, with members as person keys.
func (g *gen) team(source string, key, alias model.Key, name string, members []model.Key) {
	g.emit(source, teamData(key, alias, name, members))
}

func teamData(key, alias model.Key, name string, members []model.Key) *modelv1alpha1.ObservationData {
	rels := make([]*modelv1alpha1.Relation, len(members))
	for i, m := range members {
		rels[i] = &modelv1alpha1.Relation{Type: string(model.RelMemberOf), End: &modelv1alpha1.Relation_From{From: string(m)}}
	}
	e := &modelv1alpha1.Entity{
		Kind: string(model.KindTeam), Key: string(key),
		Attributes: map[string]*structpb.Value{"name": str(name), "slug": str(slug(name))},
	}
	if alias != "" {
		e.Aliases = []string{string(alias)}
	}
	return &modelv1alpha1.ObservationData{
		Entity: e, Relations: rels,
		Snapshots: []*modelv1alpha1.SnapshotScope{{Direction: modelv1alpha1.Direction_DIRECTION_IN, Predicates: []string{string(model.RelMemberOf)}}},
	}
}

func personData(key model.Key, name string, emails ...string) *modelv1alpha1.ObservationData {
	attrs := map[string]*structpb.Value{"name": str(name)}
	if len(emails) > 0 {
		attrs["email"] = strs(emails...)
	}
	return &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{Kind: string(model.KindPerson), Key: string(key), Attributes: attrs}}
}

// members observes n fresh people (in GitHub) and returns their keys.
func (g *gen) members(n int) []model.Key {
	keys := make([]model.Key, n)
	for i := range keys {
		login := g.word()
		keys[i] = model.NewKey("github", "user", login)
		g.emit(SourceGitHub, personData(keys[i], g.fullName(), login+"@acme.example"))
	}
	return keys
}

// split returns the members of two teams that share overlap of n people.
func (g *gen) split(n int, overlap float64) (a, b []model.Key) {
	shared := int(math.Round(overlap * float64(n)))
	all := g.members(2*n - shared)
	return all[:n], append(append([]model.Key{}, all[:shared]...), all[n:]...)
}

func ghTeam(id, slug string) (model.Key, model.Key) {
	return model.NewKey("github", "team_node", id), model.NewKey("github", "team", "acme/"+slug)
}

func dirTeam(id, slug string) (model.Key, model.Key) {
	return model.NewKey("authentik", "group", id), model.NewKey("authentik", "group_name", slug)
}

// pairOfTeams observes a GitHub team and a directory team and labels them.
func (g *gen) pairOfTeams(group, ghName, dirName string, same bool, overlap float64) {
	a, b := g.split(4, overlap)
	ak, aa := ghTeam(g.id("T_"), slug(ghName))
	bk, ba := dirTeam(g.id("g-"), slug(dirName))
	g.team(SourceGitHub, ak, aa, ghName, a)
	g.team(SourceDirectory, bk, ba, dirName, b)
	g.cases = append(g.cases, Case{Group: group, Kind: model.KindTeam, A: ak, B: bk, Same: same})
}

func (g *gen) conventions() {
	for _, conv := range conventions {
		for _, ov := range overlaps {
			for range 2 {
				w := g.word()
				g.pairOfTeams("convention", w, conv(w), true, ov)
			}
		}
	}
}

func (g *gen) renames() {
	for i, r := range renames {
		g.used[r[0]], g.used[r[1]] = true, true
		g.pairOfTeams("rename", r[0], r[1], true, []float64{0, 0.5, 1}[i%3])
	}
}

func (g *gen) nearMisses() {
	for i, r := range nearMisses {
		g.used[r[0]], g.used[r[1]] = true, true
		g.pairOfTeams("near-miss", r[0], r[1], false, []float64{0, 0.25, 0.5}[i%3])
	}
}

func (g *gen) unrelated() {
	for range 20 {
		g.pairOfTeams("unrelated", g.word(), g.word(), false, 0)
	}
}

// acme labels what the Acme story establishes: its three teams are one
// across GitHub, the directory and Kubernetes labels; Jane and Mei are one
// person each; Tom's directory record and GitHub account stay apart (the
// directory's login link is not authoritative evidence).
func (g *gen) acme() {
	for _, name := range []string{"payments", "platform", "sre"} {
		g.used[name] = true
		a, b := g.split(4, 1)
		ak, aa := ghTeam(g.id("T_"), name)
		bk, ba := dirTeam(g.id("g-"), name)
		ck := model.NewKey("kubernetes", "team", name)
		g.team(SourceGitHub, ak, aa, title(name), a)
		g.team(SourceDirectory, bk, ba, name, b)
		g.team(SourceKubernetes, ck, "", name, nil)
		for _, p := range [][2]model.Key{{ak, bk}, {ak, ck}, {bk, ck}} {
			g.cases = append(g.cases, Case{Group: "acme", Kind: model.KindTeam, A: p[0], B: p[1], Same: true})
		}
	}
	person := func(gh, dir, ghName, dirName string, ghEmail, dirEmail string, same bool) {
		ak, bk := model.NewKey("github", "user", gh), model.NewKey("authentik", "user", g.id("u-"))
		g.emit(SourceGitHub, personData(ak, ghName, ghEmail))
		g.emit(SourceDirectory, personData(bk, dirName, dirEmail))
		g.cases = append(g.cases, Case{Group: "acme", Kind: model.KindPerson, A: ak, B: bk, Same: same})
	}
	person("jdoe", "jdoe", "Jane Doe", "Jane Doe", "jdoe@acme.example", "jdoe@acme.example", true)
	person("meichen", "mchen", "Mei Chen", "Mei Chen", "mchen@acme.example", "mchen@acme.example", true)
	person("tbecker", "tbecker", "Tom Becker", "Tom Becker", "tom.becker@users.noreply.example", "tbecker@acme.example", false)
	for _, w := range []string{"jdoe", "meichen", "tbecker"} {
		g.used[w] = true
	}
}

// fullName is a name no other person has, so a shared name is only ever
// the case's doing.
func (g *gen) fullName() string { return title(g.word()) + " " + title(g.word()) }

// persons labels people across GitHub and the directory by name and email.
func (g *gen) persons() {
	add := func(group string, same bool, ghName, dirName, ghEmail, dirEmail string) {
		login := g.word()
		ak, bk := model.NewKey("github", "user", login), model.NewKey("authentik", "user", g.id("u-"))
		g.emit(SourceGitHub, personData(ak, ghName, ghEmail))
		g.emit(SourceDirectory, personData(bk, dirName, dirEmail))
		g.cases = append(g.cases, Case{Group: group, Kind: model.KindPerson, A: ak, B: bk, Same: same})
	}
	email := func() string { return g.word() + "@acme.example" }
	for range 15 { // same name, same email
		n, e := g.fullName(), email()
		add("person-same-email", true, n, n, e, e)
	}
	for range 10 { // a nickname or login for a name, same email
		n, e := g.fullName(), email()
		add("person-alias", true, n, strings.ToLower(strings.Fields(n)[0]), e, e)
	}
	for range 15 { // a shared name, different people
		n := g.fullName()
		add("person-same-name", false, n, n, email(), email())
	}
	for range 15 {
		add("person-different", false, g.fullName(), g.fullName(), email(), email())
	}
}
