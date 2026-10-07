package fakes

import (
	"errors"
	"testing"
	"time"

	"bearing.example/internal/testkit"
)

// play applies the story's steps up to and including until, setting the
// clock to each step's time first.
func play(t *testing.T, o *Org, c *testkit.FakeClock, until time.Time) {
	t.Helper()
	for _, s := range Story() {
		if s.At.After(until) {
			break
		}
		c.Set(s.At)
		if s.Apply != nil {
			if err := s.Apply(o); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
	c.Set(until)
}

func TestStoryIsInOrderAndApplies(t *testing.T) {
	steps := Story()
	for i := 1; i < len(steps); i++ {
		if !steps[i].At.After(steps[i-1].At) {
			t.Fatalf("step %q is not after %q", steps[i].Name, steps[i-1].Name)
		}
	}
	c := testkit.NewClock(Start)
	o := NewOrg(c)
	play(t, o, c, MembershipEndsAt.Add(time.Hour))

	r, ok := o.Repo("payments")
	if !ok || r.NodeID() != nextNodeID("R", 525776495) {
		t.Fatalf("got payments %+v, %v; want the renamed payments-api", r, ok)
	}
	if got, want := r.Files[".github/CODEOWNERS"], "# Payments service\n* @acme/platform\n"; got != want {
		t.Fatalf("got CODEOWNERS %q, want %q", got, want)
	}
	if !r.UpdatedAt.Equal(CodeownersChangedAt) {
		t.Fatalf("got updated_at %v, want %v", r.UpdatedAt, CodeownersChangedAt)
	}
	if _, ok := o.Repo("payments-api"); ok {
		t.Fatal("the old name still finds the repository")
	}
	if tm, ok := o.Team("reliability"); !ok || tm.ID != "sre" || tm.GitHub.Name != "Reliability" {
		t.Fatalf("got %+v, %v; want sre renamed to Reliability", tm, ok)
	}
	if tm, _ := o.Team("legacy-ops"); !tm.GitHub.Deleted {
		t.Fatal("legacy-ops is not deleted")
	}
	if _, ok := o.Person("meichen"); !ok {
		t.Fatal("a GitHub login doesn't find its person")
	}
	if _, ok := o.Person("lfischer"); !ok {
		t.Fatal("a directory username doesn't find its person")
	}
}

func TestMembershipEndsOnlyInTheDirectoryUntilRemoved(t *testing.T) {
	scheduled := Membership{From: day(2026, 3, 1), Until: MembershipEndsAt}
	removed := Membership{From: day(2026, 3, 1), Ended: day(2026, 10, 10)}
	tests := []struct {
		name                string
		m                   Membership
		at                  time.Time
		directory, onGitHub bool
	}{
		{"before start", scheduled, day(2026, 2, 28), false, false},
		{"at start", scheduled, day(2026, 3, 1), true, true},
		{"just before the scheduled end", scheduled, MembershipEndsAt.Add(-time.Microsecond), true, true},
		{"at the scheduled end", scheduled, MembershipEndsAt, false, true},
		{"before removal", removed, day(2026, 10, 9), true, true},
		{"at removal", removed, day(2026, 10, 10), false, false},
		{"unbounded", Membership{}, Start, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.InDirectoryAt(tt.at); got != tt.directory {
				t.Errorf("InDirectoryAt: got %v, want %v", got, tt.directory)
			}
			if got := tt.m.OnGitHubAt(tt.at); got != tt.onGitHub {
				t.Errorf("OnGitHubAt: got %v, want %v", got, tt.onGitHub)
			}
		})
	}
}

func TestMembershipChangesUseTheClock(t *testing.T) {
	c := testkit.NewClock(Start)
	o := NewOrg(c)
	c.Set(day(2026, 10, 10))
	if err := o.EndMembership("rpatel", "payments"); err != nil {
		t.Fatal(err)
	}
	if err := o.EndMembership("rpatel", "payments"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ending it twice: got %v, want ErrNotFound", err)
	}
	c.Set(day(2026, 10, 12))
	if err := o.AddMembership("rpatel", "platform", "member"); err != nil {
		t.Fatal(err)
	}
	if err := o.AddMembership("rpatel", "platform", "member"); err == nil {
		t.Fatal("adding an active membership again succeeded")
	}
	var ended, added bool
	for _, m := range o.Memberships() {
		if m.Person == "rpatel" && m.Team == "payments" && m.Ended.Equal(day(2026, 10, 10)) {
			ended = true
		}
		if m.Person == "rpatel" && m.Team == "platform" && m.From.Equal(day(2026, 10, 12)) && m.Ended.IsZero() {
			added = true
		}
	}
	if !ended || !added {
		t.Fatalf("got memberships %+v; want rpatel's payments ended on 10-10 and platform from 10-12", o.Memberships())
	}
}

func TestMutationsRejectUnknownTargets(t *testing.T) {
	o := NewOrg(testkit.NewClock(Start))
	tests := []struct {
		name string
		err  error
	}{
		{"rename missing repo", o.RenameRepo("nope", "x")},
		{"set file in missing repo", o.SetFile("nope", "a", "b")},
		{"delete missing file", o.DeleteFile("sandbox", "nope")},
		{"rename missing team", o.RenameTeam("nope", "X")},
		{"delete missing team", o.DeleteTeam("nope")},
		{"add for missing person", o.AddMembership("nope", "payments", "member")},
		{"end for missing team", o.EndMembership("jdoe", "nope")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, ErrNotFound) {
				t.Fatalf("got %v, want ErrNotFound", tt.err)
			}
		})
	}
	if err := o.RenameRepo("web", "sandbox"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("renaming onto an existing repo: got %v, want an error", err)
	}
	if err := o.DeleteTeam("legacy-ops"); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteTeam("legacy-ops"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: got %v, want ErrNotFound", err)
	}
}

func TestLookupsReturnCopies(t *testing.T) {
	o := NewOrg(nil)
	r, _ := o.Repo("web")
	r.Files["CODEOWNERS"] = "changed"
	r.Topics[0] = "changed"
	p, _ := o.Person("jdoe")
	p.GitHub.Login = "changed"
	tm, _ := o.Team("payments")
	tm.GitHub.Slug = "changed"
	if again, _ := o.Repo("web"); again.Files["CODEOWNERS"] == "changed" || again.Topics[0] == "changed" {
		t.Fatal("changing a returned repo changed the org")
	}
	if _, ok := o.Person("jdoe"); !ok {
		t.Fatal("changing a returned person changed the org")
	}
	if _, ok := o.Team("payments"); !ok {
		t.Fatal("changing a returned team changed the org")
	}
	if _, ok := o.Repo("nope"); ok {
		t.Fatal("found a repo that doesn't exist")
	}
	if _, ok := o.Person("nope"); ok {
		t.Fatal("found a person who doesn't exist")
	}
	if _, ok := o.Team("nope"); ok {
		t.Fatal("found a team that doesn't exist")
	}
}

func TestSlugify(t *testing.T) {
	tests := map[string]string{
		"Reliability":        "reliability",
		"Legacy Ops":         "legacy-ops",
		"  Data & ML team! ": "data-ml-team",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := slugify(in); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}
