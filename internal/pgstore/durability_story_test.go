package pgstore

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"bearing.example/adapters/github"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
)

// The durability tests (#140) feed a store the inputs of the fictional org the
// way the core will: every input is an event on the log, and a worker turns
// the log's ObservationsEmitted events into applies. bearing server (#139)
// does not exist yet, so this file holds what the tests need of it: the
// inputs as log events, the resolver's configuration and the worker loop.

const (
	storyGitHub    = "github-acme"
	storyDirectory = "authentik-acme"
	// storyGroup is the consumer group of the worker.
	storyGroup = "apply"

	storyToken  = "test-token"
	storySecret = "test-secret"
)

// observationsEvent is the log event for one page of adapter output. Its ID
// follows docs/spec/contracts.md, "Event IDs": the source, a slash and the
// hex SHA-256 of the causing event's ID, a NUL and the page number, so a
// repeated run repeats its IDs.
func observationsEvent(t testing.TB, source, trigger string, page int, at time.Time, obs adapter.Observations, done bool, complete *modelv1alpha1.CompleteSync) contracts.Event {
	t.Helper()
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d", trigger, page))
	data, err := model.EncodeJSON(&eventv1alpha1.ObservationsEmitted{
		Source: source, TriggerEventId: trigger, Observations: obs, Done: done, CompleteSync: complete,
	})
	if err != nil {
		t.Fatal(err)
	}
	return contracts.Event{
		ID: source + "/" + hex.EncodeToString(sum[:]), Partition: contracts.Partition(source),
		Type: "dev.bearing.observations_emitted.v1", Time: at.UTC().Truncate(time.Microsecond), Data: data,
	}
}

// triggerEvent is the log event that an ObservationsEmitted event answers: a
// request to sync a source or a webhook delivery. The worker applies neither.
func triggerEvent(t testing.TB, id, source string, at time.Time, msg proto.Message) contracts.Event {
	t.Helper()
	typ, ok := model.EventType(msg)
	if !ok {
		t.Fatalf("%T is not an event", msg)
	}
	data, err := model.EncodeJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	return contracts.Event{ID: id, Partition: contracts.Partition(source), Type: typ, Time: at.UTC().Truncate(time.Microsecond), Data: data}
}

