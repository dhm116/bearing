package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"bearing.example/pkg/contracts"
)

// EventLog runs the EventLog conformance suite. newLog must return an empty
// log each time it is called, together with the clock that stamps the
// entries it appends (Entry.AppendedAt, which Trim goes by); the suite
// drives it. A backend that keeps events past the process is checked for
// that by its own tests, since a suite can't restart it.
func EventLog(t *testing.T, newLog func(t *testing.T) (contracts.EventLog, Clock)) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	// A time with nanoseconds, which the log truncates to microseconds.
	eventTime := time.Date(2026, 10, 10, 11, 59, 58, 123456789, time.UTC)

	fresh := func(t *testing.T) (contracts.EventLog, Clock) {
		t.Helper()
		l, clk := newLog(t)
		clk.Set(t0)
		return l, clk
	}
	event := func(partition, id string) contracts.Event {
		return contracts.Event{
			ID: partition + "/" + id, Partition: contracts.Partition(partition),
			Type: "dev.bearing.webhook_received.v1", Time: eventTime, Data: []byte(`{"id":"` + id + `"}`),
			Retain: partition == "manual", // required there
		}
	}
	appendOne := func(t *testing.T, l contracts.EventLog, e contracts.Event) contracts.Appended {
		t.Helper()
		got, err := l.Append(ctx, []contracts.Event{e})
		if err != nil || len(got) != 1 {
			t.Fatalf("Append %s: %v, %v", e.ID, got, err)
		}
		return got[0]
	}
	read := func(t *testing.T, l contracts.EventLog, p string, after contracts.Offset, limit int) []contracts.Entry {
		t.Helper()
		got, err := l.Read(ctx, contracts.Partition(p), after, limit)
		if err != nil {
			t.Fatalf("Read %s after %d: %v", p, after, err)
		}
		return got
	}
	// trim has the "applier" group process every partition, then trims by
	// age with it as the required group.
	trim := func(t *testing.T, l contracts.EventLog, before time.Time) (int, error) {
		t.Helper()
		ps, err := l.Partitions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if err := l.Commit(ctx, "applier", p.Partition, p.Head); err != nil {
				t.Fatal(err)
			}
		}
		return l.Trim(ctx, before, []string{"applier"})
	}
	ids := func(es []contracts.Entry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}
	offsets := func(es []contracts.Entry) []contracts.Offset {
		var out []contracts.Offset
		for _, e := range es {
			out = append(out, e.Offset)
		}
		return out
	}

	t.Run("Append gives consecutive offsets from 1 and Read returns the events as stored", func(t *testing.T) {
		l, clk := fresh(t)
		clk.Set(t0.Add(1500 * time.Nanosecond))
		in := []contracts.Event{event("github-acme", "d1"), event("github-acme", "d2"), event("github-acme", "d3")}
		in[1].Retain = true
		got, err := l.Append(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		var prev contracts.Offset
		for i, a := range got {
			if a.Partition != "github-acme" || a.Offset <= prev || a.Duplicate {
				t.Fatalf("Append result %d = %+v, want github-acme, an offset above %d, not a duplicate", i, a, prev)
			}
			prev = a.Offset
		}
		es := read(t, l, "github-acme", 0, 10)
		if len(es) != 3 {
			t.Fatalf("Read returned %d entries, want 3", len(es))
		}
		for i, e := range es {
			want := in[i]
			wantAt := t0.Add(time.Microsecond)
			switch {
			case e.Offset != got[i].Offset, e.ID != want.ID, e.Partition != want.Partition, e.Type != want.Type,
				!bytes.Equal(e.Data, want.Data), e.Retain != want.Retain:
				t.Fatalf("entry %d = %+v, want %+v at offset %d", i, e, want, got[i].Offset)
			case !e.Time.Equal(eventTime.Truncate(time.Microsecond)) || e.Time.Location() != time.UTC:
				t.Fatalf("entry %d time = %v, want %v in UTC (microseconds)", i, e.Time, eventTime.Truncate(time.Microsecond))
			case !e.AppendedAt.Equal(wantAt) || e.AppendedAt.Location() != time.UTC:
				t.Fatalf("entry %d appended at %v, want the clock's %v in UTC (microseconds)", i, e.AppendedAt, wantAt)
			}
		}
	})

	t.Run("Offsets are per partition, in the order given within a call", func(t *testing.T) {
		l, _ := fresh(t)
		got, err := l.Append(ctx, []contracts.Event{
			event("github-acme", "a"), event("manual", "m"), event("github-acme", "b"), event("core/scheduler", "tick"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Partition != "github-acme" || got[1].Partition != "manual" || got[2].Partition != "github-acme" || got[3].Partition != "core/scheduler" {
			t.Fatalf("results name the wrong partitions: %+v", got)
		}
		if got[2].Offset <= got[0].Offset {
			t.Fatalf("offsets in one partition went %d then %d, want increasing in the order given", got[0].Offset, got[2].Offset)
		}
		if g := ids(read(t, l, "github-acme", 0, 10)); !slices.Equal(g, []string{"github-acme/a", "github-acme/b"}) {
			t.Fatalf("github-acme = %v", g)
		}
		if g := ids(read(t, l, "manual", 0, 10)); !slices.Equal(g, []string{"manual/m"}) {
			t.Fatalf("manual = %v", g)
		}
		if more := appendOne(t, l, event("github-acme", "c")); more.Offset <= got[2].Offset {
			t.Fatalf("next offset = %d, want above %d", more.Offset, got[2].Offset)
		}
	})

	t.Run("A repeated ID writes nothing and returns the original position", func(t *testing.T) {
		l, _ := fresh(t)
		first := appendOne(t, l, event("github-acme", "d1"))
		second := appendOne(t, l, event("github-acme", "d2"))
		changed := event("github-acme", "d1")
		changed.Data = []byte(`{"different":true}`)
		again := appendOne(t, l, changed)
		if !again.Duplicate || again.Offset != first.Offset || again.Partition != first.Partition {
			t.Fatalf("got %+v, want a duplicate of %+v", again, first)
		}
		es := read(t, l, "github-acme", 0, 10)
		if g := ids(es); !slices.Equal(g, []string{"github-acme/d1", "github-acme/d2"}) || string(es[0].Data) != `{"id":"d1"}` {
			t.Fatalf("log after a duplicate: %v, first data %s; want the original kept and nothing added", g, es[0].Data)
		}
		if next := appendOne(t, l, event("github-acme", "d3")); next.Offset <= second.Offset {
			t.Fatalf("next offset = %d, want above %d", next.Offset, second.Offset)
		}
	})

	t.Run("An ID repeated within one call is a duplicate of its first use", func(t *testing.T) {
		l, _ := fresh(t)
		got, err := l.Append(ctx, []contracts.Event{event("github-acme", "x"), event("github-acme", "y"), event("github-acme", "x")})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Duplicate || got[1].Duplicate || !got[2].Duplicate || got[2].Offset != got[0].Offset || got[1].Offset <= got[0].Offset {
			t.Fatalf("got %+v", got)
		}
		if n := len(read(t, l, "github-acme", 0, 10)); n != 2 {
			t.Fatalf("%d entries, want 2", n)
		}
	})

	t.Run("Append refuses invalid events and writes none of the batch", func(t *testing.T) {
		l, _ := fresh(t)
		bad := event("github-acme", "bad")
		bad.Data = nil
		other := event("github-acme", "x")
		other.ID = "gitea-acme/x"
		slashed := event("github-acme", "a/b")
		unretained := event("manual", "m")
		unretained.Retain = false
		for name, batch := range map[string][]contracts.Event{
			"no data":                    {event("github-acme", "good"), bad},
			"ID of another partition":    {event("github-acme", "good"), other},
			"slash in the local ID":      {event("github-acme", "good"), slashed},
			"manual event not retained":  {event("github-acme", "good"), unretained},
			"no events":                  nil,
			"too many events":            make([]contracts.Event, contracts.MaxAppendEvents+1),
			"data past the append limit": oversized(),
		} {
			if _, err := l.Append(ctx, batch); !errors.Is(err, contracts.ErrInvalidEvent) {
				t.Errorf("%s: got %v, want ErrInvalidEvent", name, err)
			}
		}
		if ps, err := l.Partitions(ctx); err != nil || len(ps) != 0 {
			t.Fatalf("Partitions = %v, %v, want none: a refused batch must write nothing", ps, err)
		}
	})

	t.Run("Read honours after and limit, and an unknown partition is empty", func(t *testing.T) {
		l, _ := fresh(t)
		var at []contracts.Offset
		for _, id := range []string{"a", "b", "c", "d"} {
			at = append(at, appendOne(t, l, event("github-acme", id)).Offset)
		}
		if g := offsets(read(t, l, "github-acme", at[0], 2)); !slices.Equal(g, at[1:3]) {
			t.Fatalf("after %d limit 2 = %v, want %v", at[0], g, at[1:3])
		}
		if g := offsets(read(t, l, "github-acme", at[3], 10)); len(g) != 0 {
			t.Fatalf("after the head = %v, want none", g)
		}
		if g := read(t, l, "nobody", 0, 10); len(g) != 0 {
			t.Fatalf("unknown partition = %v, want none", g)
		}
		for _, c := range []struct {
			name  string
			p     contracts.Partition
			after contracts.Offset
			limit int
		}{
			{"zero limit", "github-acme", 0, 0},
			{"limit over the maximum", "github-acme", 0, contracts.MaxReadEntries + 1},
			{"negative offset", "github-acme", -1, 10},
			{"empty partition name", "", 0, 10},
		} {
			if _, err := l.Read(ctx, c.p, c.after, c.limit); !errors.Is(err, contracts.ErrInvalidRequest) {
				t.Errorf("%s: got %v, want ErrInvalidRequest", c.name, err)
			}
		}
	})

	t.Run("Read stops before the data limit but returns at least one entry", func(t *testing.T) {
		l, _ := fresh(t)
		const n = 5
		var last contracts.Offset
		for i := range n {
			e := event("github-acme", fmt.Sprintf("big%d", i))
			e.Data = bytes.Repeat([]byte("x"), contracts.MaxEventBytes)
			last = appendOne(t, l, e).Offset
		}
		es := read(t, l, "github-acme", 0, 10)
		if want := contracts.MaxReadBytes / contracts.MaxEventBytes; len(es) != want {
			t.Fatalf("Read returned %d entries of %d bytes, want %d (the %d byte limit)", len(es), contracts.MaxEventBytes, want, contracts.MaxReadBytes)
		}
		if es := read(t, l, "github-acme", es[len(es)-1].Offset, 10); len(es) != 1 || es[0].Offset != last {
			t.Fatalf("the rest = %v, want offset %d alone", offsets(es), last)
		}
	})

	t.Run("Commit keeps each group's offset and never moves it back", func(t *testing.T) {
		l, _ := fresh(t)
		var at []contracts.Offset
		for _, id := range []string{"a", "b", "c"} {
			at = append(at, appendOne(t, l, event("github-acme", id)).Offset)
		}
		committed := func(group string) contracts.Offset {
			t.Helper()
			o, err := l.Committed(ctx, group, "github-acme")
			if err != nil {
				t.Fatal(err)
			}
			return o
		}
		if o := committed("workers"); o != 0 {
			t.Fatalf("Committed before any commit = %d, want 0", o)
		}
		if err := l.Commit(ctx, "workers", "github-acme", at[1]); err != nil {
			t.Fatal(err)
		}
		if err := l.Commit(ctx, "audit", "github-acme", at[0]); err != nil {
			t.Fatal(err)
		}
		if err := l.Commit(ctx, "workers", "github-acme", at[0]); err != nil {
			t.Fatalf("a commit behind the group's offset is not an error: %v", err)
		}
		if g, a := committed("workers"), committed("audit"); g != at[1] || a != at[0] {
			t.Fatalf("workers at %d, audit at %d, want %d and %d", g, a, at[1], at[0])
		}
		if err := l.Commit(ctx, "workers", "github-acme", at[2]); err != nil {
			t.Fatal(err)
		}
		if o := committed("workers"); o != at[2] {
			t.Fatalf("workers at %d, want %d", o, at[2])
		}
		if o, err := l.Committed(ctx, "workers", "nobody"); err != nil || o != 0 {
			t.Fatalf("Committed in an unknown partition = %d, %v, want 0", o, err)
		}
	})

	t.Run("Commit refuses an unknown partition, an offset past the head and bad names", func(t *testing.T) {
		l, _ := fresh(t)
		head := appendOne(t, l, event("github-acme", "a")).Offset
		if err := l.Commit(ctx, "workers", "nobody", 1); !errors.Is(err, contracts.ErrNotFound) {
			t.Errorf("unknown partition: got %v, want ErrNotFound", err)
		}
		if err := l.Commit(ctx, "workers", "github-acme", head+1); !errors.Is(err, contracts.ErrInvalidRequest) {
			t.Errorf("past the head: got %v, want ErrInvalidRequest", err)
		}
		if err := l.Commit(ctx, "workers", "github-acme", -1); !errors.Is(err, contracts.ErrInvalidRequest) {
			t.Errorf("negative offset: got %v, want ErrInvalidRequest", err)
		}
		if err := l.Commit(ctx, "", "github-acme", head); !errors.Is(err, contracts.ErrInvalidRequest) {
			t.Errorf("empty group: got %v, want ErrInvalidRequest", err)
		}
		if _, err := l.Committed(ctx, "", "github-acme"); !errors.Is(err, contracts.ErrInvalidRequest) {
			t.Errorf("Committed with an empty group: got %v, want ErrInvalidRequest", err)
		}
		if o, _ := l.Committed(ctx, "workers", "github-acme"); o != 0 {
			t.Errorf("refused commits moved the group to %d", o)
		}
	})

	t.Run("Partitions lists every partition by name with its head", func(t *testing.T) {
		l, _ := fresh(t)
		m := appendOne(t, l, event("manual", "m"))
		appendOne(t, l, event("github-acme", "a"))
		b := appendOne(t, l, event("github-acme", "b"))
		c := appendOne(t, l, event("core/scheduler", "t"))
		ps, err := l.Partitions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []contracts.PartitionInfo{{Partition: "core/scheduler", Head: c.Offset}, {Partition: "github-acme", Head: b.Offset}, {Partition: "manual", Head: m.Offset}}
		if !slices.Equal(ps, want) {
			t.Fatalf("Partitions = %+v, want %+v", ps, want)
		}
	})

	t.Run("Trim removes old unretained entries, reports them in Trimmed and never reuses an offset", func(t *testing.T) {
		l, clk := fresh(t)
		appendOne(t, l, event("github-acme", "old1"))
		keep := event("github-acme", "old2-kept")
		keep.Retain = true
		appendOne(t, l, keep)
		old3 := appendOne(t, l, event("github-acme", "old3"))
		appendOne(t, l, event("manual", "m-old"))
		clk.Set(t0.Add(48 * time.Hour))
		new1 := appendOne(t, l, event("github-acme", "new1"))
		if _, err := l.Trim(ctx, time.Time{}, []string{"applier"}); !errors.Is(err, contracts.ErrInvalidRequest) {
			t.Fatalf("zero cutoff: got %v, want ErrInvalidRequest", err)
		}
		n, err := trim(t, l, t0.Add(24*time.Hour))
		if err != nil || n != 2 {
			t.Fatalf("Trim = %d, %v, want 2 (old1 and old3; the retained events stay)", n, err)
		}
		if g := ids(read(t, l, "github-acme", 0, 10)); !slices.Equal(g, []string{"github-acme/old2-kept", "github-acme/new1"}) {
			t.Fatalf("after Trim = %v", g)
		}
		ps, _ := l.Partitions(ctx)
		if len(ps) != 2 || ps[0].Partition != "github-acme" || ps[0].Head != new1.Offset || ps[0].Trimmed != old3.Offset || ps[1].Trimmed != 0 {
			t.Fatalf("Partitions = %+v, want github-acme with head %d and trimmed %d, and manual untrimmed", ps, new1.Offset, old3.Offset)
		}
		if next := appendOne(t, l, event("github-acme", "new2")); next.Offset <= new1.Offset {
			t.Fatalf("offset after Trim = %d, want above %d: offsets are never reused", next.Offset, new1.Offset)
		}
		if n, err := trim(t, l, t0.Add(24*time.Hour)); err != nil || n != 0 {
			t.Fatalf("second Trim = %d, %v, want 0", n, err)
		}
	})

	t.Run("A trimmed ID can be appended again", func(t *testing.T) {
		l, clk := fresh(t)
		first := appendOne(t, l, event("github-acme", "d1"))
		clk.Set(t0.Add(time.Hour))
		if n, err := trim(t, l, t0.Add(time.Minute)); err != nil || n != 1 {
			t.Fatalf("Trim = %d, %v, want 1", n, err)
		}
		again := appendOne(t, l, event("github-acme", "d1"))
		if again.Duplicate || again.Offset <= first.Offset {
			t.Fatalf("got %+v, want a new event above offset %d", again, first.Offset)
		}
	})

	t.Run("Release lets Trim remove a retained event and ignores unknown IDs", func(t *testing.T) {
		l, clk := fresh(t)
		keep := event("manual", "merge-1")
		keep.Retain = true
		appendOne(t, l, keep)
		clk.Set(t0.Add(48 * time.Hour))
		cutoff := t0.Add(24 * time.Hour)
		if n, _ := trim(t, l, cutoff); n != 0 {
			t.Fatalf("Trim removed %d retained events", n)
		}
		if err := l.Release(ctx, []string{"manual/merge-1", "manual/never-appended"}); err != nil {
			t.Fatal(err)
		}
		if es := read(t, l, "manual", 0, 10); len(es) != 1 || es[0].Retain {
			t.Fatalf("after Release = %+v, want the event kept with Retain cleared", es)
		}
		if n, err := trim(t, l, cutoff); err != nil || n != 1 {
			t.Fatalf("Trim after Release = %d, %v, want 1", n, err)
		}
		for name, ids := range map[string][]string{"empty ID": {""}, "no IDs over the limit": make([]string, contracts.MaxAppendEvents+1)} {
			if err := l.Release(ctx, ids); !errors.Is(err, contracts.ErrInvalidEvent) {
				t.Errorf("%s: got %v, want ErrInvalidEvent", name, err)
			}
		}
	})

	t.Run("Trim keeps an entry until every required group has committed at or above it", func(t *testing.T) {
		l, clk := fresh(t)
		o1 := appendOne(t, l, event("github-acme", "d1"))
		o2 := appendOne(t, l, event("github-acme", "d2"))
		o3 := appendOne(t, l, event("github-acme", "d3"))
		clk.Set(t0.Add(48 * time.Hour))
		cutoff := t0.Add(24 * time.Hour)
		required := []string{"applier"}
		if n, err := l.Trim(ctx, cutoff, required); err != nil || n != 0 {
			t.Fatalf("Trim with a required group that never committed = %d, %v, want 0", n, err)
		}
		if err := l.Commit(ctx, "applier", "github-acme", o2.Offset); err != nil {
			t.Fatal(err)
		}
		// A reader that is not required and has read nothing holds nothing back.
		if err := l.Commit(ctx, "debug", "github-acme", o1.Offset); err != nil {
			t.Fatal(err)
		}
		n, err := l.Trim(ctx, cutoff, required)
		if err != nil || n != 2 {
			t.Fatalf("Trim = %d, %v, want 2: the entry at the committed offset goes, the one above stays", n, err)
		}
		if g := ids(read(t, l, "github-acme", 0, 10)); !slices.Equal(g, []string{"github-acme/d3"}) {
			t.Fatalf("after Trim = %v, want only d3", g)
		}
		if ps, _ := l.Partitions(ctx); len(ps) != 1 || ps[0].Trimmed != o2.Offset || ps[0].Head != o3.Offset {
			t.Fatalf("Partitions = %+v, want trimmed %d and head %d", ps, o2.Offset, o3.Offset)
		}
		if err := l.Commit(ctx, "applier", "github-acme", o3.Offset); err != nil {
			t.Fatal(err)
		}
		if n, err := l.Trim(ctx, cutoff, required); err != nil || n != 1 {
			t.Fatalf("Trim after the last commit = %d, %v, want 1", n, err)
		}
	})

	t.Run("Trim waits for the slowest of several required groups, partition by partition", func(t *testing.T) {
		l, clk := fresh(t)
		a1 := appendOne(t, l, event("github-acme", "a1"))
		a2 := appendOne(t, l, event("github-acme", "a2"))
		appendOne(t, l, event("github-other", "b1"))
		clk.Set(t0.Add(48 * time.Hour))
		cutoff := t0.Add(24 * time.Hour)
		for group, offset := range map[string]contracts.Offset{"fast": a2.Offset, "slow": a1.Offset} {
			if err := l.Commit(ctx, group, "github-acme", offset); err != nil {
				t.Fatal(err)
			}
		}
		// Both groups are required; neither has committed in github-other,
		// which they have never read, so it keeps everything.
		n, err := l.Trim(ctx, cutoff, []string{"fast", "slow", "slow"})
		if err != nil || n != 1 {
			t.Fatalf("Trim = %d, %v, want 1 (a1 only)", n, err)
		}
		if g := ids(read(t, l, "github-other", 0, 10)); !slices.Equal(g, []string{"github-other/b1"}) {
			t.Fatalf("github-other after Trim = %v, want b1 kept", g)
		}
	})

	t.Run("Trim refuses no groups, a bad group and too many groups", func(t *testing.T) {
		l, _ := fresh(t)
		many := make([]string, contracts.MaxTrimGroups+1)
		for i := range many {
			many[i] = "g"
		}
		for name, groups := range map[string][]string{"none": nil, "empty list": {}, "empty name": {"applier", ""}, "too many": many} {
			if _, err := l.Trim(ctx, t0, groups); !errors.Is(err, contracts.ErrInvalidRequest) {
				t.Errorf("%s: got %v, want ErrInvalidRequest", name, err)
			}
		}
	})

	t.Run("Concurrent appends get distinct increasing offsets, and a reader never sees a lower offset appear late", func(t *testing.T) {
		l, _ := fresh(t)
		const writers, each = 8, 25
		stop := make(chan struct{})
		var readerErr error
		var readerDone sync.WaitGroup
		readerDone.Add(1)
		go func() {
			defer readerDone.Done()
			// Each pass reads the whole log. An offset below the highest
			// one an earlier pass saw must have been in that pass too.
			prev := map[contracts.Offset]bool{}
			var prevMax contracts.Offset
			for {
				es, err := l.Read(ctx, "github-acme", 0, contracts.MaxReadEntries)
				if err != nil {
					readerErr = err
					return
				}
				cur := make(map[contracts.Offset]bool, len(es))
				for _, e := range es {
					if e.Offset <= prevMax && !prev[e.Offset] {
						readerErr = fmt.Errorf("offset %d appeared after offset %d had been read", e.Offset, prevMax)
						return
					}
					cur[e.Offset] = true
				}
				if n := len(es); n > 0 {
					prevMax = es[n-1].Offset
				}
				prev = cur
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
		var wg sync.WaitGroup
		results := make([][]contracts.Appended, writers)
		errs := make([]error, writers)
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range each {
					got, err := l.Append(ctx, []contracts.Event{event("github-acme", fmt.Sprintf("w%d-%d", w, i))})
					if err != nil {
						errs[w] = err
						return
					}
					results[w] = append(results[w], got...)
				}
			}()
		}
		wg.Wait()
		close(stop)
		readerDone.Wait()
		if readerErr != nil {
			t.Fatal(readerErr)
		}
		handed := map[contracts.Offset]bool{}
		for w := range writers {
			if errs[w] != nil {
				t.Fatalf("writer %d: %v", w, errs[w])
			}
			var last contracts.Offset
			for _, a := range results[w] {
				if handed[a.Offset] || a.Duplicate {
					t.Fatalf("offset %d handed out twice, or a duplicate: %+v", a.Offset, a)
				}
				if a.Offset <= last {
					t.Fatalf("writer %d got offset %d after %d", w, a.Offset, last)
				}
				handed[a.Offset] = true
				last = a.Offset
			}
		}
		es := read(t, l, "github-acme", 0, contracts.MaxReadEntries)
		if len(es) != writers*each {
			t.Fatalf("Read returned %d entries, want %d", len(es), writers*each)
		}
		for _, e := range es {
			if !handed[e.Offset] {
				t.Fatalf("offset %d was read but never handed out", e.Offset)
			}
		}
	})

	t.Run("Concurrent appends of one ID store it once", func(t *testing.T) {
		l, _ := fresh(t)
		const callers = 8
		var wg sync.WaitGroup
		results := make([]contracts.Appended, callers)
		errs := make([]error, callers)
		for c := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := l.Append(ctx, []contracts.Event{event("github-acme", "same")})
				if err == nil {
					results[c] = got[0]
				}
				errs[c] = err
			}()
		}
		wg.Wait()
		fresh := 0
		for c := range callers {
			if errs[c] != nil {
				t.Fatal(errs[c])
			}
			if !results[c].Duplicate {
				fresh++
			}
			if results[c].Offset != 1 {
				t.Fatalf("caller %d got offset %d, want 1", c, results[c].Offset)
			}
		}
		if fresh != 1 {
			t.Fatalf("%d callers wrote the event, want exactly 1", fresh)
		}
	})
}

// oversized returns a batch whose data passes MaxAppendBytes with each event
// inside MaxEventBytes.
func oversized() []contracts.Event {
	n := contracts.MaxAppendBytes/contracts.MaxEventBytes + 1
	var out []contracts.Event
	for i := range n {
		out = append(out, contracts.Event{
			ID: fmt.Sprintf("github-acme/big%d", i), Partition: "github-acme", Type: "t",
			Time: time.Unix(1, 0), Data: bytes.Repeat([]byte("x"), contracts.MaxEventBytes),
		})
	}
	return out
}
