package pgstore

import (
	"bytes"
	"context"
	"slices"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// logAndGraph is a backend that holds a log and the graph built from it.
type logAndGraph interface {
	dumpable
	contracts.EventLog
}

// observationCount is how many observations the log's ObservationsEmitted
// events carry, and how many events of other types it holds.
func observationCount(ctx context.Context, t testing.TB, log contracts.EventLog) (observations, others int) {
	t.Helper()
	parts, err := log.Partitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		var after contracts.Offset
		for {
			entries, err := log.Read(ctx, p.Partition, after, contracts.MaxReadEntries)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) == 0 {
				break
			}
			for _, e := range entries {
				after = e.Offset
				if e.Type != "dev.bearing.observations_emitted.v1" {
					others++
					continue
				}
				oe := &eventv1alpha1.ObservationsEmitted{}
				if err := model.DecodeJSON(e.Data, oe); err != nil {
					t.Fatal(err)
				}
				observations += len(oe.GetObservations())
			}
		}
	}
	return observations, others
}

// liveBackends are the stores a log is filled and applied in as the server
// would, each with its real clock and ID source.
func liveBackends() map[string]func(testing.TB) logAndGraph {
	return map[string]func(testing.TB) logAndGraph{
		"memory":     func(testing.TB) logAndGraph { return memstore.New() },
		"postgresql": func(t testing.TB) logAndGraph { return newTestStore(t) },
	}
}

// replayBackends are the empty stores a log is replayed into. Each takes a
// fixed clock and an ID source that mints the same IDs for the same record
// times, so a replay does not depend on when it runs.
func replayBackends() map[string]func(testing.TB) dumpable {
	return map[string]func(testing.TB) dumpable{
		"memory": func(testing.TB) dumpable {
			s := memstore.New()
			s.Now, s.IDs = fixedClock(), testkit.NewUUIDv7s(fixedClock())
			return s
		},
		"postgresql": func(t testing.TB) dumpable {
			s := newTestStore(t)
			s.Now, s.IDs = fixedClock(), testkit.NewUUIDv7s(fixedClock())
			return s
		},
	}
}

// A graph built live from the log, and one built by replaying the log into an
// empty store, are the same graph apart from what the clock and the ID source
// decided. The replay reads ObservationsEmitted events only (the log also
// holds the requests and deliveries they answer, and the replay is given a
// log without them), applies them in its own
// store and so writes an audit chain of its own, and leaves the live chain
// alone.
func TestReplayingTheLogIntoAnEmptyStoreGivesTheSameGraph(t *testing.T) {
	t.Parallel()
	events := storyEvents(t)
	for liveName, openLive := range liveBackends() {
		t.Run("live "+liveName, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			live := openLive(t)
			if _, err := live.Append(ctx, events); err != nil {
				t.Fatal(err)
			}
			lw := &worker{log: live, store: live, resolver: storyResolver(t, live), group: storyGroup}
			if _, err := lw.drain(ctx); err != nil {
				t.Fatal(err)
			}
			want := canonicalDump(ctx, t, live, dumpCanonical)
			liveReport := verifyAuditChain(ctx, t, live)
			liveHead, err := live.AuditLog().Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if want.AuditRecords == 0 {
				t.Fatal("the story wrote no audit records, so comparing chains would prove nothing")
			}
			observations, others := observationCount(ctx, t, live)
			if others == 0 {
				t.Fatal("the log holds only observations, so reading them alone proves nothing")
			}
			if len(want.Journal) != observations {
				t.Fatalf("the live run applied %d events for %d observations", len(want.Journal), observations)
			}

			// The replay's log holds nothing but the observations, so it cannot
			// lean on any other event. It has the same partitions, in the same
			// order, as the live log.
			observationsOnly := memstore.New()
			if _, err := observationsOnly.Append(ctx, slices.DeleteFunc(slices.Clone(events), func(e contracts.Event) bool {
				return e.Type != "dev.bearing.observations_emitted.v1"
			})); err != nil {
				t.Fatal(err)
			}
			for replayName, openReplay := range replayBackends() {
				t.Run("replay into "+replayName, func(t *testing.T) {
					t.Parallel()
					replay := openReplay(t)
					rw := &worker{log: observationsOnly, store: replay, resolver: storyResolver(t, replay)}
					n, err := rw.drain(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if n != observations {
						t.Errorf("replay applied %d observations, want %d", n, observations)
					}

					got := canonicalDump(ctx, t, replay, dumpCanonical)
					if got.String() != want.String() {
						t.Fatalf("the replayed graph differs from the live one:\n%s", firstDifference(want.String(), got.String()))
					}

					// Its own chain: complete from record 1, and not the live one.
					replayReport := verifyAuditChain(ctx, t, replay)
					if replayReport.First != 1 || replayReport.Records != liveReport.Records {
						t.Errorf("replay chain has records %d to %d (%d), want 1 to %d", replayReport.First, replayReport.Last, replayReport.Records, liveReport.Records)
					}
					replayHead, err := replay.AuditLog().Head(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Equal(replayHead.Hash, liveHead.Hash) {
						t.Error("the replay's chain ends in the live chain's hash; it should be a chain of its own")
					}
					after, err := live.AuditLog().Head(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if after.Seq != liveHead.Seq || !bytes.Equal(after.Hash, liveHead.Hash) {
						t.Errorf("the live chain moved during the replay: head %d, was %d", after.Seq, liveHead.Seq)
					}
					if again := canonicalDump(ctx, t, live, dumpCanonical); again.String() != want.String() {
						t.Errorf("the live store changed during the replay:\n%s", firstDifference(want.String(), again.String()))
					}
				})
			}
		})
	}
}