// storyEvents plays the fictional org's story (internal/fakes.Story) and
// returns what the core would have put on the log, in the order it arrived:
// a full GitHub sync, the webhook deliveries of each change, the directory
// feed, and a second full sync after the last change.
func storyEvents(t testing.TB) []contracts.Event {
	t.Helper()
	ctx := context.Background()
	clk := testkit.NewClock(fakes.Start)
	org := fakes.NewOrg(clk)
	srv := fakes.NewGitHub(t, org, fakes.GitHubOptions{Token: storyToken, WebhookSecret: storySecret})
	cfg, err := json.Marshal(map[string]any{"org": "acme", "api_url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GITHUB_TOKEN": storyToken, "GITHUB_WEBHOOK_SECRET": storySecret}
	a := &github.Adapter{HTTP: srv.Client(), Now: clk.Now, Getenv: func(k string) string { return env[k] }}

	var events []contracts.Event
	syncs, delivered := 0, 0
	at := func(when time.Time) {
		if when.After(clk.Now()) {
			clk.Set(when)
		}
	}
	syncGitHub := func() {
		syncs++
		trigger := fmt.Sprintf("%s/sync-%d", storyGitHub, syncs)
		events = append(events, triggerEvent(t, trigger, storyGitHub, clk.Now(), &eventv1alpha1.SyncRequested{
			Source: storyGitHub, Actor: &eventv1alpha1.Actor{Subject: "system:scheduler"},
		}))
		cursor := ""
		for page := 1; ; page++ {
			res, err := a.Sync(ctx, adapter.SyncParams{Config: cfg, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, observationsEvent(t, storyGitHub, trigger, page, clk.Now(), res.Observations, res.Done, res.CompleteSync))
			if res.Done {
				return
			}
			cursor = res.NextCursor
			clk.Advance(time.Millisecond)
		}
	}
	deliver := func() {
		ds := srv.Deliveries()
		for _, d := range ds[delivered:] {
			res, err := a.Handle(ctx, adapter.HandleParams{Config: cfg, Headers: d.Header(), Body: d.Body})
			if err != nil {
				t.Fatalf("%s delivery: %v", d.Event, err)
			}
			trigger := storyGitHub + "/delivery-" + d.ID
			var headers []*eventv1alpha1.Header
			for name, values := range d.Header() {
				for _, v := range values {
					headers = append(headers, &eventv1alpha1.Header{Name: name, Value: v})
				}
			}
			slices.SortFunc(headers, func(a, b *eventv1alpha1.Header) int { return strings.Compare(a.GetName(), b.GetName()) })
			events = append(events, triggerEvent(t, trigger, storyGitHub, clk.Now(), &eventv1alpha1.WebhookReceived{
				Source: storyGitHub, Headers: headers, Body: d.Body, ReceivedAt: timestamppb.New(clk.Now()),
			}))
			events = append(events, observationsEvent(t, storyGitHub, trigger, 1, clk.Now(), res.Observations, true, nil))
			clk.Advance(time.Millisecond)
		}
		delivered = len(ds)
	}
	directory := func() {
		f, err := os.Open(repoPath("testdata/acme/directory.ndjson"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		var obs adapter.Observations
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			o, err := model.DecodeObservation(sc.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			obs = append(obs, o)
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		trigger := storyDirectory + "/sync-1"
		events = append(events, triggerEvent(t, trigger, storyDirectory, clk.Now(), &eventv1alpha1.SyncRequested{
			Source: storyDirectory, Actor: &eventv1alpha1.Actor{Subject: "system:scheduler"},
		}))
		events = append(events, observationsEvent(t, storyDirectory, trigger, 1, clk.Now(), obs, true, nil))
	}

	syncGitHub()
	directoryDone := false
	for _, step := range fakes.Story() {
		if !directoryDone && step.At.After(fakes.DirectorySyncAt) {
			directoryDone = true
			at(fakes.DirectorySyncAt)
			directory()
		}
		at(step.At)
		if step.Apply != nil {
			if err := step.Apply(org); err != nil {
				t.Fatal(err)
			}
			deliver()
		}
	}
	at(fakes.MembershipEndsAt.Add(time.Hour))
	syncGitHub()
	return events
}

// repoPath is a path in the repository, which the tests find from their own
// directory (internal/pgstore, and the same in a child process).
func repoPath(rel string) string { return filepath.Join("..", "..", filepath.FromSlash(rel)) }

// storyResolver is the resolver the story's two sources need, on store.
func storyResolver(t testing.TB, store contracts.GraphStore) *resolver.Resolver {
	t.Helper()
	decl := func(name string) *modelv1alpha1.AdapterDeclaration {
		b, err := os.ReadFile(repoPath("testdata/declarations/" + name + ".json"))
		if err != nil {
			t.Fatal(err)
		}
		d := &modelv1alpha1.AdapterDeclaration{}
		if err := model.DecodeJSON(b, d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	r, err := resolver.New(resolver.Config{
		Declarations: []*modelv1alpha1.AdapterDeclaration{decl("github"), decl("authentik")},
		Sources: map[string]*resolver.Source{
			storyGitHub: {Name: storyGitHub, Adapter: "github"},
			storyDirectory: {
				Name: storyDirectory, Adapter: "authentik",
				Issues: []resolver.Namespace{{Name: "authentik-saml", IssuerType: "saml"}},
				Links:  []resolver.Namespace{{Name: "github", IssuerType: "github"}},
			},
		},
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// worker is the stand-in for the server's apply loop: it reads the log's
// partitions in name order, applies the observations of each
// ObservationsEmitted event through the resolver and only then commits the
// entry's offset, so a crash redelivers the entry and Apply's processed-event
// mark makes the repeat harmless (docs/adr/0007-durable-event-log.md).
type worker struct {
	log      contracts.EventLog
	store    contracts.GraphStore
	resolver *resolver.Resolver
	// group is the consumer group whose offsets the worker keeps. Empty
	// means a replay: nothing is committed and every partition is read from
	// the start.
	group string
}

// drain processes everything on the log and returns the number of
// observations it applied (duplicates included).
func (w *worker) drain(ctx context.Context) (int, error) {
	parts, err := w.log.Partitions(ctx)
	if err != nil {
		return 0, fmt.Errorf("partitions: %w", err)
	}
	n := 0
	for _, p := range parts {
		var after contracts.Offset
		if w.group != "" {
			if after, err = w.log.Committed(ctx, w.group, p.Partition); err != nil {
				return n, fmt.Errorf("committed %s: %w", p.Partition, err)
			}
		}
		for {
			entries, err := w.log.Read(ctx, p.Partition, after, 100)
			if err != nil {
				return n, fmt.Errorf("read %s after %d: %w", p.Partition, after, err)
			}
			if len(entries) == 0 {
				break
			}
			for _, e := range entries {
				k, err := w.apply(ctx, e)
				n += k
				if err != nil {
					return n, err
				}
				if w.group != "" {
					if err := w.log.Commit(ctx, w.group, p.Partition, e.Offset); err != nil {
						return n, fmt.Errorf("commit %s at %d: %w", p.Partition, e.Offset, err)
					}
				}
				after = e.Offset
			}
		}
	}
	return n, nil
}

// apply applies the observations of an ObservationsEmitted entry, each as an
// event of its own named for the log event and its place in the page. Any
// other event is ignored: the stand-in applies nothing else.
func (w *worker) apply(ctx context.Context, e contracts.Entry) (int, error) {
	if e.Type != "dev.bearing.observations_emitted.v1" {
		return 0, nil
	}
	oe := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(e.Data, oe); err != nil {
		return 0, fmt.Errorf("event %s: %w", e.ID, err)
	}
	for i, o := range oe.GetObservations() {
		ev := resolver.Event{
			ID: fmt.Sprintf("%s/%d", e.ID, i+1), Source: oe.GetSource(), Observation: o, IngestedAt: e.Time,
		}
		if err := w.applyObservation(ctx, ev); err != nil {
			return i, fmt.Errorf("event %s: %w", ev.ID, err)
		}
	}
	return len(oe.GetObservations()), nil
}

// maxStaleRetries is how often the worker resolves an observation again
// after another apply landed first.
const maxStaleRetries = 16

// applyObservation resolves ev and applies its ChangeSet with the audit
// entries the resolver's output calls for. Nothing in the repository turns
// that output into entries yet (the server will), so the worker does it for
// the decisions the story makes: mints, bindings, merges and rejections.
func (w *worker) applyObservation(ctx context.Context, ev resolver.Event) error {
	for range maxStaleRetries {
		res, err := w.resolver.Resolve(ctx, ev)
		if err != nil {
			return err
		}
		cs := res.ChangeSet
		cs.Audit = append(cs.Audit, auditEntries(ev.ID, cs, res.Rejections)...)
		if _, err := w.store.Apply(ctx, cs); errors.Is(err, contracts.ErrStale) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return contracts.ErrStale
}

func auditEntries(eventID string, cs *modelv1alpha1.ChangeSet, rejections []resolver.Rejection) []*modelv1alpha1.AuditEntry {
	system := &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "system:resolver"}
	var out []*modelv1alpha1.AuditEntry
	for _, m := range cs.GetMints() {
		out = append(out, &modelv1alpha1.AuditEntry{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, Actor: system, Rule: model.ShortName(m.GetRule()),
			Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, Id: m.GetRef()},
		})
	}
	for _, b := range cs.GetBindings() {
		out = append(out, &modelv1alpha1.AuditEntry{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, Actor: system,
			Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, Id: b.GetAlias()},
		})
	}
	for _, m := range cs.GetMerges() {
		out = append(out, &modelv1alpha1.AuditEntry{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, Actor: system, Rule: model.ShortName(m.GetRule()), ConfidencePpm: m.GetConfidencePpm(),
			Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, Id: m.GetSubjectIds()[0]},
		})
	}
	for _, r := range rejections {
		out = append(out, &modelv1alpha1.AuditEntry{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_REJECTION, Actor: system, RejectionCode: r.Code, Reason: model.Clip(r.String()),
			Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_EVENT, Id: eventID},
		})
	}
	return out
}

// fixedClock reads the same instant every time. A store's record time is
// then the head plus a microsecond, and its IDs depend on that and nothing
// else, so the same inputs give the same bytes.
func fixedClock() func() time.Time {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
}
