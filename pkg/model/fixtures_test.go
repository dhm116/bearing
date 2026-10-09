package model

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "bearing.example/gen/go/bearing/config/v1alpha1" // registers the config resources
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
)

// fixtureMessage returns an empty message for a fixture named
// "<package>.<Message>.json", for example "event.AuditRecord.json".
func fixtureMessage(t *testing.T, file string) proto.Message {
	t.Helper()
	parts := strings.Split(filepath.Base(file), ".")
	if len(parts) != 3 {
		t.Fatalf("fixture %s: want <package>.<Message>.json", file)
	}
	name := protoreflect.FullName("bearing." + parts[0] + ".v1alpha1." + parts[1])
	mt, err := protoregistry.GlobalTypes.FindMessageByName(name)
	if err != nil {
		t.Fatalf("fixture %s: %v", file, err)
	}
	return mt.New().Interface()
}

// TestExampleEventsAndConfigRoundTrip checks that the examples under
// testdata/events and testdata/config are what the generated types read and
// write: unknown fields and numeric enums fail to decode or re-encode
// differently.
func TestExampleEventsAndConfigRoundTrip(t *testing.T) {
	for _, glob := range []string{"../../testdata/events/*.json", "../../testdata/config/*.json"} {
		files, err := filepath.Glob(glob)
		if err != nil || len(files) == 0 {
			t.Fatalf("got %d fixtures for %s (%v), want some", len(files), glob, err)
		}
		for _, file := range files {
			t.Run(filepath.Base(file), func(t *testing.T) {
				raw := readFixture(t, file)
				m := fixtureMessage(t, file)
				if err := DecodeJSON(raw, m); err != nil {
					t.Fatal(err)
				}
				enc, err := EncodeJSON(m)
				if err != nil {
					t.Fatal(err)
				}
				var got, want any
				if err := json.Unmarshal(enc, &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("re-encoded %s\nwant        %s", enc, raw)
				}
			})
		}
	}
}

func TestExampleDeclarationChangedDeclaresValidly(t *testing.T) {
	m := &eventv1alpha1.DeclarationChanged{}
	if err := DecodeJSON(readFixture(t, "../../testdata/events/event.DeclarationChanged.json"), m); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDeclaration(m.GetDeclaration()); err != nil {
		t.Fatal(err)
	}
}
