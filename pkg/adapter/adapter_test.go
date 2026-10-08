package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

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

func connect(t *testing.T, a Adapter) *Client {
	t.Helper()
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, a, reqR, respW); _ = respW.Close() }()
	t.Cleanup(func() {
		_ = reqW.Close() // io.PipeWriter.Close always returns nil
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("serve: %v", err)
		}
	})
	return NewClient("pager", respR, reqW)
}

func TestDescribeFillsProtocolVersion(t *testing.T) {
	c := connect(t, pager{n: 1})
	d, err := c.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "pager" || d.ProtocolVersion != ProtocolVersion {
		t.Fatalf("unexpected describe result %+v", d)
	}
}

func TestSyncAllPagesUntilDone(t *testing.T) {
	c := connect(t, pager{n: 3})
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
	c := connect(t, pager{n: 1000})
	_, err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 5, func(*eventv1alpha1.Observation) error { return nil })
	if !errors.Is(err, ErrTooManyPages) {
		t.Fatalf("got %v, want ErrTooManyPages", err)
	}
}

func TestHandleNotSupported(t *testing.T) {
	c := connect(t, pager{n: 1})
	_, err := c.Handle(context.Background(), HandleParams{})
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeNotSupported {
		t.Fatalf("got %v, want not supported", err)
	}
}

