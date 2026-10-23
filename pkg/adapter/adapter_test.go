package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// pager emits one Team observation per page for n pages.
type pager struct{ n int }

func (p pager) Describe(context.Context) (DescribeResult, error) {
	return DescribeResult{Name: "pager", Version: "test", Emits: []model.Kind{model.KindTeam}, Access: []string{"none"}}, nil
}

func (p pager) Sync(_ context.Context, sp SyncParams) (SyncResult, error) {
	var cfg struct{ Prefix string }
	if err := json.Unmarshal(sp.Config, &cfg); err != nil {
		return SyncResult{}, err
	}
	page := 0
	if sp.Cursor != "" {
		page, _ = strconv.Atoi(sp.Cursor)
	}
	o := model.NewObservation("adapter/pager", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(model.KindTeam), Key: string(model.NewKey("test", "team", cfg.Prefix+strconv.Itoa(page)))},
	})
	next := page + 1
	return SyncResult{Observations: Observations{o}, NextCursor: strconv.Itoa(next), Done: next == p.n}, nil
}

func (p pager) Handle(context.Context, HandleParams) (HandleResult, error) {
	return HandleResult{}, ErrNotSupported
}

// dialed serves a over the adapter service for the test and returns a
// Remote for it.
func dialed(t *testing.T, a Adapter) *Remote {
	t.Helper()
	r, err := Dial("pager", DialConfig{Address: serveGRPC(t, a)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSyncAllPagesUntilDone(t *testing.T) {
	c := dialed(t, pager{n: 3})
	var keys []model.Key
	sum, err := SyncAll(context.Background(), c, json.RawMessage(`{"Prefix":"t"}`), 10, func(o *eventv1alpha1.Observation) error {
		keys = append(keys, model.Key(o.GetData().GetEntity().GetKey()))
		return nil
	})
	if err != nil || sum != (SyncSummary{Pages: 3, Accepted: 3}) {
		t.Fatalf("got %v and %+v, want 3 pages and 3 accepted", err, sum)
	}
	want := []model.Key{"test:team/t0", "test:team/t1", "test:team/t2"}
	if len(keys) != len(want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	}
}

func TestSyncAllStopsRunawayAdapters(t *testing.T) {
	c := dialed(t, pager{n: 1000})
	_, err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 5, func(*eventv1alpha1.Observation) error { return nil })
	if !errors.Is(err, ErrTooManyPages) {
		t.Fatalf("got %v, want ErrTooManyPages", err)
	}
}

// mixed emits two pages. The first holds a good observation, one whose key
// doesn't parse (rejected whole), one with a relation of an unregistered type
// (rejected claim only) and one with a bearingsource only the core may set;
// the second holds a good observation, to show the sync goes on.
type mixed struct{ pager }

func (mixed) Sync(_ context.Context, sp SyncParams) (SyncResult, error) {
	team := func(key string, rels ...*modelv1alpha1.Relation) *eventv1alpha1.Observation {
		return model.NewObservation("adapter/mixed", time.Unix(0, 0), &modelv1alpha1.ObservationData{
			Entity:    &modelv1alpha1.Entity{Kind: string(model.KindTeam), Key: key},
			Relations: rels,
		})
	}
	if sp.Cursor != "" {
		return SyncResult{Observations: Observations{team("test:team/last")}, Done: true}, nil
	}
	withSource := team("test:team/sourced")
	withSource.Bearingsource = "github-acme"
	return SyncResult{Observations: Observations{
		team("test:team/good"),
		team("no key"),
		team("test:team/partly", &modelv1alpha1.Relation{Type: "likes", End: &modelv1alpha1.Relation_To{To: "test:team/x"}}),
		withSource,
	}, NextCursor: "2"}, nil
}

func TestSyncAllSkipsAndCountsRejections(t *testing.T) {
	c := dialed(t, mixed{})
	var keys []string
	var rels []int
	sum, err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 10, func(o *eventv1alpha1.Observation) error {
		keys = append(keys, o.GetData().GetEntity().GetKey())
		rels = append(rels, len(o.GetData().GetRelations()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The key that doesn't parse and the bearingsource reject their
	// observations; the unregistered relation only its claim.
	if want := []string{"test:team/good", "test:team/partly", "test:team/last"}; !slices.Equal(keys, want) {
		t.Fatalf("emitted %v, want %v", keys, want)
	}
	if !slices.Equal(rels, []int{0, 0, 0}) {
		t.Fatalf("got %v relations per observation, want the rejected claim removed", rels)
	}
	if want := (SyncSummary{Pages: 2, Accepted: 3, RejectedObservations: 2, RejectedClaims: 1}); sum != want {
		t.Fatalf("got %+v, want %+v", sum, want)
	}
}

// completer reports complete_sync on its last page.
type completer struct{ pager }

func (c completer) Sync(ctx context.Context, sp SyncParams) (SyncResult, error) {
	res, err := c.pager.Sync(ctx, sp)
	if res.Done {
		res.CompleteSync = &modelv1alpha1.CompleteSync{Kinds: []string{string(model.KindTeam)}}
	}
	return res, err
}

func TestSyncResultCarriesCompleteSyncOnTheLastPage(t *testing.T) {
	c := dialed(t, completer{pager{n: 2}})
	cfg := json.RawMessage(`{"prefix":"p"}`)
	first, err := c.Sync(context.Background(), SyncParams{Config: cfg})
	if err != nil || first.Done || first.CompleteSync != nil {
		t.Fatalf("first page: %+v, %v; want not done and no complete_sync", first, err)
	}
	last, err := c.Sync(context.Background(), SyncParams{Config: cfg, Cursor: first.NextCursor})
	if err != nil || !last.Done || last.CompleteSync.GetKinds()[0] != string(model.KindTeam) {
		t.Fatalf("last page: %+v, %v; want done with complete_sync [Team]", last, err)
	}
}

func TestSyncSummaryCompleteIgnoresRejectedClaims(t *testing.T) {
	c := dialed(t, mixed{})
	sum, err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 10, func(*eventv1alpha1.Observation) error { return nil })
	if err != nil || sum.Complete() {
		t.Fatalf("got %+v (%v), want a sync that skipped observations to be incomplete", sum, err)
	}
	if !(SyncSummary{Pages: 1, Accepted: 3, RejectedClaims: 2}).Complete() {
		t.Fatal("got incomplete for a sync that only lost claims, want complete: every entity was seen")
	}
}

// scripted is a Syncer that returns one canned result.
type scripted struct {
	res SyncResult
	err error
}

func (s scripted) Sync(context.Context, SyncParams) (SyncResult, error) { return s.res, s.err }

func TestSyncAllErrorsNameThePackageAndPage(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		s    scripted
		want string
		is   error
	}{
		{"sync fails", scripted{err: boom}, "adapter: sync page 1: boom", boom},
		{"not done and no cursor", scripted{res: SyncResult{}}, "adapter: sync page 1: adapter is not done but returned no new cursor", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SyncAll(context.Background(), tt.s, json.RawMessage(`{}`), 5, func(*eventv1alpha1.Observation) error { return nil })
			if err == nil || err.Error() != tt.want {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("got %v, want it to wrap %v", err, tt.is)
			}
		})
	}
}

func TestWebhookSignatureDeclaration(t *testing.T) {
	w, err := WebhookSignature{Scheme: "WEBHOOK_SCHEME_HMAC_SHA256", SignatureHeader: "X-Sig", SignaturePrefix: "v1="}.Declaration()
	if err != nil {
		t.Fatal(err)
	}
	if w.GetScheme() != modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_HMAC_SHA256 || w.GetSignatureHeader() != "X-Sig" || w.GetSignaturePrefix() != "v1=" {
		t.Errorf("got %v", w)
	}
	for _, scheme := range []string{"", "WEBHOOK_SCHEME_UNSPECIFIED", "rot13"} {
		if _, err := (WebhookSignature{Scheme: scheme, SignatureHeader: "X-Sig"}).Declaration(); err == nil {
			t.Errorf("scheme %q: want an error", scheme)
		}
	}
}

func TestSyncPagesKeepsAPageTogetherAndNumbersEveryOne(t *testing.T) {
	// mixed's first page has four observations, two of them rejected whole;
	// its second has one. complete_sync is dropped for the sync once any
	// observation was rejected, so the core never treats it as complete.
	type seen struct {
		number int
		keys   []string
		done   bool
	}
	var got []seen
	sum, err := SyncPages(context.Background(), completingMixed{}, json.RawMessage(`{}`), 10, func(p Page) error {
		var keys []string
		for _, o := range p.Observations {
			keys = append(keys, o.GetData().GetEntity().GetKey())
		}
		got = append(got, seen{p.Number, keys, p.Done})
		if p.Done && p.CompleteSync != nil {
			t.Error("a sync that rejected an observation still reports complete_sync")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []seen{
		{1, []string{"test:team/good", "test:team/partly"}, false},
		{2, []string{"test:team/last"}, true},
	}
	if !slices.EqualFunc(got, want, func(a, b seen) bool {
		return a.number == b.number && a.done == b.done && slices.Equal(a.keys, b.keys)
	}) {
		t.Fatalf("got pages %+v, want %+v", got, want)
	}
	if sum.Pages != 2 || sum.Accepted != 3 || sum.Complete() {
		t.Errorf("got %+v", sum)
	}
}

// completingMixed is mixed with a complete_sync on its last page.
type completingMixed struct{ mixed }

func (c completingMixed) Sync(ctx context.Context, sp SyncParams) (SyncResult, error) {
	res, err := c.mixed.Sync(ctx, sp)
	if res.Done {
		res.CompleteSync = &modelv1alpha1.CompleteSync{Kinds: []string{string(model.KindTeam)}}
	}
	return res, err
}

func TestSyncPagesKeepsCompleteSyncWhenNothingWasRejected(t *testing.T) {
	var last Page
	_, err := SyncPages(context.Background(), completer{pager{n: 2}}, json.RawMessage(`{"prefix":"p"}`), 10, func(p Page) error {
		last = p
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !last.Done || last.Number != 2 || last.CompleteSync == nil {
		t.Fatalf("got last page %+v, want page 2, done, with complete_sync", last)
	}
}

func TestSyncPagesStopsWhenEmitFails(t *testing.T) {
	boom := errors.New("log is full")
	calls := 0
	_, err := SyncPages(context.Background(), pager{n: 5}, json.RawMessage(`{}`), 10, func(Page) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("got %v after %d pages, want the emit error after the first", err, calls)
	}
}

func TestHandleCheckedValidatesAndTruncates(t *testing.T) {
	a := coreFields{}
	_, err := HandleChecked(context.Background(), a, "x", HandleParams{Body: []byte("bad")})
	var ve *model.ValidationError
	if !errors.As(err, &ve) || !ve.Has(modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED) {
		t.Fatalf("got %v, want a malformed-key rejection", err)
	}
	res, err := HandleChecked(context.Background(), a, "x", HandleParams{Body: []byte("ok")})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Observations[0].GetTime().GetNanos(); got != 123_456_000 {
		t.Fatalf("got nanos %d, want 123456000", got)
	}
	if _, err := HandleChecked(context.Background(), pager{}, "x", HandleParams{}); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("got %v, want ErrNotSupported passed through", err)
	}
}

// coreFields: Handle returns an observation with a bad key unless the body
// is "ok", and nanosecond times either way.
type coreFields struct{ pager }

func (coreFields) Handle(_ context.Context, p HandleParams) (HandleResult, error) {
	key := "no key"
	if string(p.Body) == "ok" {
		key = "x:team/a"
	}
	o := model.NewObservation("adapter/x", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(model.KindTeam), Key: key},
	})
	o.Time.Nanos = 123_456_789
	return HandleResult{Observations: Observations{o}}, nil
}
