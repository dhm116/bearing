package conformance

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// pageAll reads every page of a listing, as a caller does: each page asked
// on the window the first one resolved, after the cursor of the one before.
func pageAll(t *testing.T, s contracts.GraphStore, r contracts.ChangesRequest) (all []*modelv1alpha1.FactChange, pages int) {
	t.Helper()
	var w1, w2 time.Time
	for {
		page, err := s.ChangesPage(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if pages == 1 {
			w1, w2 = page.T1, page.T2
			if w1.IsZero() || w2.IsZero() {
				t.Fatalf("page 1 resolved the window to %v, %v, want both times", w1, w2)
			}
		} else if !page.T1.Equal(w1) || !page.T2.Equal(w2) {
			t.Fatalf("page %d is on window %v, %v, want the first page's %v, %v", pages, page.T1, page.T2, w1, w2)
		}
		if n := len(page.Changes); n == 0 || n > r.PageSize() {
			t.Fatalf("page %d has %d changes, want 1 to %d", pages, n, r.PageSize())
		}
		all = append(all, page.Changes...)
		if page.Next == nil {
			return all, pages
		}
		if len(page.Changes) != r.PageSize() {
			t.Fatalf("page %d has a cursor but %d of %d changes", pages, len(page.Changes), r.PageSize())
		}
		if last := page.Changes[len(page.Changes)-1]; page.Next.FactID != last.GetFactId() || !page.Next.ChangedAt.Equal(last.GetChangedAt().AsTime()) {
			t.Fatalf("page %d: cursor %+v is not its last change %v", pages, page.Next, last)
		}
		r.T1, r.T2, r.After = w1, w2, page.Next
		if pages > 10_000 {
			t.Fatal("a listing never ends")
		}
	}
}

// factIDs lists the fact IDs of changes in order.
func factIDs(cs []*modelv1alpha1.FactChange) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.GetFactId()
	}
	return out
}

