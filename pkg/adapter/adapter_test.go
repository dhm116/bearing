package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"

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
	o := model.NewObservation("adapter/pager", time.Unix(0, 0), model.ObservationData{
		Entity: model.Entity{Kind: model.KindTeam, Key: model.NewKey("test", "team", cfg.Prefix+strconv.Itoa(page))},
	})
	next := page + 1
	return SyncResult{Observations: []model.Observation{o}, NextCursor: strconv.Itoa(next), Done: next == p.n}, nil
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
	go func() { done <- Serve(ctx, a, reqR, respW); respW.Close() }()
	t.Cleanup(func() {
		reqW.Close()
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
	err := SyncAll(context.Background(), c, json.RawMessage(`{"Prefix":"t"}`), 10, func(o model.Observation) error {
		keys = append(keys, o.Data.Entity.Key)
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
	err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 5, func(model.Observation) error { return nil })
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

func TestUnknownMethod(t *testing.T) {
	c := connect(t, pager{n: 1})
	var out any
	err := c.call(context.Background(), "bearing.nope", nil, &out)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("got %v, want method not found", err)
	}
}
