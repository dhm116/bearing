package model

import (
	"slices"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
)

// eventMessages are the messages of proto/bearing/event/v1alpha1 that travel
// on the event log as the data of a CloudEvent (docs/spec/contracts.md,
// "EventLog"); the rest of the file is parts of them. A new event message is
// added here, and a test fails until it is.
var eventMessages = []protoreflect.Name{
	"Observation", "SyncRequested", "WebhookReceived", "ObservationsEmitted",
	"MergeRequested", "UnmergeRequested", "DistinctFromSet", "DistinctFromCleared",
	"ClaimWithdrawn", "OverrideSet", "OverrideCleared",
	"ValidTimeBoundaryReached", "SubjectDeletionDerived",
	"ConflictOpened", "ConflictResolved", "OverrideStale",
	"CompactionRequested", "DeclarationChanged", "ConfigApplied",
}

// EventType returns the CloudEvents type of an event message:
// "dev.bearing.<name in snake case>.v1", for example
// "dev.bearing.sync_requested.v1". It reports false for a message that is
// not an event, such as Actor or Header. The version stays v1 while the spec
// is a pre-1.0 draft, as for ObservationType.
func EventType(m proto.Message) (string, bool) {
	d := m.ProtoReflect().Descriptor()
	if d.ParentFile().Package() != eventv1alpha1.File_bearing_event_v1alpha1_event_proto.Package() || !slices.Contains(eventMessages, d.Name()) {
		return "", false
	}
	return "dev.bearing." + snakeCase(string(d.Name())) + ".v1", true
}

// snakeCase turns "SyncRequested" into "sync_requested".
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