func (g *suite) changesPage(t *testing.T) {
	s, _ := g.store(t)
	r, _, _ := seed(t, s)
	base := at("2026-09-01T00:00:00Z")
	hour := func(i int) string { return base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339) }
	// 300 teams each take a name at their own hour, and three more facts
	// about the repository take a value at the same instant as team 150.
	cs := &modelv1alpha1.ChangeSet{EventId: "mint"}
	for i := range 300 {
		cs.Mints = append(cs.Mints, mint(fmt.Sprintf("new:t%d", i), "Team"))
	}
	minted := apply(t, s, cs)
	cs = &modelv1alpha1.ChangeSet{EventId: "names"}
	for i := range 300 {
		id := string(minted.Subjects[fmt.Sprintf("new:t%d", i)])
		cs.Supports = append(cs.Supports, supports("github-acme", id, "name", str(fmt.Sprintf("team-%03d", i)), version("github-acme", 1_000_000, hour(i), "")))
		cs.Facts = append(cs.Facts, fact(id, "name", str(fmt.Sprintf("team-%03d", i)), span(asserted, 1_000_000, hour(i), "")))
	}
	for _, topic := range []string{"go", "grpc", "kafka"} {
		cs.Supports = append(cs.Supports, supports("github-acme", r, "topics", str(topic), version("github-acme", 1_000_000, hour(150), "")))
		cs.Facts = append(cs.Facts, fact(r, "topics", str(topic), span(asserted, 1_000_000, hour(150), "")))
	}
	apply(t, s, cs)

	window := contracts.ChangesRequest{T1: at("2026-08-31T00:00:00Z"), T2: at("2026-10-01T00:00:00Z"), Axis: contracts.AxisValid}
	all, err := s.Changes(ctx, contracts.FactFilter{}, window.T1, window.T2, window.Axis)
	if err != nil || len(all) != 303 {
		t.Fatalf("got %d changes, %v, want 303", len(all), err)
	}
	slices.SortFunc(all, contracts.CompareChanges)
	for _, c := range all {
		if c.GetChangedAt() == nil {
			t.Fatalf("Changes: %v has no changed_at", c)
		}
	}
	// The newest is team 299, an hour after team 298; the four facts of hour
	// 150 follow each other by fact ID.
	if got := all[0].GetChangedAt().AsTime(); !got.Equal(base.Add(299 * time.Hour)) {
		t.Fatalf("newest change at %v, want %v", got, base.Add(299*time.Hour))
	}

	// Pages of any size list exactly what Changes does, in the same order.
	for _, limit := range []int{0, 7, 100, 303, 304, contracts.MaxChangesLimit} {
		req := window
		req.Limit = limit
		got, pages := pageAll(t, s, req)
		size := req.PageSize()
		if want := (303 + size - 1) / size; pages != want {
			t.Errorf("limit %d: read %d pages, want %d", limit, pages, want)
		}
		if !slices.Equal(factIDs(got), factIDs(all)) {
			t.Fatalf("limit %d: got\n\t%v\nwant\n\t%v", limit, factIDs(got), factIDs(all))
		}
		for i, c := range got {
			if !proto.Equal(c, all[i]) {
				t.Fatalf("limit %d: change %d is %v, want %v", limit, i, c, all[i])
			}
		}
	}

	// Changes of one instant are told apart by fact ID, and a page may end between them.
	req := window
	req.T1, req.T2, req.Limit = base.Add(149*time.Hour), base.Add(150*time.Hour), 1
	if got, pages := pageAll(t, s, req); len(got) != 4 || pages != 4 || !slices.IsSortedFunc(got, contracts.CompareChanges) {
		t.Errorf("four changes at hour 150, one to a page: got %d changes in %d pages, %v", len(got), pages, factIDs(got))
	}
	// The window is (t1, t2]: a change at t1 is not in it, one at t2 is.
	req = window
	req.T1, req.T2 = base.Add(290*time.Hour), base.Add(295*time.Hour)
	got, _ := pageAll(t, s, req)
	if want := 5; len(got) != want || !got[0].GetChangedAt().AsTime().Equal(base.Add(295*time.Hour)) || !got[4].GetChangedAt().AsTime().Equal(base.Add(291*time.Hour)) {
		t.Errorf("window (290h, 295h]: got %d changes %v, want 5 from 295h to 291h", len(got), factIDs(got))
	}
	// A predicate narrows the listing; a subject does too.
	req = window
	req.Filter = contracts.FactFilter{Predicate: "topics"}
	if got, _ := pageAll(t, s, req); len(got) != 3 {
		t.Errorf("predicate topics: got %d changes, want 3", len(got))
	}
	req.Filter = contracts.FactFilter{SubjectID: contracts.SubjectID(r)}
	req.Limit = 2
	if got, pages := pageAll(t, s, req); len(got) != 3 || pages != 2 {
		t.Errorf("subject %s: got %d changes in %d pages, want 3 in 2", r, len(got), pages)
	}
	// An empty window is an empty page with no cursor.
	req = window
	req.T1, req.T2 = at("2026-10-02T00:00:00Z"), at("2026-10-03T00:00:00Z")
	page, err := s.ChangesPage(ctx, req)
	if err != nil || len(page.Changes) != 0 || page.Next != nil || page.T1.IsZero() {
		t.Errorf("empty window: got %+v, %v, want an empty page with the window", page, err)
	}
	// Zero times are now, and the page says what they were.
	req = window
	req.T1, req.T2 = time.Time{}, at("2026-09-02T00:00:00Z")
	if _, err := s.ChangesPage(ctx, req); err == nil {
		t.Error("a zero t1 (now) after a past t2: got no error")
	}
	req.T1, req.T2 = at("2026-09-20T00:00:00Z"), time.Time{}
	page, err = s.ChangesPage(ctx, req)
	if err != nil || page.T2.IsZero() || len(page.Changes) != 0 {
		t.Errorf("a window to now: got %+v, %v, want no changes and a resolved t2", page, err)
	}
	// Bad requests are errors.
	for name, bad := range map[string]contracts.ChangesRequest{
		"negative limit":  {T1: window.T1, T2: window.T2, Limit: -1},
		"limit too large": {T1: window.T1, T2: window.T2, Limit: contracts.MaxChangesLimit + 1},
		"cursor no fact":  {T1: window.T1, T2: window.T2, After: &contracts.ChangeCursor{ChangedAt: window.T2}},
		"t1 after t2":     {T1: window.T2, T2: window.T1},
		"unknown axis":    {T1: window.T1, T2: window.T2, Axis: contracts.Axis(99)},
		"bad object":      {T1: window.T1, T2: window.T2, Filter: contracts.FactFilter{Object: &modelv1alpha1.FactObject{}}},
	} {
		if _, err := s.ChangesPage(ctx, bad); err == nil {
			t.Errorf("%s: got no error", name)
		}
	}
}