func TestServeAnswersUnparseableRequests(t *testing.T) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), pager{n: 1}, reqR, respW); _ = respW.Close() }()
	go func() {
		_, _ = io.WriteString(reqW, "{not json\n")
		_ = reqW.Close()
	}()
	var resp struct {
		ID    json.RawMessage `json:"id"`
		Error *Error          `json:"error"`
	}
	if err := json.NewDecoder(respR).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.ID) != "null" || resp.Error == nil || resp.Error.Code != CodeParseError {
		t.Fatalf("got id %s, error %+v; want a parse error for id null", resp.ID, resp.Error)
	}
	_, _ = io.Copy(io.Discard, respR)
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func TestStartRunsAnAdapterProcess(t *testing.T) {
	// The child is this test binary; TestMain serves pager when it sees
	// childEnv.
	t.Setenv(childEnv, "serve")
	ctx := context.Background()
	c, err := Start(ctx, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "pager" {
		t.Fatalf("described %q, want pager", d.Name)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCloseReportsAdapterExitStatus(t *testing.T) {
	t.Setenv(childEnv, "fail")
	ctx := context.Background()
	c, err := Start(ctx, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Describe(ctx); err != nil {
		t.Fatal(err)
	}
	err = c.Close()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("got %v, want an *exec.ExitError with code 1", err)
	}
}

func TestStartReportsMissingExecutables(t *testing.T) {
	_, err := Start(context.Background(), "/nonexistent/bearing-adapter")
	if err == nil || !strings.Contains(err.Error(), "adapter: start") {
		t.Fatalf("got %v, want a start error", err)
	}
}

func TestUnknownMethod(t *testing.T) {
	c := connect(t, pager{n: 1})
	var out any
	err := c.call(context.Background(), "bearing.nope", nil, &out)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("got %v, want method not found", err)
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
	c := connect(t, mixed{})
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

func TestObservationsJSONRoundTrip(t *testing.T) {
	in := Observations{model.NewObservation("adapter/x", time.Unix(1, 2000), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(model.KindTeam), Key: "x:team/a"},
	})}
	b, err := json.Marshal(SyncResult{Observations: in, Done: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"time":"1970-01-01T00:00:01.000002Z"`) {
		t.Fatalf("got %s, want ProtoJSON times", b)
	}
	var out SyncResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Observations) != 1 || !proto.Equal(out.Observations[0], in[0]) {
		t.Fatalf("got %v, want %v", out.Observations, in)
	}
	if err := json.Unmarshal([]byte(`{"observations":{}}`), &out); err == nil {
		t.Error("decoding a non-array: got no error, want one")
	}
	// A sync page keeps going past an observation it can't decode; a
	// delivery's result doesn't.
	if err := json.Unmarshal([]byte(`{"observations":[{"nope":1}]}`), &out); err != nil || len(out.Observations) != 0 || len(out.Undecodable) != 1 {
		t.Errorf("sync page: got %v, %+v; want it decoded with one undecodable", err, out)
	}
	var handled HandleResult
	if err := json.Unmarshal([]byte(`{"observations":[{"nope":1}]}`), &handled); err == nil {
		t.Error("delivery: got no error for an undecodable observation, want one")
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

func TestClientHandleValidatesAndTruncates(t *testing.T) {
	c := connect(t, coreFields{})
	_, err := c.Handle(context.Background(), HandleParams{Body: []byte("bad")})
	var ve *model.ValidationError
	if !errors.As(err, &ve) || !ve.Has(modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED) {
		t.Fatalf("got %v, want a malformed-key rejection", err)
	}
	res, err := c.Handle(context.Background(), HandleParams{Body: []byte("ok")})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Observations[0].GetTime().GetNanos(); got != 123_456_000 {
		t.Fatalf("got nanos %d, want 123456000", got)
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
	c := connect(t, completer{pager{n: 2}})
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

// TestSyncAllSkipsUndecodableObservationsOnTheWire sends a page over the real
// client with an unknown field on the middle observation: the page and the
// sync survive, and the summary counts it.
func TestSyncAllSkipsUndecodableObservationsOnTheWire(t *testing.T) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	good := func(key string) string {
		return `{"specversion":"1.0","id":"` + key + `","source":"adapter/raw","type":"dev.bearing.observation.v1","time":"1970-01-01T00:00:00Z",` +
			`"data":{"entity":{"kind":"Team","key":"` + key + `"}}}`
	}
	go func() {
		defer func() { _ = respW.Close() }()
		sc := bufio.NewScanner(reqR)
		for sc.Scan() {
			var req struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				return
			}
			page := `{"observations":[` + good("test:team/a") + `,{"specversion":"1.0","traceparent":"` + strings.Repeat("x", 5000) + `"},` +
				good("test:team/b") + `],"done":true}`
			_, _ = io.WriteString(respW, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":`+page+"}\n")
		}
	}()
	t.Cleanup(func() { _ = reqW.Close() })
	c := NewClient("raw", respR, reqW)
	var keys []string
	sum, err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 10, func(o *eventv1alpha1.Observation) error {
		keys = append(keys, o.GetData().GetEntity().GetKey())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []string{"test:team/a", "test:team/b"}) {
		t.Fatalf("emitted %v, want the two decodable observations", keys)
	}
	if want := (SyncSummary{Pages: 1, Accepted: 2, RejectedObservations: 1}); sum != want || sum.Complete() {
		t.Fatalf("got %+v (complete %v), want %+v and not complete", sum, sum.Complete(), want)
	}
	// The decode error carries the adapter's text, so it must be cut.
	var res SyncResult
	if err := json.Unmarshal([]byte(`{"observations":[{"traceparent":"`+strings.Repeat("x", 5000)+`"}]}`), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Undecodable) != 1 || len(res.Undecodable[0]) > 2*model.MaxQuoted {
		t.Fatalf("got %d undecodable, first %d bytes; want one cut to about %d", len(res.Undecodable), len(res.Undecodable[0]), model.MaxQuoted)
	}
}

func TestSyncSummaryCompleteIgnoresRejectedClaims(t *testing.T) {
	c := connect(t, mixed{})
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

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestServeReportsWriteFailures(t *testing.T) {
	broken := errors.New("pipe closed")
	req := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"` + MethodDescribe + `"}` + "\n")
	err := Serve(context.Background(), pager{n: 1}, req, failingWriter{err: broken})
	if !errors.Is(err, broken) || !strings.HasPrefix(err.Error(), "adapter: write response: ") {
		t.Fatalf("got %v, want an adapter: write response error wrapping %v", err, broken)
	}
}
