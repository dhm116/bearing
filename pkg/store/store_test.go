package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

func TestOpenMemoryServesTheContracts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Config{Graph: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if _, err := s.Graph.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "e"}); err != nil {
		t.Fatal(err)
	}
	applyAudited(t, s, "audited")
	got, err := s.Events.Append(ctx, []contracts.Event{{ID: "github-acme/d1", Partition: "github-acme", Type: "dev.bearing.webhook_received.v1", Time: time.Unix(1, 0), Data: []byte("{}")}})
	if err != nil || len(got) != 1 || got[0].Offset != 1 {
		t.Fatalf("Append through the store = %+v, %v, want offset 1", got, err)
	}
}

// applyAudited applies an event with one audit entry through the store's
// graph store and checks the record shows in its audit log.
func applyAudited(t *testing.T, s *Store, event string) {
	t.Helper()
	ctx := context.Background()
	entry := &modelv1alpha1.AuditEntry{
		Action: modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN,
		Actor:  &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "core/resolver"},
		Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, Id: "github:repo/acme/a"},
	}
	head, err := s.Graph.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cs := &modelv1alpha1.ChangeSet{EventId: event, Audit: []*modelv1alpha1.AuditEntry{entry}}
	if !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	if _, err := s.Graph.Apply(ctx, cs); err != nil {
		t.Fatal(err)
	}
	recs, err := s.Audit.Query(ctx, contracts.AuditFilter{Limit: 10})
	if err != nil || len(recs) != 1 || recs[0].GetEventId() != event {
		t.Fatalf("got %v, %v, want the one record of event %s", recs, err, event)
	}
	if h, err := s.Audit.Head(ctx); err != nil || h.Seq != 1 {
		t.Fatalf("got head %+v, %v, want record 1", h, err)
	}
}

func TestOpenTakesAnEventLogURL(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Config{Graph: "mem://", Events: "mem://?x=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if s.Events == nil {
		t.Fatal("no event log")
	}
	if _, err := Open(ctx, Config{Graph: "mem://", Events: "neo4j://localhost"}); err == nil {
		t.Fatal("an unsupported event log URL opened")
	}
}

func TestOpenRequiresAGraphURL(t *testing.T) {
	_, err := Open(context.Background(), Config{Events: "mem://"})
	if err == nil || !strings.Contains(err.Error(), "a graph store URL is required") {
		t.Fatalf("got %v, want an error asking for a graph store URL", err)
	}
}

func TestOpenRejectsUnknownSchemes(t *testing.T) {
	_, err := Open(context.Background(), Config{Graph: "neo4j://localhost"})
	if err == nil || !strings.Contains(err.Error(), "unsupported URL scheme") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenDoesNotLeakURLsItCannotParse(t *testing.T) {
	_, err := Open(context.Background(), Config{Graph: "mem://root:hunter2@localhost:80%zz/bearing"})
	if err == nil || !strings.Contains(err.Error(), "store: parse URL") {
		t.Fatalf("got %v, want a parse error", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q repeats the password", err)
	}
}

func TestOpenRejectsMalformedServerURLs(t *testing.T) {
	for _, tc := range []struct {
		name, url, want string
	}{
		{"opaque mem", "mem:x", "no // after the scheme"},
		// Redacted hides nothing in an opaque URL, so these would leak.
		{"opaque mem with password", "mem:root:hunter2@x", "no // after the scheme"},
		{"postgres", "postgres://root@db/bearing", "unsupported URL scheme"},
		{"SurrealDB URL", "surrealdb+ws://root@db:8000", "unsupported URL scheme"},
		{"embedded SurrealDB URL", "surrealkv:///var/lib/bearing", "unsupported URL scheme"},
		{"unknown scheme with secrets", "neo4j://root@db/hunter2?pass=hunter2", "unsupported URL scheme"},
		{"another unknown scheme", "qdrant://root@db/hunter2?key=hunter2", "unsupported URL scheme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// As the event log, so the graph URL is not what fails.
			cfg := Config{Graph: "mem://", Events: tc.url}
			s, err := Open(context.Background(), cfg)
			if err == nil {
				closeStore(t, s)
				t.Fatalf("opened %s", tc.url)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q repeats a secret from the URL", err)
			}
		})
	}
}

// closeStore closes s and fails the test if that fails.
func closeStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("close store: %v", err)
	}
}