func (g *suite) changedAt(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	one := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e1",
		Supports: []*modelv1alpha1.SupportTimeline{
			supports("a", r, "owned_by", ref(p), version("a", 900_000, "2026-09-01T00:00:00Z", "")),
			supports("a", r, "owned_by", ref(l), version("a", 900_000, "2026-09-01T00:00:00Z", "")),
		},
		Facts: []*modelv1alpha1.FactTimeline{
			// Ownership by P falls twice in confidence and then ends.
			fact(r, "owned_by", ref(p),
				span(asserted, 900_000, "2026-09-01T00:00:00Z", "2026-09-10T00:00:00Z"),
				span(asserted, 800_000, "2026-09-10T00:00:00Z", "2026-09-20T00:00:00Z"),
				span(asserted, 700_000, "2026-09-20T00:00:00Z", "2026-09-25T00:00:00Z")),
			// L's holds at one confidence in three spans, and so has not changed.
			fact(r, "owned_by", ref(l),
				span(asserted, 900_000, "2026-09-01T00:00:00Z", "2026-09-12T00:00:00Z"),
				span(asserted, 900_000, "2026-09-12T00:00:00Z", "2026-09-18T00:00:00Z"),
				span(asserted, 900_000, "2026-09-18T00:00:00Z", "")),
			// A value that rose and fell back to where it started.
			fact(r, "language", str("Go"),
				span(asserted, 900_000, "2026-09-01T00:00:00Z", "2026-09-14T00:00:00Z"),
				span(asserted, 500_000, "2026-09-14T00:00:00Z", "2026-09-16T00:00:00Z"),
				span(asserted, 900_000, "2026-09-16T00:00:00Z", "")),
			// One that begins inside the window.
			fact(r, "language", str("Rust"), span(asserted, 900_000, "2026-09-22T00:00:00Z", "")),
		},
	})
	// changed lists, by "predicate object", when each fact in the window
	// took the value it has at the window's end.
	changed := func(from, to string, axis contracts.Axis) map[string]string {
		t.Helper()
		page, err := s.ChangesPage(ctx, contracts.ChangesRequest{T1: at(from), T2: at(to), Axis: axis})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, c := range page.Changes {
			obj := c.GetObject().GetValue().GetStringValue()
			switch c.GetObject().GetSubjectId() {
			case p:
				obj = "P"
			case l:
				obj = "L"
			}
			out[c.GetPredicate()+" "+obj] = show(c.GetChangedAt())
		}
		return out
	}
	expect := func(what string, got map[string]string, want map[string]string) {
		t.Helper()
		if !maps.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}
	// P's confidence fell on the 10th and 20th and its ownership ended on the
	// 25th; L's held at one confidence across three spans, and Go fell and
	// recovered, so neither changed over a window holding both its steps.
	expect("(5 Sep, 30 Sep]", changed("2026-09-05T00:00:00Z", "2026-09-30T00:00:00Z", contracts.AxisValid),
		map[string]string{"owned_by P": "2026-09-25T00:00:00Z", "language Rust": "2026-09-22T00:00:00Z"})
	// Over a narrower window only the fall on the 20th and Go's recovery are in it.
	expect("(15 Sep, 21 Sep]", changed("2026-09-15T00:00:00Z", "2026-09-21T00:00:00Z", contracts.AxisValid),
		map[string]string{"owned_by P": "2026-09-20T00:00:00Z", "language Go": "2026-09-16T00:00:00Z"})
	// Go fell on the 14th and recovered on the 16th.
	expect("(13 Sep, 15 Sep]", changed("2026-09-13T00:00:00Z", "2026-09-15T00:00:00Z", contracts.AxisValid),
		map[string]string{"language Go": "2026-09-14T00:00:00Z"})
	expect("(15 Sep, 17 Sep]", changed("2026-09-15T00:00:00Z", "2026-09-17T00:00:00Z", contracts.AxisValid),
		map[string]string{"language Go": "2026-09-16T00:00:00Z"})
	// On the record axis nothing was known before e1 was recorded on the
	// 28th, so what holds on the 29th took its value then, however long it had
	// held in the world. P's ownership had ended by then and is not among them.
	expect("record axis, before e1", changed("2026-09-05T00:00:00Z", "2026-09-27T00:00:00Z", contracts.AxisRecord), map[string]string{})
	e1 := one.RecordedAt.UTC().Format(time.RFC3339)
	expect("record axis, across e1", changed("2026-09-27T00:00:00Z", "2026-09-29T00:00:00Z", contracts.AxisRecord), map[string]string{
		"owned_by L": e1, "language Rust": e1, "language Go": e1,
	})
}

