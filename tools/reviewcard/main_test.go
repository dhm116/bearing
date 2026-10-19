package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSVGIsWellFormedInEveryState(t *testing.T) {
	rd := round{Round: 3, Reviewers: []reviewer{
		{Name: "security", State: "queued"},
		{Name: "data", State: "running"},
		{Name: "go", State: "approve", Suggestions: 1},
		{Name: "spec", State: "discuss", Blockers: []string{`Is "--json" breaking? <yes>`}},
		{Name: "search", State: "changes", Blockers: []string{"a", "b", "c", "d"}, Suggestions: 2},
	}}
	for _, th := range themes {
		s := svg(th, rd)
		dec := xml.NewDecoder(strings.NewReader(s))
		for {
			if _, err := dec.Token(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", th.name, err)
			}
		}
		for _, want := range []string{"Queued", "In progress", "Approve", "Needs discussion", "Request changes", "+2 more", "3 OF 5 DONE", th.bad} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: got no %q", th.name, want)
			}
		}
	}
}

func TestSVGHidesTheOverallPillForOneReviewer(t *testing.T) {
	one := svg(themes[0], round{Round: 1, Reviewers: []reviewer{{Name: "go", State: "approve"}}})
	if strings.Contains(one, "Approved") {
		t.Error("got the overall pill on a one-reviewer round")
	}
	two := svg(themes[0], round{Round: 1, Reviewers: []reviewer{{Name: "go", State: "approve"}, {Name: "spec", State: "approve"}}})
	if !strings.Contains(two, "Approved") {
		t.Error("got no overall pill on a two-reviewer round")
	}
}

