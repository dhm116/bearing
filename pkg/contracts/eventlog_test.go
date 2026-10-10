package contracts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validEvent() Event {
	return Event{
		ID: "github-acme/delivery-1", Partition: "github-acme", Type: "dev.bearing.webhook_received.v1",
		Time: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), Data: []byte(`{}`),
	}
}

func TestCheckEventsAcceptsValidEvents(t *testing.T) {
	t.Parallel()
	manual := validEvent()
	manual.ID, manual.Partition, manual.Retain = "manual/0198-aa", "manual", true
	core := validEvent()
	core.ID, core.Partition = "core/scheduler/tick-9", "core/scheduler"
	if err := CheckEvents([]Event{validEvent(), manual, core}); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

func TestCheckEventRejectsInvalidEvents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*Event)
	}{
		{"no partition", func(e *Event) { e.Partition = "" }},
		{"partition with a control character", func(e *Event) { e.Partition = "github\x00acme"; e.ID = "github\x00acme/1" }},
		{"partition not UTF-8", func(e *Event) { e.Partition = "a\xffb"; e.ID = "a\xffb/1" }},
		{"partition too long", func(e *Event) {
			e.Partition = Partition(strings.Repeat("a", MaxNameBytes+1))
			e.ID = string(e.Partition) + "/1"
		}},
		{"no ID", func(e *Event) { e.ID = "" }},
		{"ID too long", func(e *Event) { e.ID = "github-acme/" + strings.Repeat("a", MaxEventIDBytes) }},
		{"ID of another partition", func(e *Event) { e.ID = "gitea-acme/delivery-1" }},
		{"ID with no slash after the partition", func(e *Event) { e.ID = "github-acme-1" }},
		{"nothing after the partition", func(e *Event) { e.ID = "github-acme/" }},
		{"slash in the local ID", func(e *Event) { e.ID = "github-acme/delivery/1" }},
		{"manual event not retained", func(e *Event) { e.Partition, e.ID = "manual", "manual/m1" }},
		{"no type", func(e *Event) { e.Type = "" }},
		{"no time", func(e *Event) { e.Time = time.Time{} }},
		{"no data", func(e *Event) { e.Data = nil }},
		{"too much data", func(e *Event) { e.Data = make([]byte, MaxEventBytes+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEvent()
			tt.change(&e)
			err := CheckEvent(&e)
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("got %v, want ErrInvalidEvent", err)
			}
			if len(err.Error()) > 400 {
				t.Errorf("error is %d bytes, want it to quote at most 64 bytes of the input", len(err.Error()))
			}
		})
	}
}

// TestCheckEventKeepsPartitionsFromSharingAnID covers the sources whose names
// hold a slash: the ID of one must not be the ID of an event in another
// partition, because the log dedupes by ID.
func TestCheckEventKeepsPartitionsFromSharingAnID(t *testing.T) {
	t.Parallel()
	byPartition := func(p Partition, id string) Event {
		e := validEvent()
		e.Partition, e.ID = p, id
		return e
	}
	// Source "a" with delivery "b/x" and source "a/b" with delivery "x" both
	// spell "a/b/x"; so do "core" with "scheduler/t" and "core/scheduler"
	// with "t". Only the split at the last slash is allowed.
	for _, e := range []Event{byPartition("a", "a/b/x"), byPartition("core", "core/scheduler/t")} {
		if err := CheckEvent(&e); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("partition %s ID %s: got %v, want ErrInvalidEvent", e.Partition, e.ID, err)
		}
	}
	for _, e := range []Event{byPartition("a/b", "a/b/x"), byPartition("core/scheduler", "core/scheduler/t")} {
		if err := CheckEvent(&e); err != nil {
			t.Errorf("partition %s ID %s: got %v, want nil", e.Partition, e.ID, err)
		}
	}
}

func TestCheckEventsBoundsTheBatch(t *testing.T) {
	t.Parallel()
	if err := CheckEvents(nil); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("no events: got %v, want ErrInvalidEvent", err)
	}
	many := make([]Event, MaxAppendEvents+1)
	for i := range many {
		many[i] = validEvent()
	}
	if err := CheckEvents(many); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("too many events: got %v, want ErrInvalidEvent", err)
	}
	big := make([]Event, 5)
	for i := range big {
		big[i] = validEvent()
		big[i].Data = make([]byte, MaxEventBytes)
	}
	if err := CheckEvents(big); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("too many bytes: got %v, want ErrInvalidEvent", err)
	}
	bad := []Event{validEvent(), validEvent()}
	bad[1].Type = ""
	if err := CheckEvents(bad); err == nil || !strings.Contains(err.Error(), "event 1") {
		t.Errorf("got %v, want an error naming event 1", err)
	}
}

func TestCheckNamesWrapInvalidRequest(t *testing.T) {
	t.Parallel()
	if err := CheckPartition("manual"); err != nil {
		t.Errorf("CheckPartition(manual) = %v, want nil", err)
	}
	if err := CheckGroup("workers"); err != nil {
		t.Errorf("CheckGroup(workers) = %v, want nil", err)
	}
	for _, err := range []error{CheckPartition(""), CheckGroup(""), CheckGroup("a\nb")} {
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("got %v, want ErrInvalidRequest", err)
		}
	}
	if err := CheckEventID(""); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("CheckEventID(\"\") = %v, want ErrInvalidEvent", err)
	}
}

func TestCheckRequestsRefuseOutOfRangeArguments(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"read zero limit":      CheckRead("p", 0, 0),
		"read too large":       CheckRead("p", 0, MaxReadEntries+1),
		"read negative offset": CheckRead("p", -1, 1),
		"read no partition":    CheckRead("", 0, 1),
		"commit no group":      CheckCommit("", "p", 1),
		"commit no partition":  CheckCommit("g", "", 1),
		"commit negative":      CheckCommit("g", "p", -1),
		"trim zero":            CheckTrim(time.Time{}),
	} {
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: got %v, want ErrInvalidRequest", name, err)
		}
	}
	for name, err := range map[string]error{
		"read":    CheckRead("p", 0, MaxReadEntries),
		"commit":  CheckCommit("g", "p", 0),
		"trim":    CheckTrim(time.Unix(1, 0)),
		"release": CheckRelease(nil),
	} {
		if err != nil {
			t.Errorf("%s: got %v, want nil", name, err)
		}
	}
	if err := CheckRelease([]string{"a/b", ""}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("release with an empty ID: got %v, want ErrInvalidEvent", err)
	}
	if err := CheckRelease(make([]string, MaxAppendEvents+1)); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("release with too many IDs: got %v, want ErrInvalidEvent", err)
	}
}
