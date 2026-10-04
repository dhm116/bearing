package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
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
	err := SyncAll(context.Background(), c, json.RawMessage(`{"Prefix":"t"}`), 10, func(o *eventv1alpha1.Observation) error {
		keys = append(keys, model.Key(o.GetData().GetEntity().GetKey()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
	err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 5, func(*eventv1alpha1.Observation) error { return nil })
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
	if err == nil || !strings.Contains(err.Error(), "start adapter") {
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

// badKeys emits one observation whose key doesn't parse.
type badKeys struct{ pager }

func (badKeys) Sync(context.Context, SyncParams) (SyncResult, error) {
	o := model.NewObservation("adapter/bad", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(model.KindTeam), Key: "no key"},
	})
	return SyncResult{Observations: Observations{o}, Done: true}, nil
}

func TestSyncAllRejectsInvalidObservations(t *testing.T) {
	c := connect(t, badKeys{})
	emitted := 0
	err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 10, func(*eventv1alpha1.Observation) error {
		emitted++
		return nil
	})
	var ve *model.ValidationError
	if !errors.As(err, &ve) || !ve.Has(modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED) || emitted != 0 {
		t.Fatalf("got %v after %d emitted, want a malformed-key rejection before any emit", err, emitted)
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
	for _, bad := range []string{`{"observations":{}}`, `{"observations":[{"nope":1}]}`} {
		if err := json.Unmarshal([]byte(bad), &out); err == nil {
			t.Errorf("decoding %s: got no error, want one", bad)
		}
	}
}
