package authentik_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"bearing.example/adapters/authentik"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

func personKeys(obs []*eventv1alpha1.Observation) []string {
	var keys []string
	for _, o := range obs {
		if e := o.GetData().GetEntity(); e.GetKind() == string(model.KindPerson) {
			keys = append(keys, e.GetKey())
		}
	}
	return keys
}

func TestSyncOrdersEveryListByPK(t *testing.T) {
	s, srv := newStub(t)
	s.users = append(s.users, userJSON(1))
	syncTo(t, adapterFor(srv.Client(), canary), authentik.Config{APIURL: srv.URL})
	if len(s.queries) == 0 {
		t.Fatal("no requests")
	}
	for _, q := range s.queries {
		if q["ordering"][0] != "pk" {
			t.Fatalf("got ordering %v, want pk", q["ordering"])
		}
	}
}

func TestSyncLosesNoUserWhenOneIsDeletedBetweenPages(t *testing.T) {
	s, srv := newStub(t)
	for pk := 1; pk <= 5; pk++ {
		s.users = append(s.users, userJSON(pk))
	}
	deleted := false
	s.before = func(s *stub, path string, page int) {
		if path == "/api/v3/core/users/" && page == 2 && !deleted {
			deleted = true
			s.users = s.users[1:] // user 1 goes: offsets shift, user 3 would be skipped
		}
	}
	obs, res := syncTo(t, adapterFor(srv.Client(), canary), authentik.Config{APIURL: srv.URL, PageSize: 2})
	keys := personKeys(obs)
	for pk := 2; pk <= 5; pk++ {
		if !slices.Contains(keys, "authentik:user/"+userJSON(pk)["uuid"].(string)) {
			t.Errorf("user %d was lost; got %v", pk, keys)
		}
	}
	if want := []string{string(model.KindPerson), string(model.KindTeam)}; res.CompleteSync == nil || !slices.Equal(res.CompleteSync.Kinds, want) {
		t.Fatalf("got complete_sync %v, want kinds %v", res.CompleteSync, want)
	}
}

func TestSyncOmitsCompleteSyncForAListThatKeepsChanging(t *testing.T) {
	s, srv := newStub(t)
	for pk := 1; pk <= 3; pk++ {
		s.users = append(s.users, userJSON(pk))
	}
	next := 4
	s.before = func(s *stub, path string, page int) {
		if path == "/api/v3/core/users/" && page == 2 {
			s.users = append(s.users, userJSON(next)) // a user joins before every second page
			next++
		}
	}
	_, res := syncTo(t, adapterFor(srv.Client(), canary), authentik.Config{APIURL: srv.URL, PageSize: 2})
	if !res.Done || res.CompleteSync != nil {
		t.Fatalf("got done %v, complete_sync %v, want done without complete_sync", res.Done, res.CompleteSync)
	}
}

func TestSyncCapsPageSize(t *testing.T) {
	s, srv := newStub(t)
	syncTo(t, adapterFor(srv.Client(), canary), authentik.Config{APIURL: srv.URL, PageSize: 100000})
	for _, q := range s.queries {
		if q["page_size"][0] != "500" {
			t.Fatalf("got page_size %v, want 500", q["page_size"])
		}
	}
}

func TestSyncRefusesACursorOverTheLimit(t *testing.T) {
	s, srv := newStub(t)
	s.conns = []map[string]any{{"user": 1, "identifier": "42"}}
	authentik.SetMaxCursorBytes(t, 20)
	raw, err := json.Marshal(authentik.Config{APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	a := adapterFor(srv.Client(), canary)
	big := `{"phase":"users","page":1,"users":{"1":"` + strings.Repeat("x", 50) + `"}}`
	for name, cursor := range map[string]string{"incoming": big, "produced": ""} {
		_, err = a.Sync(t.Context(), adapter.SyncParams{Config: raw, Cursor: cursor})
		var ae *adapter.Error
		if !errors.As(err, &ae) || ae.Code != adapter.CodeUpstream || !strings.Contains(ae.Message, "shortcut") {
			t.Fatalf("%s cursor: got %v, want an upstream error naming the shortcut", name, err)
		}
	}
}

func TestSyncSkipsAUserWhoseUUIDDoesNotParse(t *testing.T) {
	installCapture()
	s, srv := newStub(t)
	bad := userJSON(2)
	bad["uuid"] = "not-a-uuid-" + canary
	s.users = []map[string]any{userJSON(1), bad}
	obs, _ := syncTo(t, adapterFor(srv.Client(), canary), authentik.Config{APIURL: srv.URL})
	if keys := personKeys(obs); len(keys) != 1 || !strings.HasSuffix(keys[0], userJSON(1)["uuid"].(string)) {
		t.Fatalf("got %v, want only user 1", keys)
	}
	if !strings.Contains(logs.all(), "uuid does not parse pk=2") {
		t.Fatalf("no warning for the skipped user in %q", logs.all())
	}
}

func TestSyncIgnoresANonNumericOAuthIdentifier(t *testing.T) {
	s, srv := newStub(t)
	s.users = []map[string]any{userJSON(1)}
	s.conns = []map[string]any{{"user": 1, "identifier": "octocat"}}
	obs, _ := syncTo(t, adapterFor(srv.Client(), canary), authentik.Config{APIURL: srv.URL})
	if len(obs) != 1 {
		t.Fatalf("got %d observations, want 1", len(obs))
	}
	e := obs[0].GetData().GetEntity()
	if len(e.GetLinkedIds()) != 0 || !e.GetLinkedIdsComplete() {
		t.Fatalf("got linked_ids %v (complete %v), want none", e.GetLinkedIds(), e.GetLinkedIdsComplete())
	}
}

func TestSyncReportsUpstreamFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"server error", 500, `{"detail":"boom"}`, "500 boom"},
		{"server error without JSON", 500, `<html>`, "500"},
		{"undecodable page", 200, `{"results":`, "decode response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptestServer(t, tt.status, tt.body)
			raw, err := json.Marshal(authentik.Config{APIURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapterFor(srv.Client(), canary).Sync(t.Context(), adapter.SyncParams{Config: raw})
			var ae *adapter.Error
			if !errors.As(err, &ae) || ae.Code != adapter.CodeUpstream || !strings.Contains(ae.Message, tt.want) {
				t.Fatalf("got %v, want an upstream error containing %q", err, tt.want)
			}
		})
	}
}

func TestDescribeNamesTheTokenPermissions(t *testing.T) {
	d, err := authentik.New().Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"authentik_core.view_user", "authentik_core.view_group", "authentik_sources_oauth.view_useroauthsourceconnection"}
	if !slices.Equal(d.Access, want) {
		t.Fatalf("got access %v, want %v", d.Access, want)
	}
}

func TestSyncLogsHoldNoToken(t *testing.T) {
	installCapture()
	d := fakes.NewDirectory(t, fakes.NewOrg(testkit.NewClock(fakes.DirectorySyncAt)), fakes.DirectoryOptions{Token: canary})
	syncAll(t, adapterFor(d.Client(), canary), authentik.Config{APIURL: d.URL})
	testkit.AssertNoLeaks(t, logs.all(), canary)
}
