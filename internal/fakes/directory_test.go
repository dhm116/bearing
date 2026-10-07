package fakes

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"bearing.example/internal/testkit"
)

func newDirectory(t *testing.T) (*Directory, *Org, *testkit.FakeClock) {
	t.Helper()
	c := testkit.NewClock(DirectorySyncAt)
	o := NewOrg(c)
	return NewDirectory(t, o, DirectoryOptions{Token: testToken}), o, c
}

type akPagination struct {
	Next       int `json:"next"`
	Previous   int `json:"previous"`
	Count      int `json:"count"`
	Current    int `json:"current"`
	TotalPages int `json:"total_pages"`
	StartIndex int `json:"start_index"`
	EndIndex   int `json:"end_index"`
}

type akList[T any] struct {
	Pagination akPagination `json:"pagination"`
	Results    []T          `json:"results"`
}

type akUser struct {
	PK         int    `json:"pk"`
	UUID       string `json:"uuid"`
	Username   string `json:"username"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	IsActive   bool   `json:"is_active"`
	Attributes struct {
		GitHub *struct {
			NodeID string `json:"node_id"`
			Login  string `json:"login"`
		} `json:"github"`
	} `json:"attributes"`
	Groups    []string         `json:"groups"`
	GroupsObj []map[string]any `json:"groups_obj"`
}

type akPeriod struct {
	User  int     `json:"user"`
	Start *string `json:"start"`
	End   *string `json:"end"`
}

type akGroup struct {
	PK         string           `json:"pk"`
	NumPK      int              `json:"num_pk"`
	Name       string           `json:"name"`
	Parent     *string          `json:"parent"`
	ParentName *string          `json:"parent_name"`
	Users      []int            `json:"users"`
	UsersObj   []map[string]any `json:"users_obj"`
	Attributes struct {
		MembershipPeriods []akPeriod `json:"membership_periods"`
	} `json:"attributes"`
}

// listAll reads every page of an Authentik list endpoint.
func listAll[T any](t *testing.T, base, path string) []T {
	t.Helper()
	var all []T
	for page := 1; page < 20; page++ {
		resp, body := do(t, http.MethodGet, base+path+"?page_size=2&page="+strconv.Itoa(page), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
		}
		l := decode[akList[T]](t, body)
		all = append(all, l.Results...)
		if l.Pagination.Next == 0 {
			return all
		}
		if l.Pagination.Next != page+1 {
			t.Fatalf("page %d: got next %d", page, l.Pagination.Next)
		}
	}
	t.Fatal("too many pages")
	return nil
}

func TestDirectoryRejectsMissingToken(t *testing.T) {
	d, _, _ := newDirectory(t)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, d.URL+"/api/v3/core/users/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d, want 403", resp.StatusCode)
	}
}

func TestDirectoryListsUsersWithLinks(t *testing.T) {
	d, _, _ := newDirectory(t)
	users := listAll[akUser](t, d.URL, "/api/v3/core/users/")
	var names []string
	for _, u := range users {
		names = append(names, u.Username)
	}
	if want := []string{"jdoe", "rpatel", "mchen", "tbecker", "lfischer"}; !slices.Equal(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	jdoe, mchen, tbecker, lfischer := users[0], users[2], users[3], users[4]
	if jdoe.UUID != "7f3c2a9e-1b4d-4c8e-9a0f-2d6e8b1c5a74" || jdoe.Attributes.GitHub == nil ||
		jdoe.Attributes.GitHub.NodeID != "U_kgDOA1b2cw" || jdoe.Attributes.GitHub.Login != "jdoe" {
		t.Fatalf("jdoe = %+v", jdoe)
	}
	if mchen.Attributes.GitHub.Login != "meichen" {
		t.Fatalf("mchen's GitHub login = %q, want meichen", mchen.Attributes.GitHub.Login)
	}
	if tbecker.Attributes.GitHub.NodeID != "" || tbecker.Attributes.GitHub.Login != "tbecker" {
		t.Fatalf("tbecker links = %+v, want the login only", tbecker.Attributes.GitHub)
	}
	if lfischer.Attributes.GitHub != nil {
		t.Fatalf("lfischer links = %+v, want none", lfischer.Attributes.GitHub)
	}
	if !slices.Equal(jdoe.Groups, []string{"5b0e2c1d-3f4a-4b5c-8d6e-7f8091a2b3c4"}) || len(jdoe.GroupsObj) != 1 {
		t.Fatalf("jdoe groups = %v / %v", jdoe.Groups, jdoe.GroupsObj)
	}
	_, body := do(t, http.MethodGet, d.URL+"/api/v3/core/users/?include_groups=false", nil)
	if got := decode[akList[akUser]](t, body).Results[0]; got.GroupsObj != nil || got.Groups == nil {
		t.Fatalf("include_groups=false: got %+v", got)
	}
}

func TestDirectoryGroupsCarryMembershipPeriods(t *testing.T) {
	d, _, c := newDirectory(t)
	groups := listAll[akGroup](t, d.URL, "/api/v3/core/groups/")
	var names []string
	for _, g := range groups {
		names = append(names, g.Name)
	}
	if want := []string{"engineering", "payments", "platform"}; !slices.Equal(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	pay := groups[1]
	if pay.Parent == nil || *pay.Parent != groups[0].PK || *pay.ParentName != "engineering" {
		t.Fatalf("payments parent = %v", pay.Parent)
	}
	if !slices.Equal(pay.Users, []int{42, 43}) || len(pay.UsersObj) != 2 {
		t.Fatalf("payments users = %v", pay.Users)
	}
	p := pay.Attributes.MembershipPeriods
	if len(p) != 2 || p[0].User != 42 || *p[0].Start != "2026-03-01T00:00:00Z" || *p[0].End != "2026-11-01T00:00:00Z" || p[1].End != nil {
		t.Fatalf("payments periods = %+v", p)
	}

	c.Set(MembershipEndsAt)
	_, body := do(t, http.MethodGet, d.URL+"/api/v3/core/groups/"+pay.PK+"/", nil)
	if got := decode[akGroup](t, body); !slices.Equal(got.Users, []int{43}) {
		t.Fatalf("payments at %v: users %v, want [43]", MembershipEndsAt, got.Users)
	}
	_, body = do(t, http.MethodGet, d.URL+"/api/v3/core/users/42/", nil)
	if got := decode[akUser](t, body); len(got.Groups) != 0 {
		t.Fatalf("jdoe at %v: groups %v, want none", MembershipEndsAt, got.Groups)
	}
	_, body = do(t, http.MethodGet, d.URL+"/api/v3/core/groups/?include_users=false", nil)
	if got := decode[akList[akGroup]](t, body).Results[0]; got.UsersObj != nil {
		t.Fatalf("include_users=false: got users_obj %v", got.UsersObj)
	}
}

func TestDirectoryNotFound(t *testing.T) {
	d, _, _ := newDirectory(t)
	for _, path := range []string{"/api/v3/core/users/999/", "/api/v3/core/groups/nope/", "/api/v3/core/roles/"} {
		if resp, _ := do(t, http.MethodGet, d.URL+path, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: got %d, want 404", path, resp.StatusCode)
		}
	}
	_, body := do(t, http.MethodGet, d.URL+"/api/v3/core/users/?page=9", nil)
	if l := decode[akList[akUser]](t, body); len(l.Results) != 0 || l.Pagination.Previous != 8 || l.Pagination.StartIndex != 0 {
		t.Fatalf("page past the end = %+v", l)
	}
}
