package authentik_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"bearing.example/adapters/authentik"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/adapter"
)

// stub serves hand-made Authentik lists, ordered as given, so a test can
// change them between requests.
type stub struct {
	mu                   sync.Mutex
	conns, users, groups []map[string]any
	// before runs under the lock ahead of each list reply.
	before func(s *stub, path string, page int)
	// queries holds the query of every request.
	queries []map[string][]string
}

func newStub(t *testing.T) (*stub, *httptest.Server) {
	t.Helper()
	s := &stub{}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func userJSON(pk int) map[string]any {
	return map[string]any{
		"pk": pk, "uuid": fmt.Sprintf("00000000-0000-0000-0000-%012d", pk),
		"username": "user" + strconv.Itoa(pk), "name": "User " + strconv.Itoa(pk),
		"email": fmt.Sprintf("user%d@acme.example", pk), "attributes": map[string]any{},
	}
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	s.queries = append(s.queries, q)
	size, _ := strconv.Atoi(q.Get("page_size"))
	pg, _ := strconv.Atoi(q.Get("page"))
	if size < 1 || pg < 1 {
		http.Error(w, "bad paging", http.StatusBadRequest)
		return
	}
	if s.before != nil {
		s.before(s, r.URL.Path, pg)
	}
	var items []map[string]any
	switch r.URL.Path {
	case "/api/v3/sources/user_connections/oauth/":
		items = s.conns
	case "/api/v3/core/users/":
		items = s.users
	case "/api/v3/core/groups/":
		items = s.groups
	default:
		http.NotFound(w, r)
		return
	}
	lo, hi := min((pg-1)*size, len(items)), min(pg*size, len(items))
	next := 0
	if hi < len(items) {
		next = pg + 1
	}
	res := items[lo:hi]
	if res == nil {
		res = []map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pagination": map[string]any{"next": next, "count": len(items), "current": pg},
		"results":    res,
	})
}

// syncTo pages a sync to its end and returns the observations and the last
// result; unlike syncAll it takes a sync without complete_sync.
func syncTo(t *testing.T, a *authentik.Adapter, cfg authentik.Config) ([]*eventv1alpha1.Observation, adapter.SyncResult) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var all []*eventv1alpha1.Observation
	cur := ""
	for range 200 {
		res, err := a.Sync(t.Context(), adapter.SyncParams{Config: raw, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if res.Done {
			return all, res
		}
		cur = res.NextCursor
	}
	t.Fatal("sync did not finish")
	return nil, adapter.SyncResult{}
}

// httptestServer answers every request with status and body.
func httptestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}
