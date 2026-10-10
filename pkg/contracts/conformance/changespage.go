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
			fatalIf(t, w1.IsZero() || w2.IsZero(), "page 1 resolved the window to %v, %v, want both times", w1, w2)
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
		fatalIf(t, len(page.Changes) != r.PageSize(), "page %d has a cursor but %d of %d changes", pages, len(page.Changes), r.PageSize())
		if last := page.Changes[len(page.Changes)-1]; page.Next.FactID != last.GetFactId() || !page.Next.ChangedAt.Equal(last.GetChangedAt().AsTime()) {
			t.Fatalf("page %d: cursor %+v is not its last change %v", pages, page.Next, last)
		}
		r.T1, r.T2, r.After = w1, w2, page.Next
		fatalIf(t, pages > 10_000, "a listing never ends")
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
	fatalIf(t, err != nil || len(all) != 303, "got %d changes, %v, want 303", len(all), err)
	slices.SortFunc(all, contracts.CompareChanges)
	for _, c := range all {
		fatalIf(t, c.GetChangedAt() == nil, "Changes: %v has no changed_at", c)
	}
	// The newest is team 299, an hour after team 298; the four facts of hour
	// 150 follow each other by fact ID.
	newest := all[0].GetChangedAt().AsTime()
	fatalIf(t, !newest.Equal(base.Add(299*time.Hour)), "newest change at %v, want %v", newest, base.Add(299*time.Hour))

	// A filter whose object holds only attributes, or whose key is bound to
	// nothing, lists what Changes does too.
	for name, f := range map[string]contracts.FactFilter{
		"an attribute object":               {Object: str("team-007")},
		"an attribute object and predicate": {Object: str("go"), Predicate: "topics"},
		"an attribute object nothing holds": {Object: str("no such value")},
		"a key bound to nothing":            {Key: "github:repo_node/NOPE"},
	} {
		req := window
		req.Filter = f
		want, err := s.Changes(ctx, f, window.T1, window.T2, window.Axis)
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(want, contracts.CompareChanges)
		page, err := s.ChangesPage(ctx, req)
		errorIf(t, err != nil || !slices.Equal(factIDs(page.Changes), factIDs(want)), "%s: got %v, %v, want %v", name, factIDs(page.Changes), err, factIDs(want))
	}
	one, _ := s.ChangesPage(ctx, contracts.ChangesRequest{Filter: contracts.FactFilter{Object: str("team-007")}, T1: window.T1, T2: window.T2})
	errorIf(t, len(one.Changes) != 1, "an attribute object: got %d changes, want the one team name", len(one.Changes))

	// Pages of any size list exactly what Changes does, in the same order.
	for _, limit := range []int{0, 7, 100, 303, 304, contracts.MaxChangesLimit} {
		req := window
		req.Limit = limit
		got, pages := pageAll(t, s, req)
		size := req.PageSize()
		want := (303 + size - 1) / size
		errorIf(t, pages != want, "limit %d: read %d pages, want %d", limit, pages, want)
		fatalIf(t, !slices.Equal(factIDs(got), factIDs(all)), "limit %d: got\n\t%v\nwant\n\t%v", limit, factIDs(got), factIDs(all))
		for i, c := range got {
			if !proto.Equal(c, all[i]) {
				t.Fatalf("limit %d: change %d is %v, want %v", limit, i, c, all[i])
			}
		}
	}

	// Changes of one instant are told apart by fact ID, and a page may end between them.
	req := window
	req.T1, req.T2, req.Limit = base.Add(149*time.Hour), base.Add(150*time.Hour), 1
	got, pages := pageAll(t, s, req)
	errorIf(t, len(got) != 4 || pages != 4 || !slices.IsSortedFunc(got, contracts.CompareChanges), "four changes at hour 150, one to a page: got %d changes in %d pages, %v", len(got), pages, factIDs(got))
	// The window is (t1, t2]: a change at t1 is not in it, one at t2 is.
	req = window
	req.T1, req.T2 = base.Add(290*time.Hour), base.Add(295*time.Hour)
	got, _ = pageAll(t, s, req)
	errorIf(t, len(got) != 5 || !got[0].GetChangedAt().AsTime().Equal(base.Add(295*time.Hour)) || !got[4].GetChangedAt().AsTime().Equal(base.Add(291*time.Hour)), "window (290h, 295h]: got %d changes %v, want 5 from 295h to 291h", len(got), factIDs(got))
	// A predicate narrows the listing; a subject does too.
	req = window
	req.Filter = contracts.FactFilter{Predicate: "topics"}
	got, _ = pageAll(t, s, req)
	errorIf(t, len(got) != 3, "predicate topics: got %d changes, want 3", len(got))
	req.Filter = contracts.FactFilter{SubjectID: contracts.SubjectID(r)}
	req.Limit = 2
	got, pages = pageAll(t, s, req)
	errorIf(t, len(got) != 3 || pages != 2, "subject %s: got %d changes in %d pages, want 3 in 2", r, len(got), pages)
	// An empty window is an empty page with no cursor.
	req = window
	req.T1, req.T2 = at("2026-10-02T00:00:00Z"), at("2026-10-03T00:00:00Z")
	page, err := s.ChangesPage(ctx, req)
	errorIf(t, err != nil || len(page.Changes) != 0 || page.Next != nil || page.T1.IsZero(), "empty window: got %+v, %v, want an empty page with the window", page, err)
	// Zero times are now, and the page says what they were.
	req = window
	req.T1, req.T2 = time.Time{}, at("2026-09-02T00:00:00Z")
	_, err = s.ChangesPage(ctx, req)
	errorIf(t, err == nil, "a zero t1 (now) after a past t2: got no error")
	req.T1, req.T2 = at("2026-09-20T00:00:00Z"), time.Time{}
	page, err = s.ChangesPage(ctx, req)
	errorIf(t, err != nil || page.T2.IsZero() || len(page.Changes) != 0, "a window to now: got %+v, %v, want no changes and a resolved t2", page, err)
	// Bad requests are errors.
	for name, bad := range map[string]contracts.ChangesRequest{
		"negative limit":  {T1: window.T1, T2: window.T2, Limit: -1},
		"limit too large": {T1: window.T1, T2: window.T2, Limit: contracts.MaxChangesLimit + 1},
		"cursor no fact":  {T1: window.T1, T2: window.T2, After: &contracts.ChangeCursor{ChangedAt: window.T2}},
		"t1 after t2":     {T1: window.T2, T2: window.T1},
		"unknown axis":    {T1: window.T1, T2: window.T2, Axis: contracts.Axis(99)},
		"bad object":      {T1: window.T1, T2: window.T2, Filter: contracts.FactFilter{Object: &modelv1alpha1.FactObject{}}},
		// Refused even when no fact changed in the window to meet it.
		"bad object in an empty window": {T1: at("2026-10-02T00:00:00Z"), T2: at("2026-10-03T00:00:00Z"), Filter: contracts.FactFilter{Object: &modelv1alpha1.FactObject{}}},
	} {
		_, err := s.ChangesPage(ctx, bad)
		errorIf(t, err == nil, "%s: got no error", name)
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
		errorIf(t, !maps.Equal(got, want), "%s: got %v, want %v", what, got, want)
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
	_, err := s.LastChange(ctx, contracts.FactFilter{}, time.Time{}, contracts.AxisValid)
	fatalIf(t, !errors.Is(err, contracts.ErrNotFound), "an empty graph: got %v, want ErrNotFound", err)
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
	e1 := one.RecordedAt.UTC().Format(time.RFC3339Nano)
	const (
		validAt = "2026-12-01T00:00:00Z"
		sep20   = "2026-09-20T00:00:00Z"
		sep5    = "2026-09-05T00:00:00Z"
	)
	for _, c := range []struct {
		name string
		f    contracts.FactFilter
		at   string
		axis contracts.Axis
		want string
	}{
		{"before everything", everything, "2026-08-31T00:00:00Z", contracts.AxisValid, "none"},
		{"the instant itself counts", everything, "2026-09-01T00:00:00Z", contracts.AxisValid, "2026-09-01T00:00:00Z"},
		{"between two starts", everything, "2026-09-04T00:00:00Z", contracts.AxisValid, "2026-09-01T00:00:00Z"},
		{"the second start", everything, sep5, contracts.AxisValid, sep5},
		// The split at 10 September changes nothing; the answer stays that of the 5th.
		{"after a split that changes nothing", everything, "2026-09-15T00:00:00Z", contracts.AxisValid, sep5},
		// P's ownership ends on the 20th.
		{"after an end", everything, "2026-09-21T00:00:00Z", contracts.AxisValid, sep20},
		{"long after", everything, validAt, contracts.AxisValid, sep20},
		{"predicate language", contracts.FactFilter{Predicate: "language"}, validAt, contracts.AxisValid, sep5},
		{"a subject with no facts", contracts.FactFilter{SubjectID: contracts.SubjectID(l)}, validAt, contracts.AxisValid, "none"},
		{"a key", contracts.FactFilter{Key: "github:repo_node/R_1", Predicate: "owned_by"}, validAt, contracts.AxisValid, sep20},
		{"a key bound to nothing", contracts.FactFilter{Key: "github:repo_node/NOPE"}, validAt, contracts.AxisValid, "none"},
		// An object of attributes only matches the facts that hold it, with or
		// without a predicate to narrow the search.
		{"an attribute object", contracts.FactFilter{Object: str("Go")}, validAt, contracts.AxisValid, sep5},
		{"an attribute object under another predicate", contracts.FactFilter{Object: str("Go"), Predicate: "owned_by"}, validAt, contracts.AxisValid, "none"},
		// Zero is now: the clock is at the end of September.
		{"now", everything, "", contracts.AxisValid, sep20},
		// On the record axis, what Bearing answered changed when it recorded e1.
		{"record axis", everything, "2026-09-29T00:00:00Z", contracts.AxisRecord, e1},
		{"record axis before e1 was recorded", everything, "2026-09-10T00:00:00Z", contracts.AxisRecord, "none"},
	} {
		got := last(c.f, c.at, c.axis)
		errorIf(t, got != c.want, "%s: got %s, want %s", c.name, got, c.want)
	}
	for name, bad := range map[string]struct {
		f    contracts.FactFilter
		axis contracts.Axis
	}{
		"a bad filter object": {contracts.FactFilter{Object: &modelv1alpha1.FactObject{}}, contracts.AxisValid},
		"an unknown axis":     {everything, contracts.Axis(99)},
	} {
		_, err := s.LastChange(ctx, bad.f, time.Time{}, bad.axis)
		errorIf(t, err == nil, "%s: got no error", name)
	}
	// Refused even when nothing changed before the time asked about.
	_, err = s.LastChange(ctx, contracts.FactFilter{Object: &modelv1alpha1.FactObject{}}, at("2026-08-01T00:00:00Z"), contracts.AxisValid)
	errorIf(t, err == nil || errors.Is(err, contracts.ErrNotFound), "a bad filter object before any change: got %v, want an error that is not ErrNotFound", err)
}

// fatalIf stops the test with the message if bad. The message's arguments
// are evaluated either way, so they must be safe to evaluate when it passes.
func fatalIf(t *testing.T, bad bool, format string, args ...any) {
	t.Helper()
	if bad {
		t.Fatalf(format, args...)
	}
}

// errorIf fails the test with the message if bad, and carries on.
func errorIf(t *testing.T, bad bool, format string, args ...any) {
	t.Helper()
	if bad {
		t.Errorf(format, args...)
	}
}