func TestOverallWaitsForEveryReviewerThenTakesTheWorst(t *testing.T) {
	for _, c := range []struct {
		name   string
		states []string
		want   string
	}{
		{"all approve", []string{"approve", "approve"}, "approve"},
		{"one running", []string{"changes", "running"}, "running"},
		{"discuss beats approve", []string{"approve", "discuss"}, "discuss"},
		{"changes beats discuss", []string{"changes", "discuss"}, "changes"},
		{"discuss after changes", []string{"discuss", "changes"}, "changes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var rs []reviewer
			for _, s := range c.states {
				rs = append(rs, reviewer{State: s})
			}
			if got := overall(rs); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestLinesCapsBlockersWithoutChangingThem(t *testing.T) {
	r := reviewer{Blockers: []string{"a", "b", "c", "d", "e"}}
	if got, want := lines(r), []string{"a", "b", "+3 more"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if want := []string{"a", "b", "c", "d", "e"}; !reflect.DeepEqual(r.Blockers, want) {
		t.Fatalf("blockers changed to %q", r.Blockers)
	}
}

func TestLoadRoundRejectsBadInput(t *testing.T) {
	for _, c := range []struct{ name, json, want string }{
		{"no round", `{"reviewers":[{"name":"go","state":"approve"}]}`, "round must be"},
		{"no reviewers", `{"round":1}`, "no reviewers"},
		{"unknown reviewer", `{"round":1,"reviewers":[{"name":"perf","state":"approve"}]}`, "unknown reviewer"},
		{"unknown state", `{"round":1,"reviewers":[{"name":"go","state":"lgtm"}]}`, "unknown state"},
		{"findings while running", `{"round":1,"reviewers":[{"name":"go","state":"running","suggestions":1}]}`, "has findings"},
		{"approve with blockers", `{"round":1,"reviewers":[{"name":"go","state":"approve","blockers":["x"]}]}`, "approves with blocking"},
		{"unknown field", `{"round":1,"reviewers":[{"name":"go","state":"approve","summary":"x"}]}`, "unknown field"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "round.json")
			if err := os.WriteFile(p, []byte(c.json), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadRound(p); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

func TestLoadRoundAcceptsAgentNames(t *testing.T) {
	p := filepath.Join(t.TempDir(), "round.json")
	if err := os.WriteFile(p, []byte(`{"round":2,"reviewers":[{"name":"security-reviewer","state":"running"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := loadRound(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := rd.Reviewers[0].Name; got != "security" {
		t.Fatalf("got %q, want %q", got, "security")
	}
}

// fakeGH plays GitHub for post: it serves the comment list, appends uploads
// as gh does, and records every call. PATCH bodies are read from their files.
type fakeGH struct {
	me       string
	comments []comment
	calls    [][]string
	patched  string
	nextID   int64
}

func (f *fakeGH) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch {
	case args[0] == "api" && args[1] == "user":
		return []byte(f.me + "\n"), nil
	case args[0] == "api" && args[1] == "--paginate":
		var b strings.Builder
		for _, c := range f.comments {
			j, _ := json.Marshal(c)
			b.Write(append(j, '\n'))
		}
		return []byte(b.String()), nil
	case args[0] == "api" && args[1] == "graphql":
		return []byte(`{}`), nil
	case args[0] == "pr" && args[1] == "comment":
		bodyFile := args[indexOf(args, "--body-file")+1]
		b, err := os.ReadFile(bodyFile)
		if err != nil {
			return nil, err
		}
		text := string(b)
		for i, a := range args {
			if a == "--attach" {
				name := strings.TrimSuffix(filepath.Base(args[i+1]), ".svg")
				text += "\n![" + name + "](https://github.com/user-attachments/assets/" + name + "-uuid)\n"
			}
		}
		if indexOf(args, "--edit-last") >= 0 {
			for i := len(f.comments) - 1; i >= 0; i-- {
				if f.comments[i].Login == f.me {
					f.comments[i].Body = text
					break
				}
			}
		} else {
			f.nextID++
			f.comments = append(f.comments, comment{ID: f.nextID, NodeID: "new", Login: f.me, Body: text})
		}
		return nil, nil
	case args[0] == "api" && args[1] == "-X":
		b, err := os.ReadFile(strings.TrimPrefix(args[len(args)-1], "body=@"))
		if err != nil {
			return nil, err
		}
		f.patched = string(b)
		return []byte(`{}`), nil
	}
	return nil, errors.New("unexpected call: " + strings.Join(args, " "))
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

// hidden lists the node IDs the fake was asked to minimize.
func (f *fakeGH) hidden() []string {
	var ids []string
	for _, c := range f.calls {
		if c[1] == "graphql" {
			ids = append(ids, strings.TrimPrefix(c[len(c)-1], "id="))
		}
	}
	return ids
}

func (f *fakeGH) posted() []string {
	for _, c := range f.calls {
		if c[0] == "pr" {
			return c
		}
	}
	return nil
}

var round2 = round{Round: 2, Reviewers: []reviewer{{Name: "security", State: "running"}, {Name: "go", State: "running"}}}

func TestPostStartsARoundAndHidesOnlyTheLeadsOldReviews(t *testing.T) {
	f := &fakeGH{me: "lead", nextID: 100, comments: []comment{
		{ID: 1, NodeID: "old-format", Login: "lead", Body: "## Code Review: Round 1\n..."},
		{ID: 2, NodeID: "old-card", Login: "lead", Body: marker(1) + "\n<picture>..."},
		{ID: 3, NodeID: "ci-note", Login: "lead", Body: "CI note: flaky test re-run"},
		{ID: 4, NodeID: "human", Login: "someone", Body: "## Code Review: my own notes"},
	}}
	id, err := post(f.run, 7, round2, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.hidden(), []string{"old-format", "old-card"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hid %q, want %q", got, want)
	}
	if indexOf(f.posted(), "--edit-last") >= 0 {
		t.Fatal("got --edit-last for a new round")
	}
	if id != 101 {
		t.Fatalf("got comment %d, want 101", id)
	}
	for _, want := range []string{
		marker(2),
		`srcset="https://github.com/user-attachments/assets/review-round-2-dark-uuid"`,
		`src="https://github.com/user-attachments/assets/review-round-2-light-uuid"`,
	} {
		if !strings.Contains(f.patched, want) {
			t.Errorf("patched body has no %q:\n%s", want, f.patched)
		}
	}
	if strings.Contains(f.patched, "![") {
		t.Errorf("patched body keeps gh's appended images:\n%s", f.patched)
	}
}

func TestPostEditsTheRoundCommentInPlace(t *testing.T) {
	f := &fakeGH{me: "lead", comments: []comment{
		{ID: 1, NodeID: "old", Login: "lead", Body: "## Code Review: Round 1"},
		{ID: 9, NodeID: "this", Login: "lead", Body: marker(2) + "\n<picture>..."},
		{ID: 10, NodeID: "human", Login: "someone", Body: "Looks fine"},
	}}
	done := round{Round: 2, Reviewers: []reviewer{
		{Name: "security", State: "changes", Blockers: []string{"Body read before the cap"}, Suggestions: 3},
		{Name: "go", State: "approve", Suggestions: 1},
	}}
	id, err := post(f.run, 7, done, "<details>tables</details>", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if id != 9 {
		t.Fatalf("got comment %d, want 9", id)
	}
	if indexOf(f.posted(), "--edit-last") < 0 {
		t.Fatal("got a new comment, want --edit-last")
	}
	if len(f.hidden()) != 0 {
		t.Fatalf("hid %q during an update", f.hidden())
	}
	for _, want := range []string{"<details>tables</details>", "Body read before the cap", "review-round-2-light-uuid"} {
		if !strings.Contains(f.patched, want) {
			t.Errorf("patched body has no %q:\n%s", want, f.patched)
		}
	}
}

func TestPostRefusesWhenTheRoundCommentIsNotTheLatest(t *testing.T) {
	f := &fakeGH{me: "lead", comments: []comment{
		{ID: 9, Login: "lead", Body: marker(2)},
		{ID: 11, Login: "lead", Body: "CI note"},
	}}
	_, err := post(f.run, 7, round2, "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not your latest") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if f.posted() != nil {
		t.Fatal("posted despite the refusal")
	}
}

func TestRunRendersWithoutPosting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "round.json")
	if err := os.WriteFile(p, []byte(`{"round":1,"reviewers":[{"name":"go","state":"approve"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cards")
	var stdout, stderr strings.Builder
	called := false
	never := func(...string) ([]byte, error) { called = true; return nil, nil }
	if code := run([]string{"-round", p, "-out", out}, &stdout, &stderr, never); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if called {
		t.Fatal("ran gh while rendering")
	}
	for _, f := range []string{"review-round-1-light.svg", "review-round-1-dark.svg", "body.md"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("got no %s: %v", f, err)
		}
	}
	if code := run([]string{"-round", p}, &stdout, &stderr, never); code != 2 {
		t.Fatalf("got exit %d without -pr or -out, want 2", code)
	}
}
