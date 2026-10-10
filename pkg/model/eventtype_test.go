package model

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestEventTypeNamesEveryEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		msg  proto.Message
		want string
	}{
		{&eventv1alpha1.Observation{}, ObservationType},
		{&eventv1alpha1.SyncRequested{}, "dev.bearing.sync_requested.v1"},
		{&eventv1alpha1.WebhookReceived{}, "dev.bearing.webhook_received.v1"},
		{&eventv1alpha1.ObservationsEmitted{}, "dev.bearing.observations_emitted.v1"},
		{&eventv1alpha1.ValidTimeBoundaryReached{}, "dev.bearing.valid_time_boundary_reached.v1"},
		{&eventv1alpha1.ConfigApplied{}, "dev.bearing.config_applied.v1"},
	}
	for _, tt := range tests {
		got, ok := EventType(tt.msg)
		if !ok || got != tt.want {
			t.Errorf("EventType(%T) = %q, %v, want %q, true", tt.msg, got, ok, tt.want)
		}
	}
}

func TestEventTypeRejectsNonEvents(t *testing.T) {
	t.Parallel()
	for _, m := range []proto.Message{&eventv1alpha1.Actor{}, &eventv1alpha1.Header{}, &eventv1alpha1.ConfigChange{}, &modelv1alpha1.Entity{}} {
		if got, ok := EventType(m); ok {
			t.Errorf("EventType(%T) = %q, true, want false", m, got)
		}
	}
}

// TestEveryEventMessageIsClassified fails when event.proto gains a message
// that is neither an event nor one of the parts listed here, so a new event
// can't reach the log without a CloudEvents type.
func TestEveryEventMessageIsClassified(t *testing.T) {
	t.Parallel()
	parts := []string{"Header", "Actor", "ConfigChange"}
	msgs := eventv1alpha1.File_bearing_event_v1alpha1_event_proto.Messages()
	seen := map[string]bool{}
	for i := range msgs.Len() {
		name := string(msgs.Get(i).Name())
		seen[name] = true
		inEvents := slices.Contains(eventMessages, msgs.Get(i).Name())
		if inEvents == slices.Contains(parts, name) {
			t.Errorf("message %s must be in exactly one of the event messages and the parts of events", name)
		}
	}
	for _, n := range eventMessages {
		if !seen[string(n)] {
			t.Errorf("event message %s is not in event.proto", n)
		}
	}
}