func (g *suite) lastChange(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	if _, err := s.LastChange(ctx, contracts.FactFilter{}, time.Time{}, contracts.AxisValid); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("an empty graph: got %v, want ErrNotFound", err)
	}
	one := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e1",
		Supports: []*modelv1alpha1.SupportTimeline{
			supports("a", r, "owned_by", ref(p), version("a", 900_000, "2026-09-01T00:00:00Z", "2026-09-20T00:00:00Z")),
			supports("a", r, "language", str("Go"), version("a", 900_000, "2026-09-05T00:00:00Z", "")),
		},
		Facts: []*modelv1alpha1.FactTimeline{
			fact(r, "owned_by", ref(p),
				span(asserted, 900_000, "2026-09-01T00:00:00Z", "2026-09-10T00:00:00Z"),
				span(asserted, 900_000, "2026-09-10T00:00:00Z", "2026-09-20T00:00:00Z")),
			fact(r, "language", str("Go"), span(asserted, 900_000, "2026-09-05T00:00:00Z", "")),
		},
	})
	last := func(f contracts.FactFilter, when string, axis contracts.Axis) string {
		t0 := time.Time{}
		if when != "" {
			t0 = at(when)
		}
		got, err := s.LastChange(ctx, f, t0, axis)
		if errors.Is(err, contracts.ErrNotFound) {
			return "none"
		}
		if err != nil {
			t.Fatal(err)
		}
		return got.UTC().Format(time.RFC3339Nano)
	}
	everything := contracts.FactFilter{}
	for _, c := range []struct{ at, want string }{
		{"2026-08-31T00:00:00Z", "none"},
		{"2026-09-01T00:00:00Z", "2026-09-01T00:00:00Z"}, // the instant itself counts
		{"2026-09-04T00:00:00Z", "2026-09-01T00:00:00Z"},
		{"2026-09-05T00:00:00Z", "2026-09-05T00:00:00Z"},
		// The split at 10 September changes nothing; the answer stays that of the 5th.
		{"2026-09-15T00:00:00Z", "2026-09-05T00:00:00Z"},
		// P's ownership ends on the 20th.
		{"2026-09-21T00:00:00Z", "2026-09-20T00:00:00Z"},
		{"2026-12-01T00:00:00Z", "2026-09-20T00:00:00Z"},
	} {
		if got := last(everything, c.at, contracts.AxisValid); got != c.want {
			t.Errorf("valid axis at %s: got %s, want %s", c.at, got, c.want)
		}
	}
	if got := last(contracts.FactFilter{Predicate: "language"}, "2026-12-01T00:00:00Z", contracts.AxisValid); got != "2026-09-05T00:00:00Z" {
		t.Errorf("predicate language: got %s, want 2026-09-05", got)
	}
	if got := last(contracts.FactFilter{SubjectID: contracts.SubjectID(l)}, "2026-12-01T00:00:00Z", contracts.AxisValid); got != "none" {
		t.Errorf("a subject with no facts: got %s, want none", got)
	}
	if got := last(contracts.FactFilter{Key: "github:repo_node/R_1", Predicate: "owned_by"}, "2026-12-01T00:00:00Z", contracts.AxisValid); got != "2026-09-20T00:00:00Z" {
		t.Errorf("a key: got %s, want 2026-09-20", got)
	}
	// Zero is now: the clock is at the end of September.
	if got := last(everything, "", contracts.AxisValid); got != "2026-09-20T00:00:00Z" {
		t.Errorf("now: got %s, want 2026-09-20", got)
	}
	// On the record axis, what Bearing answered changed when it recorded e1,
	// and again as valid time passed 20 September.
	if got := last(everything, "2026-09-29T00:00:00Z", contracts.AxisRecord); got != one.RecordedAt.UTC().Format(time.RFC3339Nano) {
		t.Errorf("record axis: got %s, want e1's record time %s", got, one.RecordedAt.UTC().Format(time.RFC3339Nano))
	}
	if got := last(everything, "2026-09-10T00:00:00Z", contracts.AxisRecord); got != "none" {
		t.Errorf("record axis before e1 was recorded: got %s, want none", got)
	}
	if _, err := s.LastChange(ctx, contracts.FactFilter{Object: &modelv1alpha1.FactObject{}}, time.Time{}, contracts.AxisValid); err == nil {
		t.Error("a bad filter object: got no error")
	}
	if _, err := s.LastChange(ctx, everything, time.Time{}, contracts.Axis(99)); err == nil {
		t.Error("an unknown axis: got no error")
	}
}
