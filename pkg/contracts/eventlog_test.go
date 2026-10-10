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
