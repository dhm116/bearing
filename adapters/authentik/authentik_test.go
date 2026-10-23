package authentik_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"bearing.example/adapters/authentik"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
)

const canary = "ak-token-7c41e9d2b8"

func adapterFor(client *http.Client, token string) *authentik.Adapter {
	c := testkit.NewClock(fakes.DirectorySyncAt)
	return &authentik.Adapter{HTTP: client, Now: c.Now, Getenv: func(k string) string {
		if k == "AUTHENTIK_TOKEN" {
			return token
		}
		return ""
	}}
}

// syncAll pages a sync to its end and returns the observations and the
// number of Sync calls; only the last may be Done.
func syncAll(t *testing.T, a *authentik.Adapter, cfg authentik.Config) ([]*eventv1alpha1.Observation, int) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var all []*eventv1alpha1.Observation
	cur := ""
	for calls := 1; calls < 100; calls++ {
		res, err := a.Sync(t.Context(), adapter.SyncParams{Config: raw, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if res.Done {
			if res.NextCursor != "" || res.CompleteSync == nil {
				t.Fatalf("last page: cursor %q, complete_sync %v", res.NextCursor, res.CompleteSync)
			}
			return all, calls
		}
		if res.NextCursor == "" {
			t.Fatal("page without a next cursor that is not Done")
		}
		cur = res.NextCursor
	}
	t.Fatal("sync did not finish")
	return nil, 0
}

func TestSyncPagesGiveTheSameObservationsAsOneBigPage(t *testing.T) {
	d := fakes.NewDirectory(t, fakes.NewOrg(testkit.NewClock(fakes.DirectorySyncAt)), fakes.DirectoryOptions{Token: canary})
	a := adapterFor(d.Client(), canary)
	small, smallCalls := syncAll(t, a, authentik.Config{APIURL: d.URL, PageSize: 2})
	big, bigCalls := syncAll(t, a, authentik.Config{APIURL: d.URL, PageSize: 100})
	if len(small) == 0 || len(small) != len(big) {
		t.Fatalf("got %d and %d observations", len(small), len(big))
	}
	for i := range small {
		if !proto.Equal(small[i], big[i]) {
			t.Fatalf("observation %d differs:\n got %v\nwant %v", i, small[i], big[i])
		}
	}
	if smallCalls <= bigCalls {
		t.Fatalf("a page size of 2 took %d Sync calls, one of 100 took %d; want more", smallCalls, bigCalls)
	}
}

func TestSyncRefusesBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  string
		want string
	}{
		{"missing api_url", `{}`, "api_url"},
		{"plain http to a remote host", `{"api_url":"http://auth.acme.example"}`, "api_url"},
		{"api_url with userinfo", `{"api_url":"https://u:p@auth.acme.example"}`, "api_url"},
		{"ui_url is not http(s)", `{"api_url":"https://auth.acme.example","ui_url":"javascript:alert(1)"}`, "ui_url"},
		{"token_env names a Bearing variable", `{"api_url":"https://auth.acme.example","token_env":"BEARING_STORE_PASSWORD"}`, "token_env"},
		{"bad source slug", `{"api_url":"https://auth.acme.example","github_source_slug":"a&b"}`, "github_source_slug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adapterFor(http.DefaultClient, canary).Sync(t.Context(), adapter.SyncParams{Config: json.RawMessage(tt.cfg)})
			var ae *adapter.Error
			if !errors.As(err, &ae) || ae.Code != adapter.CodeInvalidParams || !strings.Contains(ae.Message, tt.want) {
				t.Fatalf("got %v, want an invalid-params error naming %s", err, tt.want)
			}
		})
	}
}

func TestSyncReportsABadTokenWithoutLeakingIt(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"Token invalid/expired"}`))
		}))
		t.Cleanup(srv.Close)
		raw, err := json.Marshal(authentik.Config{APIURL: srv.URL, TokenEnv: "AUTHENTIK_TOKEN"}) //nolint:gosec // G101: a variable name
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapterFor(srv.Client(), canary).Sync(context.Background(), adapter.SyncParams{Config: raw})
		var ae *adapter.Error
		if !errors.As(err, &ae) || ae.Code != adapter.CodeUpstream || !strings.Contains(ae.Message, "AUTHENTIK_TOKEN") {
			t.Fatalf("status %d: got %v, want an upstream error naming AUTHENTIK_TOKEN", status, err)
		}
		testkit.AssertNoLeaks(t, err.Error(), canary)
	}
}

func TestSyncLeaksNoTokenInObservations(t *testing.T) {
	d := fakes.NewDirectory(t, fakes.NewOrg(testkit.NewClock(fakes.DirectorySyncAt)), fakes.DirectoryOptions{Token: canary})
	obs, _ := syncAll(t, adapterFor(d.Client(), canary), authentik.Config{APIURL: d.URL})
	var sb strings.Builder
	for _, o := range obs {
		sb.WriteString(o.String())
	}
	testkit.AssertNoLeaks(t, sb.String(), canary)
}

func TestSyncRefusesABadCursor(t *testing.T) {
	_, err := adapterFor(http.DefaultClient, canary).Sync(t.Context(),
		adapter.SyncParams{Config: json.RawMessage(`{"api_url":"https://auth.acme.example"}`), Cursor: `{"phase":"nope","page":1}`})
	var ae *adapter.Error
	if !errors.As(err, &ae) || ae.Code != adapter.CodeInvalidParams {
		t.Fatalf("got %v, want an invalid-params error", err)
	}
}
